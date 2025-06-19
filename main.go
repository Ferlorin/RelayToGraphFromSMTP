package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-smtp"
	"github.com/google/uuid"
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

type TransactionManager struct {
	mu           sync.Mutex
	transactions map[string]*EmailTransaction
	timeouts     map[string]time.Time
}

var globalManager = NewTransactionManager()

func NewTransactionManager() *TransactionManager {
	tm := &TransactionManager{
		transactions: make(map[string]*EmailTransaction),
		timeouts:     make(map[string]time.Time),
	}
	go tm.cleanup()
	return tm
}

func (tm *TransactionManager) cleanup() {
	ticker := time.NewTicker(30 * time.Second)
	for range ticker.C {
		tm.mu.Lock()
		now := time.Now()
		for key, timeout := range tm.timeouts {
			if now.Sub(timeout) > 5*time.Minute {
				delete(tm.transactions, key)
				delete(tm.timeouts, key)
				logger.Printf("Cleaned up abandoned transaction: %s", key)
			}
		}
		tm.mu.Unlock()
	}
}

func (tm *TransactionManager) startCleanupRoutine() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			tm.mu.Lock()
			now := time.Now()
			for key, timeout := range tm.timeouts {
				if now.Sub(timeout) > 2*time.Minute {
					if trans, exists := tm.transactions[key]; exists {
						logger.Printf("Cleaning up abandoned transaction: From=%s, Recipients=%d",
							trans.from, len(trans.to)+len(trans.cc)+len(trans.bcc))
					}
					delete(tm.transactions, key)
					delete(tm.timeouts, key)
				}
			}
			tm.mu.Unlock()
		}
	}()
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
	return &Session{
		sessionID: uuid.New().String(),
	}, nil
}

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
type Session struct {
	mu          sync.Mutex
	sessionID   string
	currentKey  string
	activeEmail string
	pendingKeys []string // Add this to track all transactions in the session
}

func (s *Session) Mail(from string, _ *smtp.MailOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.activeEmail = from
	// Include timestamp to make each transaction unique
	transactionTime := time.Now().UnixNano()
	s.currentKey = fmt.Sprintf("%s:%s:%d", s.sessionID, from, transactionTime)

	globalManager.mu.Lock()
	trans := &EmailTransaction{from: from}
	globalManager.transactions[s.currentKey] = trans
	globalManager.timeouts[s.currentKey] = time.Now()
	globalManager.mu.Unlock()

	logger.Printf("[%s] MAIL FROM: %s", s.sessionID, from)
	return nil
}

func (s *Session) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.currentKey == "" {
		logger.Printf("[%s] Warning: Attempting to add recipient without an active transaction key", s.sessionID)
		return fmt.Errorf("no active transaction")
	}

	globalManager.mu.Lock()
	trans, exists := globalManager.transactions[s.currentKey]
	if !exists {
		globalManager.mu.Unlock()
		logger.Printf("[%s] Error: Transaction not found for key: %s", s.sessionID, s.currentKey)
		return fmt.Errorf("transaction not found")
	}
	trans.addRecipient(to)
	globalManager.timeouts[s.currentKey] = time.Now()
	globalManager.mu.Unlock()

	logger.Printf("[%s] Recipient added: %s", s.sessionID, to)
	return nil
}

func (s *Session) Data(r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.currentKey == "" {
		return fmt.Errorf("no active transaction")
	}

	var tempBuffer bytes.Buffer
	if _, err := io.Copy(&tempBuffer, r); err != nil {
		logger.Printf("Failed to read DATA content: %v", err)
		return err
	}

	msg, err := message.Read(strings.NewReader(tempBuffer.String()))
	if err != nil {
		logger.Printf("Failed to parse email: %v", err)
		return err
	}

	subject := msg.Header.Get("Subject")
	if subject == "" {
		subject = "No Subject"
	}

	// Create the final key with subject
	finalKey := fmt.Sprintf("%s:%s:%s", s.sessionID, s.activeEmail, subject)

	globalManager.mu.Lock()
	if trans, exists := globalManager.transactions[s.currentKey]; exists {
		// Move the transaction to the new key that includes the subject
		globalManager.transactions[finalKey] = trans
		globalManager.timeouts[finalKey] = time.Now()
		delete(globalManager.transactions, s.currentKey)
		delete(globalManager.timeouts, s.currentKey)
		s.currentKey = finalKey
		logger.Printf("[%s] Email transaction updated with subject: %s", s.sessionID, subject)
	}
	globalManager.mu.Unlock()

	return processEmailContent(msg, tempBuffer.Bytes(), globalManager.transactions[finalKey])
}

func (s *Session) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Clean up current transaction if any
	if s.currentKey != "" {
		globalManager.mu.Lock()
		delete(globalManager.transactions, s.currentKey)
		delete(globalManager.timeouts, s.currentKey)
		globalManager.mu.Unlock()
	}

	s.currentKey = ""
	s.activeEmail = ""
	s.pendingKeys = nil
	debugLog("RESET command received")
}

func (s *Session) Logout() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.currentKey == "" {
		return nil
	}

	globalManager.mu.Lock()
	trans, exists := globalManager.transactions[s.currentKey]
	if exists {
		// Process the email before cleaning up
		if err := processEmail(trans); err != nil {
			globalManager.mu.Unlock()
			logger.Printf("[%s] Failed to process email: %v", s.sessionID, err)
			return err
		}
		delete(globalManager.transactions, s.currentKey)
		delete(globalManager.timeouts, s.currentKey)
	}
	globalManager.mu.Unlock()

	logger.Printf("[%s] Session ended successfully", s.sessionID)
	return nil
}

func processEmailContent(msg *message.Entity, data []byte, trans *EmailTransaction) error {
	header := mail.Header{Header: msg.Header}

	if from, err := header.AddressList("From"); err == nil && len(from) > 0 {
		trans.from = from[0].Address
	}

	if to, err := header.AddressList("To"); err == nil {
		for _, addr := range to {
			trans.addRecipient(addr.Address)
		}
	}

	trans.appendData(data)
	return nil
}

// --- Email Processing ---
func processEmail(trans *EmailTransaction) error {
	if trans.from == "" || len(trans.to) == 0 || len(trans.dataBuffers) == 0 {
		logger.Println("Empty transaction. Skipping email processing.")
		return fmt.Errorf("invalid email transaction: missing required fields")
	}

	// Concatenate all buffers into the email content
	var fullBody bytes.Buffer
	for _, buffer := range trans.dataBuffers {
		fullBody.Write(buffer.Bytes())
	}
	emailContent := fullBody.String()

	logger.Printf("Processing email from: %s", trans.from)
	logger.Printf("Recipients: %v", trans.to)

	// Parse the email using go-message
	r := strings.NewReader(emailContent)
	msg, err := mail.CreateReader(r)
	if err != nil {
		logger.Printf("Failed to parse email: %v", err)
		return fmt.Errorf("failed to parse email: %v", err)
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
	for _, rcpt := range trans.to {
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
			return fmt.Errorf("failed to read MIME part: %v", err)
		}

		// Handle Inline Headers for email content
		switch h := part.Header.(type) {
		case *mail.InlineHeader:
			contentType, params, _ := h.ContentType()
			bodyBytes, _ := io.ReadAll(part.Body)
			charsetName := params["charset"]
			charsetName = strings.ToLower(charsetName)

			logger.Printf("CharsetName: %s and ContentType: %s", charsetName, contentType)

			switch contentType {
			case "text/plain":
				if textBody == "" { // Use first plain-text part
					textBody = string(bodyBytes)
				}
			case "text/html":
				if htmlBody == "" { // Use first HTML part, if present
					htmlBody = string(bodyBytes)
				}
			case "image/png", "image/jpeg", "image/jpg", "image/bmp", "image/gif": // Handle inline images
				contentID := h.Get("Content-ID")
				contentID = strings.Trim(contentID, "<>")

				// Convert the image to base64
				base64Image := base64.StdEncoding.EncodeToString(bodyBytes)

				// Replace the cid: reference in HTML with the base64 data
				if htmlBody != "" {
					oldRef := fmt.Sprintf("cid:%s", contentID)
					newRef := fmt.Sprintf("data:%s;base64,%s", contentType, base64Image)
					htmlBody = strings.ReplaceAll(htmlBody, oldRef, newRef)
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
				"contentBytes": base64.StdEncoding.EncodeToString(attachmentBytes),
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

	if messageBody == "" {
		return fmt.Errorf("email has no body content")
	}

	// Ensure attachments is **always** an array (important fix)
	if attachments == nil {
		attachments = []map[string]interface{}{}
	}

	// Build the email payload
	graphMessage := buildGraphMessage(subject, bodyContentType, messageBody, toList, ccList, bccList, attachments)
	if err := sendMail(trans.from, graphMessage); err != nil {
		logger.Printf("Failed to send email: %v", err)
		return fmt.Errorf("failed to send email: %v", err)
	}

	logger.Println("Email processed and sent successfully")
	return nil
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

func sendMail(sender string, payload map[string]interface{}) error {
	maxRetries := 3
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s
			backoff := time.Second * time.Duration(1<<attempt)
			logger.Printf("Retry attempt %d after %v delay", attempt+1, backoff)
			time.Sleep(backoff)
		}

		if err := doSendMail(sender, payload); err != nil {
			lastErr = err
			if !strings.Contains(err.Error(), "MailboxInfoStale") {
				// If it's not a MailboxInfoStale error, return immediately
				return err
			}
			continue
		}

		return nil // Success
	}

	return fmt.Errorf("failed after %d retries. Last error: %v", maxRetries, lastErr)
}

func doSendMail(sender string, payload map[string]interface{}) error {
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
