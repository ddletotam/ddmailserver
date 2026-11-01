package mx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/mail"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/parser"
	"github.com/ddletotam/ddmailserver/internal/smtp/localdelivery"
	"github.com/emersion/go-smtp"
)

// Recipient holds info about a validated recipient
type Recipient = localdelivery.Recipient

// Session represents an MX SMTP session
type Session struct {
	directory  localdelivery.Directory
	deliverer  *localdelivery.Deliverer
	from       string
	fromDomain string
	senderIP   string
	recipients []*Recipient
}

// AuthPlain - MX server does not require authentication
func (s *Session) AuthPlain(username, password string) error {
	// MX server accepts mail from anyone, no auth needed
	return nil
}

// Mail is called to set the sender (MAIL FROM)
func (s *Session) Mail(from string, opts *smtp.MailOptions) error {
	log.Printf("MX: MAIL FROM: %s", from)
	s.from = from

	// Extract domain for SPF check
	email := s.extractEmail(from)
	_, domain := s.splitEmail(email)
	s.fromDomain = domain

	return nil
}

// Rcpt is called to set a recipient (RCPT TO)
// This is where we validate if we accept mail for this address
func (s *Session) Rcpt(to string, opts *smtp.RcptOptions) error {
	log.Printf("MX: RCPT TO: %s", to)

	email := s.extractEmail(to)
	rcpt, err := localdelivery.Lookup(s.directory, email)
	if err != nil {
		return rcptError(email, err)
	}

	s.recipients = append(s.recipients, rcpt)
	log.Printf("MX: Accepted recipient %s (user_id=%d)", rcpt.Email, rcpt.Mailbox.UserID)
	return nil
}

// rcptError turns a lookup failure into the RCPT reply. It must be an
// *smtp.SMTPError: go-smtp answers any other error with 451, so the plain
// errors.New("550 No such user here") this used to return went out as
// "451 4.0.0 550 No such user here" — a temporary failure, and the sending
// MTA retried mail for a non-existent mailbox for days instead of bouncing it.
func rcptError(email string, err error) error {
	var reject *localdelivery.RejectError
	switch {
	case errors.Is(err, localdelivery.ErrNotLocal):
		log.Printf("MX: Domain of %s not handled", email)
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "Relay access denied - domain not handled"}
	case errors.As(err, &reject):
		log.Printf("MX: Rejecting recipient %s: %s", email, reject.Message)
		return &smtp.SMTPError{
			Code:         reject.Code,
			EnhancedCode: smtp.EnhancedCode{reject.Enhanced[0], reject.Enhanced[1], reject.Enhanced[2]},
			Message:      reject.Message,
		}
	default:
		log.Printf("MX: Recipient lookup for %s failed: %v", email, err)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary failure looking up recipient, try again later"}
	}
}

// Data is called when the client sends the message body
func (s *Session) Data(r io.Reader) error {
	log.Printf("MX: Receiving message from %s to %d recipients", s.from, len(s.recipients))

	if s.from == "" {
		return errors.New("503 No sender specified")
	}

	if len(s.recipients) == 0 {
		return errors.New("503 No valid recipients")
	}

	// Read the entire message
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return fmt.Errorf("failed to read message: %w", err)
	}

	messageData := buf.Bytes()

	// Parse the message using the new parser
	p := parser.New()
	parsed, err := p.ParseBytes(messageData)
	if err != nil {
		log.Printf("MX: Failed to parse message: %v", err)
		// Still try to save with minimal info
		parsed = &parser.ParsedMessage{
			RawData: messageData,
			RawSize: int64(len(messageData)),
		}
	}

	// Every message's identity is (user_id, Message-ID). We never invent one
	// server-side — a message without a client-assigned Message-ID is rejected
	// with a hard 5xx so the sender sees an explicit error.
	if parsed.MessageID == "" {
		log.Printf("MX: REJECT message from %s — no Message-ID header", s.from)
		return errors.New("550 Message rejected: missing Message-ID header")
	}

	fromAddr := parser.FormatAddress(parsed.From)
	if fromAddr == "" {
		fromAddr = s.from
	}

	// Reject messages with empty To header or where no To address matches our mailboxes
	if len(parsed.To) == 0 {
		log.Printf("MX: Rejecting message from %s - empty To header", fromAddr)
		return errors.New("550 Message rejected - no recipients in To header")
	}
	if !s.hasLocalRecipientInHeaders(parsed.To, parsed.Cc) {
		log.Printf("MX: Rejecting message from %s - no valid local address in To/Cc headers (To: %s)", fromAddr, parser.FormatAddressList(parsed.To))
		return errors.New("550 Message rejected - recipient not in To/Cc headers")
	}

	// Log if we found embedded messages or attachments
	if len(parsed.EmbeddedMessages) > 0 {
		log.Printf("MX: Message contains %d embedded message(s)", len(parsed.EmbeddedMessages))
	}
	if len(parsed.Attachments) > 0 {
		log.Printf("MX: Message contains %d attachment(s)", len(parsed.Attachments))
		for _, att := range parsed.Attachments {
			if att.IsDangerous {
				log.Printf("MX: WARNING - Dangerous attachment detected: %s (%s)", att.Filename, att.ContentType)
			}
		}
	}

	env := localdelivery.Envelope{From: s.from, FromDomain: s.fromDomain, SenderIP: s.senderIP}
	results, errs := s.deliverer.Deliver(env, messageData, parsed, s.recipients)

	// A duplicate is a recipient that already has this message: accepting it
	// again is correct (answering 451 would make the sender retry forever).
	accepted := 0
	for i, res := range results {
		if res == localdelivery.Failed {
			log.Printf("MX: Failed to deliver to %s: %v", s.recipients[i].Email, errs[i])
			continue
		}
		accepted++
	}

	if accepted == 0 {
		return errors.New("451 Failed to deliver message to any recipient")
	}

	log.Printf("MX: Message delivered to %d/%d recipients", accepted, len(s.recipients))
	return nil
}

// Reset resets the session state
func (s *Session) Reset() {
	log.Printf("MX: Session reset")
	s.from = ""
	s.recipients = nil
}

// Logout is called when the client logs out
func (s *Session) Logout() error {
	log.Printf("MX: Session logout")
	return nil
}

// extractEmail extracts email address from various formats
func (s *Session) extractEmail(addr string) string {
	addr = strings.TrimSpace(addr)

	// Handle formats like:
	// - "user@example.com"
	// - "Name <user@example.com>"
	// - "<user@example.com>"

	start := strings.Index(addr, "<")
	end := strings.Index(addr, ">")

	if start >= 0 && end > start {
		return strings.TrimSpace(addr[start+1 : end])
	}

	return strings.ToLower(addr)
}

// splitEmail splits email into local part and domain
func (s *Session) splitEmail(email string) (string, string) {
	parts := strings.SplitN(strings.ToLower(email), "@", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

// hasLocalRecipientInHeaders checks if any envelope recipient appears in the To or Cc headers
func (s *Session) hasLocalRecipientInHeaders(to []*mail.Address, cc []*mail.Address) bool {
	headerAddrs := make(map[string]bool)
	for _, addr := range to {
		if addr != nil {
			headerAddrs[strings.ToLower(addr.Address)] = true
		}
	}
	for _, addr := range cc {
		if addr != nil {
			headerAddrs[strings.ToLower(addr.Address)] = true
		}
	}
	for _, rcpt := range s.recipients {
		if headerAddrs[strings.ToLower(rcpt.Email)] {
			return true
		}
	}
	return false
}
