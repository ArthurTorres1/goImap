package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"

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
	Titulo    string   `json:"subject"`
	Data      string   `json:"date"`
	Mensagem  string   `json:"message"`
	Arquivos  []string `json:"attachments"`
	LocalPath []string `json:"downloaded_files"`
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

	processFolder(c, "INBOX")
	processFolder(c, "[Gmail]/Spam")
}

func processFolder(c *client.Client, folder string) {
	_, err := c.Select(folder, false)
	if err != nil {
		log.Printf("Erro ao selecionar %s: %v", folder, err)
		return
	}

	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{imap.SeenFlag}
	ids, err := c.Search(criteria)
	if err != nil {
		log.Printf("Erro ao buscar e-mails em %s: %v", folder, err)
		return
	}

	if len(ids) == 0 {
		fmt.Printf("Nenhum e-mail não lido encontrado em %s.\n", folder)
		return
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(ids...)

	messages := make(chan *imap.Message, len(ids))
	go func() {
		err := c.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope, imap.FetchRFC822}, messages)
		if err != nil {
			log.Printf("Erro ao buscar mensagens em %s: %v", folder, err)
		}
	}()

	for msg := range messages {
		emailResponse := processMessage(msg)

		// Convertendo para JSON
		emailJson, err := json.Marshal(emailResponse)
		if err != nil {
			log.Printf("Erro ao gerar JSON: %v", err)
			continue
		}

		fmt.Println(string(emailJson))

		// Marcar como lido
		markSeqSet := new(imap.SeqSet)
		markSeqSet.AddNum(msg.SeqNum)
		flags := []interface{}{imap.SeenFlag}
		if err := c.Store(markSeqSet, imap.AddFlags, flags, nil); err != nil {
			log.Printf("Erro ao marcar e-mail como lido: %v", err)
		} else {
			fmt.Printf("E-mail %d de %s marcado como lido.\n", msg.SeqNum, folder)
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
	var localPaths []string

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
				body, _ := io.ReadAll(part.Body)
				mensagem = string(body)

			case *mail.AttachmentHeader:
				filename, _ := h.Filename()
				anexos = append(anexos, filename)

				// Salvar arquivo
				filePath := saveAttachment(part.Body, filename)
				if filePath != "" {
					localPaths = append(localPaths, filePath)
				}
			}
		}
	}

	return EmailResponse{
		Titulo:    msg.Envelope.Subject,
		Data:      msg.Envelope.Date.String(),
		Mensagem:  mensagem,
		Arquivos:  anexos,
		LocalPath: localPaths,
	}
}

func saveAttachment(body io.Reader, filename string) string {
	// Criar diretório downloads se não existir
	downloadDir := "downloads"
	if _, err := os.Stat(downloadDir); os.IsNotExist(err) {
		os.Mkdir(downloadDir, 0755)
	}

	// Caminho completo do arquivo
	filePath := filepath.Join(downloadDir, filename)
	outFile, err := os.Create(filePath)
	if err != nil {
		log.Printf("Erro ao criar arquivo %s: %v", filePath, err)
		return ""
	}
	defer outFile.Close()

	_, err = io.Copy(outFile, body)
	if err != nil {
		log.Printf("Erro ao salvar anexo %s: %v", filePath, err)
		return ""
	}

	fmt.Printf("Anexo salvo: %s\n", filePath)
	return filePath
}
