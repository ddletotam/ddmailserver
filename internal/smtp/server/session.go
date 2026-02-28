package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-smtp"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"golang.org/x/crypto/bcrypt"
)

// Session represents an SMTP session
type Session struct {
	database *db.DB
	conn     *smtp.Conn
	username string
	userID   int64
	from     string
	to       []string
}

// AuthPlain implements PLAIN authentication
func (s *Session) AuthPlain(username, password string) error {
	log.Printf("SMTP AUTH PLAIN for user: %s", username)

	// Get user from database
	user, err := s.database.GetUserByUsername(username)
	if err != nil {
		log.Printf("User not found: %s", username)
		return errors.New("invalid credentials")
	}

	// Verify password using bcrypt
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		log.Printf("Invalid password for user: %s", username)
		return errors.New("invalid credentials")
	}

	log.Printf("User %s authenticated successfully", username)

	s.username = username
	s.userID = user.ID

	return nil
}

// Mail is called to set the sender
func (s *Session) Mail(from string, opts *smtp.MailOptions) error {
	log.Printf("MAIL FROM: %s", from)
	s.from = from
	return nil
}

// Rcpt is called to set a recipient
func (s *Session) Rcpt(to string, opts *smtp.RcptOptions) error {
	log.Printf("RCPT TO: %s", to)
	s.to = append(s.to, to)
	return nil
}

// Data is called when the client wants to send the message body
func (s *Session) Data(r io.Reader) error {
	log.Printf("Receiving message from %s to %v", s.from, s.to)

	if s.from == "" {
		return errors.New("no sender specified")
	}

	if len(s.to) == 0 {
		return errors.New("no recipients specified")
	}

	// Read the entire message
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return fmt.Errorf("failed to read message: %w", err)
	}

	messageData := buf.Bytes()

	// Parse the message to extract headers
	mr, err := mail.CreateReader(bytes.NewReader(messageData))
	if err != nil {
		return fmt.Errorf("failed to parse message: %w", err)
	}

	header := mr.Header

	// Extract fields
	subject, _ := header.Subject()
	_, _ = header.AddressList("From")
	_, _ = header.AddressList("To")
	cc, _ := header.AddressList("Cc")

	// Determine which account to use for sending
	accountID, err := s.determineAccount(s.from)
	if err != nil {
		return fmt.Errorf("failed to determine account: %w", err)
	}

	// Extract body
	var body, bodyHTML string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("Error reading part: %v", err)
			break
		}

		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			contentType, _, _ := h.ContentType()
			bodyBytes, _ := io.ReadAll(p.Body)

			if contentType == "text/plain" {
				body = string(bodyBytes)
			} else if contentType == "text/html" {
				bodyHTML = string(bodyBytes)
			}
		}
	}

	// Create outbox message
	outboxMsg := &models.OutboxMessage{
		UserID:    s.userID,
		AccountID: accountID,
		From:      s.from,
		To:        s.joinRecipients(s.to),
		Cc:        s.formatAddressList(cc),
		Subject:   subject,
		Body:      body,
		BodyHTML:  bodyHTML,
		RawEmail:  messageData,
		Status:    "pending",
		Retries:   0,
	}

	// Save to database
	if err := s.database.CreateOutboxMessage(outboxMsg); err != nil {
		return fmt.Errorf("failed to save message: %w", err)
	}

	log.Printf("Message %d queued for sending from %s to %v", outboxMsg.ID, s.from, s.to)

	return nil
}

// Reset resets the session state
func (s *Session) Reset() {
	log.Printf("Resetting SMTP session")
	s.from = ""
	s.to = nil
}

// Logout is called when the client logs out
func (s *Session) Logout() error {
	log.Printf("SMTP session logout")
	return nil
}

// determineAccount finds which account to use for sending based on the from address
func (s *Session) determineAccount(fromAddr string) (int64, error) {
	// Extract email from address (could be "Name <email@example.com>")
	email := s.extractEmail(fromAddr)

	// Get all accounts for this user
	accounts, err := s.database.GetAccountsByUserID(s.userID)
	if err != nil {
		return 0, err
	}

	// Find account matching the from address
	for _, account := range accounts {
		if strings.EqualFold(account.Email, email) {
			return account.ID, nil
		}
	}

	// If no exact match, use the first enabled account
	for _, account := range accounts {
		if account.Enabled {
			log.Printf("No exact match for %s, using account %s", email, account.Email)
			return account.ID, nil
		}
	}

	return 0, fmt.Errorf("no suitable account found for sending")
}

// extractEmail extracts email address from various formats
func (s *Session) extractEmail(addr string) string {
	// Handle formats like:
	// - "user@example.com"
	// - "Name <user@example.com>"
	// - "<user@example.com>"

	addr = strings.TrimSpace(addr)

	// Check for angle brackets
	start := strings.Index(addr, "<")
	end := strings.Index(addr, ">")

	if start >= 0 && end > start {
		return strings.TrimSpace(addr[start+1 : end])
	}

	return addr
}

// joinRecipients joins recipient addresses into a comma-separated string
func (s *Session) joinRecipients(recipients []string) string {
	return strings.Join(recipients, ", ")
}

// formatAddressList formats an address list to a string
func (s *Session) formatAddressList(addresses []*mail.Address) string {
	if len(addresses) == 0 {
		return ""
	}

	var result []string
	for _, addr := range addresses {
		if addr.Name != "" {
			result = append(result, fmt.Sprintf("%s <%s>", addr.Name, addr.Address))
		} else {
			result = append(result, addr.Address)
		}
	}

	return strings.Join(result, ", ")
}
