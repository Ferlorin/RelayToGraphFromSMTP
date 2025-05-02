package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-smtp"
	"golang.org/x/text/encoding/charmap"
	"gopkg.in/ini.v1"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// --- Config and Logger Setup ---

type Config struct {
	TenantID     string
	ClientID     string
	ClientSecret string
	Scope        string
	Host         string
	Port         string
	ServiceName  string
	Debug        bool
}

var config Config
var logger *log.Logger

func init() {
	// Register Windows-1251 charset
	charset.RegisterEncoding("windows-1251", charmap.Windows1251)
}

func debugLog(format string, v ...interface{}) {
	if config.Debug {
		logger.Printf("[DEBUG] "+format, v...) // Only log if debug is enabled
	}
}

func initWorkingDir() {
	ex, err := os.Executable()
	if err != nil {
		fmt.Printf("Could not get executable path: %v", err)
		return
	}
	wd := filepath.Dir(ex)
	if err := os.Chdir(wd); err != nil {
		fmt.Printf("Could not change working directory to %s: %v", wd, err)
	} else {
		current, _ := os.Getwd()
		fmt.Printf("Working directory set to: %s", current)
	}
}

func loadConfig() error {
	cfg, err := ini.Load("config.ini")
	if err != nil {
		return err
	}

	// Load Microsoft Graph settings
	config.TenantID = cfg.Section("MicrosoftGraph").Key("TenantID").String()
	config.ClientID = cfg.Section("MicrosoftGraph").Key("ClientID").String()
	config.ClientSecret = cfg.Section("MicrosoftGraph").Key("ClientSecret").String()
	config.Scope = cfg.Section("MicrosoftGraph").Key("Scope").String()

	// Load Server settings
	config.Host = cfg.Section("Server").Key("Host").String()
	config.Port = cfg.Section("Server").Key("SMTPPort").String()

	// Load Service settings
	config.ServiceName = cfg.Section("Service").Key("ServiceName").String()
	// Parse Debug as a boolean (default is false if the value is missing)
	config.Debug = cfg.Section("Service").Key("Debug").MustBool(false)

	return nil
}

func initLogger() error {
	ex, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	wd := filepath.Dir(ex)
	logPath := filepath.Join(wd, "app.log")

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return fmt.Errorf("failed to open log file at %s: %w", logPath, err)
	}

	mw := io.MultiWriter(f)
	logger = log.New(mw, "", log.LstdFlags)

	logger.Printf("Logger initialized successfully. Writing to: %s", logPath)

	return nil
}

// --- SMTP Backend ---
type Backend struct{}

func (bkd *Backend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
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

func (s *Session) Mail(from string, _ *smtp.MailOptions) error {
	mu.Lock()
	defer mu.Unlock()

	transaction.from = from
	logger.Printf("MAIL FROM: %s", from)
	transaction.resetDataBuffers()
	return nil
}

func (s *Session) Rcpt(to string, _ *smtp.RcptOptions) error {
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
	debugLog("DATA segment appended, size=%d bytes", tempBuffer.Len())
	return nil
}

func (s *Session) Reset() {
	mu.Lock()
	defer mu.Unlock()

	debugLog("RESET command received. Preserving recipient and sender state.")
}

func (s *Session) Logout() error {
	mu.Lock()
	defer mu.Unlock()

	logger.Println("Session ending. Finalizing transaction...")

	totalBufferLength := 0
	for _, buffer := range transaction.dataBuffers {
		totalBufferLength += buffer.Len()
	}

	debugLog("Transaction summary before QUIT: from=%s, to=%v, totalBuffers=%d, totalLength=%d",
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

	// Extract To, CC, and classify BCC recipients
	var (
		toList      []string
		ccList      []string
		bccList     []string
		attachments []map[string]interface{} // Array for attachments
		rcptMap     = make(map[string]bool)  // Track recipients
		textBody    string
		htmlBody    string
	)

	// Parse To and CC headers
	if toAddrs, err := msg.Header.AddressList("To"); err == nil {
		for _, addr := range toAddrs {
			toList = append(toList, addr.Address)
			rcptMap[strings.ToLower(addr.Address)] = true
		}
	}
	if ccAddrs, err := msg.Header.AddressList("Cc"); err == nil {
		for _, addr := range ccAddrs {
			ccList = append(ccList, addr.Address)
			rcptMap[strings.ToLower(addr.Address)] = true
		}
	}

	// Recipients not in To or CC are assumed to be BCC
	for _, rcpt := range transaction.to {
		if !rcptMap[strings.ToLower(rcpt)] {
			bccList = append(bccList, rcpt)
		}
	}

	// Process MIME parts to extract body and attachments
	for {
		part, err := msg.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			logger.Printf("Failed to read MIME part: %v", err)
			return
		}

		// Handle Inline Headers for email content
		switch h := part.Header.(type) {
		case *mail.InlineHeader:
			contentType, _, _ := h.ContentType()
			bodyBytes, _ := io.ReadAll(part.Body)

			switch contentType {
			case "text/plain":
				if textBody == "" { // Use first plain-text part
					textBody = string(bodyBytes)
				}
			case "text/html":
				if htmlBody == "" { // Use first HTML part, if present
					htmlBody = string(bodyBytes)
				}
			}

		case *mail.AttachmentHeader:
			// Extract Attachment Information
			filename, _ := h.Filename()
			contentType, _, _ := h.ContentType()
			attachmentBytes, _ := io.ReadAll(part.Body)

			// Add to Graph API's attachment structure
			attachments = append(attachments, map[string]interface{}{
				"@odata.type":  "#microsoft.graph.fileAttachment",
				"name":         filename,
				"contentType":  contentType,
				"contentBytes": string(base64.StdEncoding.EncodeToString(attachmentBytes)),
			})
		}
	}

	// Use HTML body if available; otherwise, fallback to plain-text
	messageBody := textBody
	bodyContentType := "Text"
	if htmlBody != "" {
		messageBody = htmlBody
		bodyContentType = "HTML"
	}

	// Debug recipients and attachments
	logger.Printf("Final Recipients: To: %v, Cc: %v, Bcc: %v", toList, ccList, bccList)
	debugLog("Attachments field (array): %v", attachments)

	// Ensure attachments is **always** an array (important fix)
	if attachments == nil {
		attachments = []map[string]interface{}{}
	}

	// Build the email payload
	graphMessage := buildGraphMessage(subject, bodyContentType, messageBody, toList, ccList, bccList, attachments)

	// Send the email
	err = sendMail(transaction.from, graphMessage)
	if err != nil {
		logger.Printf("Failed to send email: %v", err)
		return
	}

	logger.Println("Email processed and sent successfully.")
}

func buildGraphMessage(subject string, bodyContentType string, messageBody string, toList []string, ccList []string, bccList []string, attachments []map[string]interface{}) map[string]interface{} {
	graphMessage := map[string]interface{}{
		"message": map[string]interface{}{
			"subject": subject,
			"body": map[string]interface{}{
				"contentType": bodyContentType,
				"content":     messageBody,
			},
			"toRecipients":  buildRecipients(toList),
			"ccRecipients":  buildRecipients(ccList),
			"bccRecipients": buildRecipients(bccList),
			"attachments":   attachments, // Always an array, empty if no attachments
		},
		"saveToSentItems": false,
	}
	return graphMessage
}

func buildRecipients(list []string) []map[string]interface{} {
	// Always return an empty array if the input list is nil or empty
	if len(list) == 0 {
		return []map[string]interface{}{}
	}

	var recipients []map[string]interface{}
	for _, addr := range list {
		recipients = append(recipients, map[string]interface{}{
			"emailAddress": map[string]string{"address": addr},
		})
	}
	return recipients
}

// Send the email via Microsoft Graph API.
func sendMail(sender string, payload map[string]interface{}) error {
	url := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/sendMail", sender)
	body, _ := json.Marshal(payload)

	token, err := getAccessToken()
	if err != nil {
		return fmt.Errorf("failed to get access token: %v", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			logger.Printf("Error closing response body: %v", cerr)
		}
	}()

	if resp.StatusCode != 202 {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("graph API error: %s", string(responseBody))
	}
	return nil
}

// Get Microsoft Graph API access token.
func getAccessToken() (string, error) {
	url := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", config.TenantID)
	data := fmt.Sprintf(
		"client_id=%s&scope=%s&client_secret=%s&grant_type=client_credentials",
		config.ClientID, config.Scope, config.ClientSecret,
	)

	req, err := http.NewRequest("POST", url, strings.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			logger.Printf("Error closing response body: %v", cerr)
		}
	}()

	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.AccessToken, nil
}

// --- Main Function ---
func main() {

	// Set the working directory to the executable's directory.
	initWorkingDir()

	// Initialize the logger
	if err := initLogger(); err != nil {
		fmt.Printf("Failed to initialize logger: %v\n", err)
		return
	}

	// Load the configuration
	if err := loadConfig(); err != nil {
		logger.Fatalf("Error loading config: %v", err)
	}

	isWindowsService := isWindowsService()

	// Determine if running as a Windows service
	if !isWindowsService {
		newWriter := io.MultiWriter(logger.Writer(), os.Stdout)
		logger.SetOutput(newWriter)

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
				fmt.Println("  -debug - Enable debug mode (overrides config).")
				fmt.Println("  <no arguments> - Run the application in service or standalone mode.")
				os.Exit(0)

			case "-debug":
				// Enable debug mode by overriding the config variable
				config.Debug = true
				logger.Println("Debug mode enabled")
				// Continue to the application startup

			default:
				logger.Printf("Unknown command: %s\n", os.Args[1])
				logger.Println("Use 'help' for a list of available commands.")
				os.Exit(1)
			}
		}
	}

	// Determine if running as a Windows service
	if isWindowsService {
		// Run as a Windows service
		logger.Printf("Starting as a Windows Service with name: %s", config.ServiceName)
		if err := runWindowsService(); err != nil {
			logger.Fatalf("Failed to run as Windows Service: %v", err)
		}

	} else {
		// Run as a standalone application
		logger.Println("Running as standalone application...")
		if err := runApp(); err != nil {
			logger.Fatalf("Application error: %v", err)
		}

	}
	logger.Println("Application finished.")

}
