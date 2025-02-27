package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"mime/quotedprintable"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"golang.org/x/net/html"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
)

type Config struct {
	Servidor string `json:"servidor"`
	Email    string `json:"email"`
	Senha    string `json:"senha"`
	Porta    int    `json:"porta"`
	IsSSL    bool   `json:"isSSL"`
}

type EmailResponse struct {
	Subject   string   `json:"subject"`
	Date      string   `json:"date"`
	Message   string   `json:"message"`
	Attachments []string `json:"attachments"`
}

func init() {
	message.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		var decoder *encoding.Decoder

		switch strings.ToLower(charset) {
		case "iso-8859-1", "latin1":
			decoder = charmap.ISO8859_1.NewDecoder()
		case "windows-1252":
			decoder = charmap.Windows1252.NewDecoder()
		default:
			decoder = charmap.ISO8859_1.NewDecoder()
		}

		return decoder.Reader(input), nil
	}
}

func LoadConfig(filename string) (Config, error) {
	var config Config
	file, err := os.Open(filename)
	if err != nil {
		return config, err
	}
	defer file.Close()

	bytes, err := io.ReadAll(file)
	if err != nil {
		return config, err
	}

	err = json.Unmarshal(bytes, &config)
	return config, err
}

func main() {
	config, err := LoadConfig("config.json")
	if err != nil {
		log.Fatalf("Erro ao carregar configuração: %v", err)
	}

	fmt.Println("Configuração carregada com sucesso:")
	fmt.Printf("Servidor: %s\nEmail: %s\nPorta: %d\nSSL: %v\n", config.Servidor, config.Email, config.Porta, config.IsSSL)

	processEmails(config)
}

func processEmails(cfg Config) {
	c, err := client.DialTLS(fmt.Sprintf("%s:%d", cfg.Servidor, cfg.Porta), nil)
	if err != nil {
		log.Printf("Erro ao conectar ao servidor %s: %v", cfg.Servidor, err)
		return
	}
	defer c.Logout()

	if err := c.Login(cfg.Email, cfg.Senha); err != nil {
		log.Printf("Erro ao autenticar: %v", err)
		return
	}
	fmt.Println("Autenticado com sucesso.")

	_, err = c.Select("INBOX", false)
	if err != nil {
		log.Printf("Erro ao selecionar INBOX: %v", err)
		return
	}

	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{imap.SeenFlag}
	ids, err := c.Search(criteria)
	if err != nil {
		log.Printf("Erro ao buscar e-mails: %v", err)
		return
	}

	if len(ids) == 0 {
		fmt.Println("Nenhum e-mail não lido encontrado.")
		return
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(ids...)

	messages := make(chan *imap.Message, len(ids))

	go func() {
		err := c.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope, imap.FetchRFC822}, messages)
		if err != nil {
			log.Printf("Erro ao buscar mensagens: %v", err)
		}
	}()

	for msg := range messages {
		emailResponse := processMessage(msg)

		// Convert the struct to JSON and print it
		emailJson, err := json.Marshal(emailResponse)
		if err != nil {
			log.Printf("Erro ao gerar JSON: %v", err)
			continue
		}

		// Print the JSON
		fmt.Println(string(emailJson))

		// Mark the message as read
		markSeqSet := new(imap.SeqSet)
		markSeqSet.AddNum(msg.SeqNum)

		flags := []interface{}{imap.SeenFlag}
		if err := c.Store(markSeqSet, imap.AddFlags, flags, nil); err != nil {
			log.Printf("Erro ao marcar e-mail como lido: %v", err)
		} else {
			fmt.Printf("E-mail %d marcado como lido.\n", msg.SeqNum)
		}
	}
}

func processMessage(msg *imap.Message) EmailResponse {
	if msg == nil || msg.Envelope == nil {
		log.Println("Mensagem vazia ou sem envelope.")
		return EmailResponse{}
	}

	var mensagem string
	var anexos []string

	for section, literal := range msg.Body {
		reader, err := mail.CreateReader(literal)
		if err != nil {
			log.Printf("Erro ao criar reader de mensagem (Seção: %v): %v", section, err)
			continue
		}

		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Printf("Erro ao ler parte do e-mail: %v", err)
				continue
			}

			switch h := part.Header.(type) {
			case *mail.InlineHeader:
				contentType, params, _ := h.ContentType()
				transferEncoding := strings.ToLower(h.Header.Get("Content-Transfer-Encoding"))
				charsetParam := strings.ToLower(strings.Trim(params["charset"], ` "`))

				var bodyReader io.Reader = part.Body
				switch transferEncoding {
				case "quoted-printable":
					bodyReader = quotedprintable.NewReader(bodyReader)
				case "base64":
					bodyReader = base64.NewDecoder(base64.StdEncoding, bodyReader)
				}

				body, err := io.ReadAll(bodyReader)
				if err != nil {
					log.Printf("Erro ao ler corpo: %v", err)
					continue
				}

				body = convertCharset(body, charsetParam)

				switch {
				case strings.Contains(contentType, "text/plain"):
					mensagem = strings.TrimSpace(string(body))
				case strings.Contains(contentType, "text/html") && mensagem == "":
					mensagem = extractTextFromHTML(body)
				}

			case *mail.AttachmentHeader:
				filename, _ := h.Filename()
				anexos = append(anexos, filename)
			}
		}
	}

	return EmailResponse{
		Subject:   msg.Envelope.Subject,
		Date:      msg.Envelope.Date.String(),
		Message:   mensagem,
		Attachments: anexos,
	}
}

func extractTextFromHTML(htmlBytes []byte) string {
	doc, err := html.Parse(bytes.NewReader(htmlBytes))
	if err != nil {
		log.Printf("Erro ao analisar HTML: %v", err)
		return ""
	}

	var text string
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			text += n.Data
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(doc)

	return strings.TrimSpace(text)
}

func convertCharset(input []byte, charsetStr string) []byte {
	if charsetStr == "" {
		return input
	}

	charsetStr = strings.ToLower(charsetStr)

	var decoder *encoding.Decoder

	switch charsetStr {
	case "iso-8859-1", "latin1":
		decoder = charmap.ISO8859_1.NewDecoder()
	case "windows-1252":
		decoder = charmap.Windows1252.NewDecoder()
	default:
		decoder = charmap.ISO8859_1.NewDecoder()
	}

	reader := decoder.Reader(bytes.NewReader(input))
	output, err := io.ReadAll(reader)
	if err != nil {
		log.Printf("Erro na conversão de charset (%s): %v", charsetStr, err)
		return input
	}
	return output
}
