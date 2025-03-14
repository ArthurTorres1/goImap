package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"gorm.io/driver/sqlserver"
	"gorm.io/gorm"
)

type Config struct {
	Servidor string `json:"servidor"`
	Email    string `json:"email"`
	Senha    string `json:"senha"`
	Porta    int    `json:"porta"`
	IsSSL    bool   `json:"isSSL"`
	Database struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
		DbName   string `json:"dbname"`
	} `json:"database"`
}

type EmailResponse struct {
	Titulo   string   `json:"subject"`
	Data     string   `json:"date"`
	Mensagem string   `json:"message"`
	Arquivos []string `json:"attachments"`
}

type EmailRecebido struct {
	Codigo   int64  `gorm:"primaryKey;autoIncrement:true"`
	Data     string `gorm:"type:datetime"`
	Titulo   string `gorm:"type:varchar(255)"`
	Mensagem string `gorm:"type:varchar(255)"`
}

func (EmailRecebido) TableName() string {
	return "tabEmailRecebidos" // Nome correto da tabela no banco
}

type EmailAnexo struct {
	Codigo              int64  `gorm:"primaryKey;autoIncrement:true;column:codigo"`
	EmailRecebidoCodigo int64  `gorm:"not null;column:emailRecebidosCodigo"`
	NomeArquivo         string `gorm:"type:varchar(255);column:nomeArquivo"`
	IsLido              bool   `gorm:"type:bit;column:isLido"`
}

func (EmailAnexo) TableName() string {
	return "tabEmailRecebidosAnexos" // Nome da tabela que você já tem no banco
}

var db *gorm.DB

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

	// Conectar ao banco de dados SQL Server usando GORM
	dsn := fmt.Sprintf("sqlserver://%s:%s@%s:%d?database=%s", config.Database.User, config.Database.Password, config.Database.Host, config.Database.Port, config.Database.DbName)
	db, err = gorm.Open(sqlserver.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("Erro ao conectar ao banco de dados: %v", err)
	}

	fmt.Println("Conexão com o banco de dados estabelecida com sucesso.")

	// Processar os e-mails
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

		// Salvar email na tabela tabEmailRecebidos
		email := EmailRecebido{
			Data:     msg.Envelope.Date.Format("2006-01-02 15:04:05"),
			Titulo:   emailResponse.Titulo,
			Mensagem: emailResponse.Mensagem,
		}
		result := db.Create(&email)
		if result.Error != nil {
			log.Printf("Erro ao salvar e-mail na tabela tabEmailRecebidos: %v", result.Error)
		} else {
			fmt.Printf("E-mail salvo na tabela tabEmailRecebidos: %v\n", email.Titulo)
		}

		// Salvar anexos na tabela tabEmailRecebidosAnexos
		for _, anexo := range emailResponse.Arquivos {
			emailAnexo := EmailAnexo{
				EmailRecebidoCodigo: email.Codigo, // O código é gerado automaticamente
				NomeArquivo:         anexo,
				IsLido:              false, // Definir como não lido inicialmente
			}
			result := db.Create(&emailAnexo)
			if result.Error != nil {
				log.Printf("Erro ao salvar anexo na tabela tabEmailRecebidosAnexos: %v", result.Error)
			} else {
				fmt.Printf("Anexo salvo na tabela tabEmailRecebidosAnexos: %v\n", anexo)
			}
		}

		// Marcar e-mail como lido
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
				// Remover quebras de linha e tags HTML
				mensagem = cleanHTML(string(body))

			case *mail.AttachmentHeader:
				filename, _ := h.Filename()
				anexos = append(anexos, filename)
				saveAttachment(part.Body, filename)
			}
		}
	}

	return EmailResponse{
		Titulo:   msg.Envelope.Subject,
		Data:     msg.Envelope.Date.String(),
		Mensagem: mensagem,
		Arquivos: anexos,
	}
}

func saveAttachment(body io.Reader, filename string) string {
	// Criar diretório downloads se não existir
	downloadDir := "downloads"
	if _, err := os.Stat(downloadDir); os.IsNotExist(err) {
		err := os.Mkdir(downloadDir, 0755)
		if err != nil {
			log.Printf("Erro ao criar diretório para anexos: %v", err)
			return ""
		}
	}

	// Salvar anexo no diretório
	filePath := filepath.Join(downloadDir, filename)
	outFile, err := os.Create(filePath)
	if err != nil {
		log.Printf("Erro ao salvar anexo %s: %v", filename, err)
		return ""
	}
	defer outFile.Close()

	_, err = io.Copy(outFile, body)
	if err != nil {
		log.Printf("Erro ao copiar conteúdo do anexo %s: %v", filename, err)
		return ""
	}

	return filePath
}

func cleanHTML(input string) string {
	// Limpar HTML (pode ser ajustado conforme necessário)
	re := regexp.MustCompile(`<.*?>`)
	return re.ReplaceAllString(input, "")
}
