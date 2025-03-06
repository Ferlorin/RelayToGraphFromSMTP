package main

import (
	"bytes"
	"fmt"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-smtp"
	"gopkg.in/ini.v1"
	"io"
	"log"
	"os"
	"strings"
	"sync"
)

// --- Config and Logger Setup ---

type Config struct {
	TenantID     string
	ClientID     string
	ClientSecret string
	Scope        string
	Host         string
	Port         string
}

var config Config
var logger *log.Logger

func loadConfig() error {
	cfg, err := ini.Load("config.ini")
	if err != nil {
		return err
	}
	config.TenantID = cfg.Section("MicrosoftGraph").Key("TenantID").String()
	config.ClientID = cfg.Section("MicrosoftGraph").Key("ClientID").String()
	config.ClientSecret = cfg.Section("MicrosoftGraph").Key("ClientSecret").String()
	config.Scope = cfg.Section("MicrosoftGraph").Key("Scope").String()
	config.Host = cfg.Section("Server").Key("Host").String()
	config.Port = cfg.Section("Server").Key("SMTPPort").String()
	return nil
}

func initLogger() error {
	f, err := os.OpenFile("app.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return err
	}
	mw := io.MultiWriter(os.Stdout, f)
	logger = log.New(mw, "", log.LstdFlags)
	return nil
}

// --- SMTP Backend ---
type Backend struct{}

func (bkd *Backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &Session{}, nil
}

// Global email transaction state
var (
	mu          sync.Mutex
	transaction = EmailTransaction{}
)

// EmailTransaction represents an ongoing email transaction.
type EmailTransaction struct {
	from        string   // Sender email
	to          []string // Recipients (To, CC, BCC)
	cc          []string
	bcc         []string
	dataBuffers []bytes.Buffer // Email message parts (for DATA)
}

func (e *EmailTransaction) addRecipient(rcpt string) {
	for _, r := range e.to {
		if strings.EqualFold(r, rcpt) {
			return // Avoid duplicates
		}
	}
	e.to = append(e.to, rcpt)
}

func (e *EmailTransaction) appendData(content []byte) {
	var buffer bytes.Buffer
	buffer.Write(content)
	e.dataBuffers = append(e.dataBuffers, buffer)
}

func (e *EmailTransaction) resetDataBuffers() {
	e.dataBuffers = make([]bytes.Buffer, 0) // Clear data buffers
}

func (e *EmailTransaction) resetAll() {
	e.from = ""
	e.to = nil
	e.resetDataBuffers()
}

// --- SMTP Session ---
type Session struct{}

func (s *Session) Mail(from string, opts *smtp.MailOptions) error {
	mu.Lock()
	defer mu.Unlock()

	transaction.from = from
	logger.Printf("MAIL FROM: %s", from)
	transaction.resetDataBuffers()
	return nil
}

func (s *Session) Rcpt(to string, opts *smtp.RcptOptions) error {
	mu.Lock()
	defer mu.Unlock()

	transaction.addRecipient(to)
	logger.Printf("Recipient added: %s", to)
	return nil
}

func (s *Session) Data(r io.Reader) error {
	mu.Lock()
	defer mu.Unlock()

	if transaction.from == "" || len(transaction.to) == 0 {
		logger.Printf("DATA command received without MAIL FROM or RCPT TO. Ignoring.")
		return fmt.Errorf("DATA requires MAIL FROM and RCPT TO")
	}

	var tempBuffer bytes.Buffer
	_, err := io.Copy(&tempBuffer, r)
	if err != nil {
		logger.Printf("Failed to read DATA content: %v", err)
		return err
	}

	transaction.appendData(tempBuffer.Bytes())
	logger.Printf("DATA segment appended, size=%d bytes", tempBuffer.Len())
	return nil
}

func (s *Session) Reset() {
	mu.Lock()
	defer mu.Unlock()

	logger.Println("RESET command received. Preserving recipient and sender state.")
}

func (s *Session) Logout() error {
	mu.Lock()
	defer mu.Unlock()

	logger.Println("Session ending. Finalizing transaction...")

	totalBufferLength := 0
	for _, buffer := range transaction.dataBuffers {
		totalBufferLength += buffer.Len()
	}

	logger.Printf("Transaction summary before QUIT: from=%s, to=%v, totalBuffers=%d, totalLength=%d",
		transaction.from, transaction.to, len(transaction.dataBuffers), totalBufferLength)

	if transaction.from == "" || len(transaction.to) == 0 || totalBufferLength == 0 {
		logger.Println("Skipping invalid transaction (missing sender, recipients, or content).")
	} else {
		processEmail()
	}

	transaction.resetAll()
	logger.Println("Transaction reset. Session successfully ended.")
	return nil
}

// --- Email Processing ---
func processEmail() {
	if transaction.from == "" || len(transaction.to) == 0 || len(transaction.dataBuffers) == 0 {
		logger.Println("Empty transaction. Skipping email processing.")
		return
	}

	// Concatenate all buffers into the email content
	var fullBody bytes.Buffer
	for _, buffer := range transaction.dataBuffers {
		fullBody.Write(buffer.Bytes())
	}
	emailContent := fullBody.String()

	logger.Printf("Processing email from: %s", transaction.from)
	logger.Printf("Recipients: %v", transaction.to)

	// Parse the email using go-message
	r := strings.NewReader(emailContent)
	msg, err := mail.CreateReader(r)
	if err != nil {
		logger.Printf("Failed to parse email: %v", err)
		return
	}

	// Extract Subject
	subject := "No Subject"
	if headerSubject, _ := msg.Header.Subject(); headerSubject != "" {
		subject = headerSubject
	}
	logger.Printf("Email subject: %s", subject)

	// Further email parsing and sending logic (omitted for brevity)
	// Ensure full-body processing and attach email-sending functionality.
}

// --- Main Function ---
func main() {
	// Load the configuration
	if err := loadConfig(); err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	// Initialize the logger
	if err := initLogger(); err != nil {
		log.Fatalf("Error initializing logger: %v", err)
	}

	// Check if there are additional command-line arguments
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install":
			if len(os.Args) < 5 { // Ensure arguments for service setup are provided
				fmt.Println("Usage: install <service_name> <display_name> <description>")
				os.Exit(1)
			}
			serviceName := os.Args[2]
			displayName := os.Args[3]
			description := os.Args[4]
			installService(serviceName, displayName, description)
			return

		case "remove":
			if len(os.Args) < 3 { // Ensure the service name is provided
				fmt.Println("Usage: remove <service_name>")
				os.Exit(1)
			}
			serviceName := os.Args[2]
			removeService(serviceName)
			return

		case "help":
			fmt.Println("Usage:")
			fmt.Println("  install <service_name> <display_name> <description> - Install the service.")
			fmt.Println("  remove <service_name> - Remove the service.")
			fmt.Println("  <no arguments> - Run the application in service or standalone mode.")
			os.Exit(0)

		default:
			fmt.Printf("Unknown command: %s\n", os.Args[1])
			fmt.Println("Use 'help' for a list of available commands.")
			os.Exit(1)
		}
	}

	// Determine if running as a Windows service
	if isWindowsService() {
		// Run as a Windows service
		if err := runWindowsService(); err != nil {
			log.Fatalf("Failed to run service: %v", err)
		}
	} else {
		// Run as a standalone application
		log.Println("Starting as a standalone application...")
		if err := runApp(); err != nil {
			log.Fatalf("Application failed: %v", err)
		}
	}
}
