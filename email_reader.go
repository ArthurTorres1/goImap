package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/quotedprintable"
	"os"
	"strings"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
	"golang.org/x/net/html"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/transform"
)

type Config struct {
	Servidor string `json:"servidor"`
	Email    string `json:"email"`
	Senha    string `json:"senha"`
	Porta    int    `json:"porta"`
	IsSSL    bool   `json:"isSSL"`
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
		processMessage(msg)

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

func processMessage(msg *imap.Message) {
	if msg == nil || msg.Envelope == nil {
		log.Println("Mensagem vazia ou sem envelope.")
		return
	}

	fmt.Println("===================================")
	fmt.Println("Título:", msg.Envelope.Subject)
	fmt.Println("Data:", msg.Envelope.Date)

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
				fmt.Printf("Tipo de conteúdo: %s\n", contentType)
				fmt.Printf("Charset: %s\n", params["charset"])

				body, _ := io.ReadAll(part.Body)

				// Decodifica quoted-printable
				if strings.ToLower(h.Get("Content-Transfer-Encoding")) == "quoted-printable" {
					body, err = io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
					if err != nil {
						log.Printf("Erro ao decodificar quoted-printable: %v", err)
						continue
					}
				}

				body = convertCharset(body, params["charset"])

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

	if mensagem != "" {
		fmt.Println("Mensagem:", mensagem)
	} else {
		fmt.Println("Mensagem: (Nenhum texto encontrado)")
	}

	if len(anexos) > 0 {
		fmt.Println("Anexos encontrados:")
		for _, anexo := range anexos {
			fmt.Println("-", anexo)
		}
	} else {
		fmt.Println("Nenhum anexo encontrado.")
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

func convertCharset(input []byte, charset string) []byte {
	if charset == "" {
		return input
	}

	enc, err := ianaindex.IANA.Encoding(charset)
	if err != nil {
		log.Printf("Charset não suportado: %s", charset)
		return input
	}

	reader := transform.NewReader(bytes.NewReader(input), enc.NewDecoder())
	output, err := io.ReadAll(reader)
	if err != nil {
		log.Printf("Erro ao converter charset %s: %v", charset, err)
		return input
	}

	return output
}
