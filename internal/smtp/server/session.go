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
	"github.com/yourusername/mailserver/internal/authlimit"
	"github.com/yourusername/mailserver/internal/clientip"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/logmask"
	"github.com/yourusername/mailserver/internal/models"
)

// Session represents an SMTP session
type Session struct {
	database    *db.DB
	authLimiter *authlimit.Limiter
	// senders overrides database for sender-ownership lookups (tests).
	senders  db.SenderStore
	conn     *smtp.Conn
	username string
	userID   int64
	from     string
	to       []string
	// accountID is how the accepted MAIL FROM will be sent: >0 relays
	// through that external account, 0 delivers directly (local mailbox).
	accountID int64
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

	ip := s.remoteIP()
	if !s.authLimiter.Allow(ip, username) {
		log.Printf("SMTP auth throttled for user: %s from %s", username, ip)
		return errors.New("invalid credentials")
	}

	// Accepts the account password or an application password; also strips the
	// @domain part some clients insist on sending.
	user, err := s.database.AuthenticateProtocol(username, password)
	if err != nil {
		if errors.Is(err, db.ErrInvalidCredentials) {
			s.authLimiter.Failure(ip, username, password)
		}
		log.Printf("SMTP auth failed for user: %s from %s", username, ip)
		return errors.New("invalid credentials")
	}
	s.authLimiter.Success(ip, user.Username)
	username = user.Username

	log.Printf("User %s authenticated successfully", username)

	s.username = username
	s.userID = user.ID

	return nil
}

// remoteIP returns the client IP of the session, "" if unknown.
func (s *Session) remoteIP() string {
	if s.conn == nil || s.conn.Conn() == nil {
		return ""
	}
	return clientip.FromNetAddr(s.conn.Conn().RemoteAddr())
}

// Mail is called to set the sender. Submission requires AUTH, and the
// envelope sender must be one of the authenticated user's own addresses —
// otherwise any user could send as any address of any local domain, DKIM
// signed by us.
func (s *Session) Mail(from string, opts *smtp.MailOptions) error {
	log.Printf("MAIL FROM: %s", logmask.Addr(from))
	if s.userID == 0 {
		return smtp.ErrAuthRequired
	}

	owned, err := db.SenderIdentities(s.identityStore(), s.userID)
	if err != nil {
		log.Printf("SMTP: resolving sender identities for user %d: %v", s.userID, err)
		return &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 3, 0},
			Message:      "Temporary failure resolving sender, try again later",
		}
	}

	email := strings.ToLower(s.extractEmail(from))
	accountID, ok := owned[email]
	if !ok {
		log.Printf("SMTP: user %s may not send as %s", s.username, logmask.Addr(from))
		return &smtp.SMTPError{
			Code:         553,
			EnhancedCode: smtp.EnhancedCode{5, 7, 1},
			Message:      "Sender address rejected: not owned by authenticated user",
		}
	}

	s.from = from
	s.accountID = accountID
	return nil
}

// Rcpt is called to set a recipient
func (s *Session) Rcpt(to string, opts *smtp.RcptOptions) error {
	log.Printf("RCPT TO: %s", logmask.Addr(to))
	if s.userID == 0 {
		return smtp.ErrAuthRequired
	}
	s.to = append(s.to, to)
	return nil
}

// Data is called when the client wants to send the message body
func (s *Session) Data(r io.Reader) error {
	log.Printf("Receiving message from %s to %s", logmask.Addr(s.from), logmask.AddrSlice(s.to))

	if s.userID == 0 {
		return smtp.ErrAuthRequired
	}

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

	// The header From (and Sender) is what recipients see and what DMARC
	// checks; an owned envelope sender with a forged From header would still
	// be a spoof, so every address there must be the user's own too.
	if err := s.checkHeaderSenders(header); err != nil {
		return err
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
	cc, _ := header.AddressList("Cc")
	accountID := s.accountID

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
	s.accountID = 0
}

// Logout is called when the client logs out
func (s *Session) Logout() error {
	log.Printf("SMTP session logout")
	return nil
}

// identityStore returns where sender ownership is looked up.
func (s *Session) identityStore() db.SenderStore {
	if s.senders != nil {
		return s.senders
	}
	return s.database
}

// checkHeaderSenders rejects a message whose From (or Sender) header names
// an address the authenticated user does not own, or has no From at all.
func (s *Session) checkHeaderSenders(header mail.Header) error {
	reject := func(msg string) error {
		return &smtp.SMTPError{
			Code:         550,
			EnhancedCode: smtp.EnhancedCode{5, 7, 1},
			Message:      msg,
		}
	}

	from, err := header.AddressList("From")
	if err != nil || len(from) == 0 {
		return reject("A valid From header is required")
	}
	sender, err := header.AddressList("Sender")
	if err != nil {
		return reject("Invalid Sender header")
	}

	owned, err := db.SenderIdentities(s.identityStore(), s.userID)
	if err != nil {
		log.Printf("SMTP: resolving sender identities for user %d: %v", s.userID, err)
		return &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 3, 0},
			Message:      "Temporary failure resolving sender, try again later",
		}
	}
	for _, a := range append(from, sender...) {
		if _, ok := owned[strings.ToLower(a.Address)]; !ok {
			log.Printf("SMTP: user %s may not send with header address %s", s.username, logmask.Addr(a.Address))
			return reject("From header address not owned by authenticated user")
		}
	}
	return nil
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
