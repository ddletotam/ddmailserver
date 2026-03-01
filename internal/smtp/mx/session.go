package mx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-smtp"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
)

// Recipient holds info about a validated recipient
type Recipient struct {
	Email     string
	Mailbox   *models.Mailbox
	Domain    *models.Domain
	LocalPart string
}

// Session represents an MX SMTP session
type Session struct {
	database   *db.DB
	conn       *smtp.Conn
	from       string
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
	return nil
}

// Rcpt is called to set a recipient (RCPT TO)
// This is where we validate if we accept mail for this address
func (s *Session) Rcpt(to string, opts *smtp.RcptOptions) error {
	log.Printf("MX: RCPT TO: %s", to)

	// Extract email address
	email := s.extractEmail(to)
	if email == "" {
		return errors.New("550 Invalid recipient address")
	}

	// Split into local part and domain
	localPart, domainName := s.splitEmail(email)
	if localPart == "" || domainName == "" {
		return errors.New("550 Invalid recipient address format")
	}

	// Check if we handle this domain
	domain, err := s.database.GetDomainByName(domainName)
	if err != nil {
		log.Printf("MX: Domain %s not found: %v", domainName, err)
		return errors.New("550 Relay access denied - domain not handled")
	}

	if !domain.Enabled {
		log.Printf("MX: Domain %s is disabled", domainName)
		return errors.New("550 Relay access denied - domain disabled")
	}

	// Check if mailbox exists
	mailbox, err := s.database.GetMailbox(domain.ID, localPart)
	if err != nil {
		log.Printf("MX: Mailbox %s@%s not found: %v", localPart, domainName, err)
		return errors.New("550 No such user here")
	}

	if !mailbox.Enabled {
		log.Printf("MX: Mailbox %s@%s is disabled", localPart, domainName)
		return errors.New("550 Mailbox disabled")
	}

	// Add to recipients list
	s.recipients = append(s.recipients, &Recipient{
		Email:     email,
		Mailbox:   mailbox,
		Domain:    domain,
		LocalPart: localPart,
	})

	log.Printf("MX: Accepted recipient %s (user_id=%d)", email, mailbox.UserID)
	return nil
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
	messageSize := int64(len(messageData))

	// Parse the message to extract headers
	mr, err := mail.CreateReader(bytes.NewReader(messageData))
	if err != nil {
		log.Printf("MX: Failed to parse message: %v", err)
		// Still try to save raw message
	}

	var subject, fromAddr, toAddr, ccAddr, replyTo, messageID, inReplyTo, references string
	var messageDate time.Time

	if mr != nil {
		header := mr.Header

		subject, _ = header.Subject()
		messageDate, _ = header.Date()
		messageID, _ = header.MessageID()
		inReplyToList, _ := header.MsgIDList("In-Reply-To")
		if len(inReplyToList) > 0 {
			inReplyTo = inReplyToList[0]
		}
		refs, _ := header.MsgIDList("References")
		references = strings.Join(refs, " ")

		if fromList, err := header.AddressList("From"); err == nil && len(fromList) > 0 {
			fromAddr = s.formatAddress(fromList[0])
		}
		if toList, err := header.AddressList("To"); err == nil {
			toAddr = s.formatAddressList(toList)
		}
		if ccList, err := header.AddressList("Cc"); err == nil {
			ccAddr = s.formatAddressList(ccList)
		}
		if replyToList, err := header.AddressList("Reply-To"); err == nil && len(replyToList) > 0 {
			replyTo = s.formatAddress(replyToList[0])
		}
	}

	// Use envelope from if header from is empty
	if fromAddr == "" {
		fromAddr = s.from
	}

	// Use current time if date not parsed
	if messageDate.IsZero() {
		messageDate = time.Now()
	}

	// Generate message ID if missing
	if messageID == "" {
		messageID = fmt.Sprintf("<%d.%d@local>", time.Now().UnixNano(), len(s.recipients))
	}

	// Extract body
	var body, bodyHTML string
	if mr != nil {
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Printf("MX: Error reading part: %v", err)
				break
			}

			switch h := p.Header.(type) {
			case *mail.InlineHeader:
				contentType, _, _ := h.ContentType()
				bodyBytes, _ := io.ReadAll(p.Body)

				if contentType == "text/plain" && body == "" {
					body = string(bodyBytes)
				} else if contentType == "text/html" && bodyHTML == "" {
					bodyHTML = string(bodyBytes)
				}
			}
		}
	}

	// Save message for each recipient
	savedCount := 0
	for _, recipient := range s.recipients {
		// Find or create INBOX folder for the user
		folderID, err := s.getOrCreateInbox(recipient.Mailbox.UserID)
		if err != nil {
			log.Printf("MX: Failed to get inbox for user %d: %v", recipient.Mailbox.UserID, err)
			continue
		}

		// Create message
		msg := &models.Message{
			AccountID:         0, // No external account - local delivery
			UserID:            recipient.Mailbox.UserID,
			FolderID:          folderID,
			MessageID:         messageID,
			Subject:           subject,
			From:              fromAddr,
			To:                toAddr,
			Cc:                ccAddr,
			ReplyTo:           replyTo,
			Date:              messageDate,
			Body:              body,
			BodyHTML:          bodyHTML,
			Size:              messageSize,
			UID:               0, // Will be assigned
			Seen:              false,
			Flagged:           false,
			Answered:          false,
			Draft:             false,
			Deleted:           false,
			InReplyTo:         inReplyTo,
			MessageReferences: references,
		}

		// Save to database
		if err := s.database.CreateMessage(msg); err != nil {
			log.Printf("MX: Failed to save message for %s: %v", recipient.Email, err)
			continue
		}

		savedCount++
		log.Printf("MX: Message %d saved for %s (user_id=%d, folder_id=%d)",
			msg.ID, recipient.Email, recipient.Mailbox.UserID, folderID)
	}

	if savedCount == 0 {
		return errors.New("451 Failed to deliver message to any recipient")
	}

	log.Printf("MX: Message delivered to %d/%d recipients", savedCount, len(s.recipients))
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

// getOrCreateInbox finds or creates an INBOX folder for the user
func (s *Session) getOrCreateInbox(userID int64) (int64, error) {
	// Try to find existing INBOX for local delivery (account_id = 0)
	folders, err := s.database.GetFoldersByUser(userID)
	if err != nil {
		return 0, fmt.Errorf("failed to get folders: %w", err)
	}

	// Look for INBOX with account_id = 0 (local)
	for _, f := range folders {
		if f.AccountID == 0 && f.Type == "inbox" {
			return f.ID, nil
		}
	}

	// Create local INBOX folder
	folder := &models.Folder{
		UserID:    userID,
		AccountID: 0, // Local delivery
		Name:      "INBOX",
		Path:      "INBOX",
		Type:      "inbox",
	}

	if err := s.database.CreateFolder(folder); err != nil {
		return 0, fmt.Errorf("failed to create inbox: %w", err)
	}

	log.Printf("MX: Created local INBOX folder %d for user %d", folder.ID, userID)
	return folder.ID, nil
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

// formatAddress formats a mail address
func (s *Session) formatAddress(addr *mail.Address) string {
	if addr == nil {
		return ""
	}
	if addr.Name != "" {
		return fmt.Sprintf("%s <%s>", addr.Name, addr.Address)
	}
	return addr.Address
}

// formatAddressList formats a list of mail addresses
func (s *Session) formatAddressList(addrs []*mail.Address) string {
	if len(addrs) == 0 {
		return ""
	}
	var result []string
	for _, addr := range addrs {
		result = append(result, s.formatAddress(addr))
	}
	return strings.Join(result, ", ")
}
