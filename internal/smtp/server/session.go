package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/logmask"
	"github.com/yourusername/mailserver/internal/models"
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

// AuthMechanisms returns available auth mechanisms (advertised in EHLO)
func (s *Session) AuthMechanisms() []string {
	return []string{"PLAIN"}
}

// Auth handles SASL authentication for the AUTH extension
func (s *Session) Auth(mech string) (sasl.Server, error) {
	switch mech {
	case "PLAIN":
		return sasl.NewPlainServer(func(identity, username, password string) error {
			return s.AuthPlain(username, password)
		}), nil
	default:
		return nil, fmt.Errorf("unsupported auth mechanism: %s", mech)
	}
}

// AuthPlain implements PLAIN authentication
func (s *Session) AuthPlain(username, password string) error {
	log.Printf("SMTP AUTH PLAIN for user: %s", username)

	// Accepts the account password or an application password; also strips the
	// @domain part some clients insist on sending.
	user, err := s.database.AuthenticateProtocol(username, password)
	if err != nil {
		log.Printf("SMTP auth failed for user: %s", username)
		return errors.New("invalid credentials")
	}
	username = user.Username

	log.Printf("User %s authenticated successfully", username)

	s.username = username
	s.userID = user.ID

	return nil
}

// Mail is called to set the sender
func (s *Session) Mail(from string, opts *smtp.MailOptions) error {
	log.Printf("MAIL FROM: %s", logmask.Addr(from))
	s.from = from
	return nil
}

// Rcpt is called to set a recipient
func (s *Session) Rcpt(to string, opts *smtp.RcptOptions) error {
	log.Printf("RCPT TO: %s", logmask.Addr(to))
	s.to = append(s.to, to)
	return nil
}

// Data is called when the client wants to send the message body
func (s *Session) Data(r io.Reader) error {
	log.Printf("Receiving message from %s to %s", logmask.Addr(s.from), logmask.AddrSlice(s.to))

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

	// RFC 5322 §3.6.4 says Message-ID SHOULD be present, not MUST. We make it MUST on
	// our submission port: a missing Message-ID later forces our parser to mint a
	// `<unixnano@generated.local>` fallback, which makes the same email impossible to
	// dedup across folders/copies. Rejecting at submission keeps storage clean.
	msgID, _ := header.Text("Message-Id")
	msgID = strings.TrimSpace(msgID)
	msgID = strings.TrimPrefix(msgID, "<")
	msgID = strings.TrimSuffix(msgID, ">")
	if msgID == "" {
		log.Printf("SMTP: rejecting submission from %s — missing Message-Id header", logmask.Addr(s.from))
		return &smtp.SMTPError{
			Code:         554,
			EnhancedCode: smtp.EnhancedCode{5, 6, 0},
			Message:      "Message-Id header is required",
		}
	}

	// Dedup: if this Message-ID already exists in the user's Sent folder,
	// the message was already delivered — return OK without re-queuing.
	// This prevents buggy clients (e.g. eM Client) from flooding recipients
	// by re-submitting the same message every 10 minutes.
	if sentFolder, err := s.database.GetLocalFolderByType(s.userID, "sent"); err == nil && sentFolder != nil {
		if exists, _ := s.database.MessageExistsInFolder(sentFolder.ID, msgID); exists {
			log.Printf("SMTP: dedup — message %s from %s already in Sent folder, returning OK without re-queuing", msgID, logmask.Addr(s.from))
			return nil
		}
	}

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

	log.Printf("Message %d queued for sending from %s to %s", outboxMsg.ID, logmask.Addr(s.from), logmask.AddrSlice(s.to))

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

	// Check if sender is from a local domain — use direct delivery (accountID=0)
	parts := strings.SplitN(email, "@", 2)
	if len(parts) == 2 {
		if _, err := s.database.GetDomainByName(parts[1]); err == nil {
			log.Printf("Local domain sender %s, using direct delivery", logmask.Addr(email))
			return 0, nil
		}
	}

	// If no exact match, use the first enabled account
	for _, account := range accounts {
		if account.Enabled {
			log.Printf("No exact match for %s, using account %s", logmask.Addr(email), logmask.Addr(account.Email))
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
