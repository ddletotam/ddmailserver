package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/authlimit"
	"github.com/ddletotam/ddmailserver/internal/clientip"
	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/logmask"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/notify"
	"github.com/ddletotam/ddmailserver/internal/outbox"
	"github.com/ddletotam/ddmailserver/internal/smtp/localdelivery"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// Session represents an SMTP session
type Session struct {
	database    *db.DB
	authLimiter *authlimit.Limiter
	// hub tells the user's clients about the accepted message (may be nil).
	hub *notify.Hub
	// kickOutbox sends queued mail now instead of at the next scheduler
	// tick (may be nil).
	kickOutbox func()
	// senders overrides database for sender-ownership lookups (tests).
	senders db.SenderStore
	// directory overrides database for local recipient lookups (tests).
	directory localdelivery.Directory
	conn      *smtp.Conn
	username  string
	userID    int64
	from      string
	to        []string
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
	if err := s.checkLocalRecipient(to); err != nil {
		return err
	}
	s.to = append(s.to, to)
	return nil
}

// checkLocalRecipient refuses, right at RCPT, an address on one of our own
// domains that cannot take mail — by the same rules and with the same reply
// the MX gives an outside sender (550 5.1.1 for a missing mailbox). The
// client shows the error at once, instead of the message being queued and
// bounced later. Addresses on other domains are not checked here.
func (s *Session) checkLocalRecipient(to string) error {
	dir := s.recipientDirectory()
	if dir == nil {
		return nil
	}
	_, err := localdelivery.Lookup(dir, s.extractEmail(to))
	if err == nil || errors.Is(err, localdelivery.ErrNotLocal) {
		return nil
	}
	var reject *localdelivery.RejectError
	if errors.As(err, &reject) {
		log.Printf("SMTP: rejecting local recipient %s: %s", logmask.Addr(to), reject.Message)
		return &smtp.SMTPError{
			Code:         reject.Code,
			EnhancedCode: smtp.EnhancedCode{reject.Enhanced[0], reject.Enhanced[1], reject.Enhanced[2]},
			Message:      reject.Message,
		}
	}
	log.Printf("SMTP: recipient lookup for %s failed: %v", logmask.Addr(to), err)
	return &smtp.SMTPError{
		Code:         451,
		EnhancedCode: smtp.EnhancedCode{4, 3, 0},
		Message:      "Temporary failure looking up recipient, try again later",
	}
}

// recipientDirectory returns where local recipients are looked up, nil if
// nowhere (a session built without a database).
func (s *Session) recipientDirectory() localdelivery.Directory {
	if s.directory != nil {
		return s.directory
	}
	if s.database != nil {
		return s.database
	}
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

	// The copy in Sent with delivery status «pending», now: the conversation
	// exists from the moment we answer 250, not from delivery. The client's
	// own APPEND to Sent, if it makes one, is the same message (same
	// Message-ID) and is deduplicated. A failure leaves the message queued;
	// the send task finishes the acceptance.
	if err := outbox.New(s.database, s.hub).Accept(outboxMsg); err != nil {
		log.Printf("Message %d queued, but accepting it failed (it is sent anyway): %v", outboxMsg.ID, err)
	}
	if s.kickOutbox != nil {
		s.kickOutbox()
	}

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
