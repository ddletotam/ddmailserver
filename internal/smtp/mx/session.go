package mx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/emersion/go-smtp"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/notify"
	"github.com/yourusername/mailserver/internal/parser"
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
	database            *db.DB
	hub                 *notify.Hub
	conn                *smtp.Conn
	analyzer            *parser.Analyzer
	calendarSyncTrigger func(userID int64)
	from                string
	fromDomain          string
	senderIP            string
	recipients          []*Recipient
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

	// Parse the message using the new parser
	p := parser.New()
	parsed, err := p.ParseBytes(messageData)
	if err != nil {
		log.Printf("MX: Failed to parse message: %v", err)
		// Still try to save with minimal info
		parsed = &parser.ParsedMessage{
			RawData: messageData,
			RawSize: messageSize,
		}
	}

	// Extract values from parsed message
	subject := parsed.Subject
	messageID := parsed.GetMessageID()
	messageDate := parsed.GetDate()
	inReplyTo := parsed.InReplyTo
	references := strings.Join(parsed.References, " ")

	fromAddr := parser.FormatAddress(parsed.From)
	toAddr := parser.FormatAddressList(parsed.To)
	ccAddr := parser.FormatAddressList(parsed.Cc)
	replyTo := parser.FormatAddress(parsed.ReplyTo)

	// Use envelope from if header from is empty
	if fromAddr == "" {
		fromAddr = s.from
	}

	body := parsed.Body
	bodyHTML := parsed.BodyHTML

	// Run spam analysis with sender context
	if s.analyzer != nil {
		s.analyzer.AnalyzeWithContext(parsed, s.senderIP, s.fromDomain)
		if parsed.SpamScore > 0 {
			log.Printf("MX: Spam analysis - score=%.1f status=%s reasons=%v",
				parsed.SpamScore, parsed.SpamStatus, parsed.SpamReasons)
		}
		// Log auth results if available
		if parsed.AuthResults != nil {
			log.Printf("MX: Auth results - SPF=%s DKIM=%s",
				parsed.AuthResults.SPF, parsed.AuthResults.DKIM)
		}
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

	// Save message for each recipient
	savedCount := 0
	for _, recipient := range s.recipients {
		// Find or create INBOX folder for the user
		folderID, err := s.getOrCreateInbox(recipient.Mailbox.UserID)
		if err != nil {
			log.Printf("MX: Failed to get inbox for user %d: %v", recipient.Mailbox.UserID, err)
			continue
		}

		// Get next UID atomically for this folder
		nextUID, err := s.database.GetNextUIDForFolder(folderID)
		if err != nil {
			log.Printf("MX: Failed to get next UID for folder %d: %v", folderID, err)
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
			UID:               nextUID,
			Seen:              false,
			Flagged:           false,
			Answered:          false,
			Draft:             false,
			Deleted:           false,
			InReplyTo:         inReplyTo,
			MessageReferences: references,
			SpamScore:         parsed.SpamScore,
			SpamStatus:        string(parsed.SpamStatus),
			SpamReasons:       parser.GetSpamReasonsJSON(parsed.SpamReasons),
		}

		// Save to database
		if err := s.database.CreateMessage(msg); err != nil {
			log.Printf("MX: Failed to save message for %s: %v", recipient.Email, err)
			continue
		}

		// Save attachments
		for _, att := range parsed.Attachments {
			attachment := &models.Attachment{
				MessageID:   msg.ID,
				Filename:    att.Filename,
				ContentType: att.ContentType,
				Size:        att.Size,
				Data:        att.Data,
			}
			if err := s.database.CreateAttachment(attachment); err != nil {
				log.Printf("MX: Failed to save attachment %s: %v", att.Filename, err)
			}
		}

		savedCount++
		log.Printf("MX: Message %d saved for %s (user_id=%d, folder_id=%d)",
			msg.ID, recipient.Email, recipient.Mailbox.UserID, folderID)

		// Publish notification for IMAP IDLE clients
		if s.hub != nil {
			// Get message count and username for IMAP update
			count, _ := s.database.GetMessageCountByFolder(folderID)
			user, _ := s.database.GetUserByID(recipient.Mailbox.UserID)
			username := ""
			if user != nil {
				username = user.Username
			}

			s.hub.Publish(notify.Event{
				UserID:   recipient.Mailbox.UserID,
				FolderID: folderID,
				Type:     notify.EventNewMessage,
				Count:    count,
				Username: username,
				Mailbox:  "INBOX",
			})
		}

		// Trigger calendar sync if message contains calendar invite (.ics)
		if s.calendarSyncTrigger != nil && s.hasCalendarInvite(parsed) {
			log.Printf("MX: Calendar invite detected, triggering sync for user %d", recipient.Mailbox.UserID)
			go s.calendarSyncTrigger(recipient.Mailbox.UserID)
		}
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
	folder, err := s.database.GetOrCreateLocalInbox(userID)
	if err != nil {
		return 0, fmt.Errorf("failed to get/create local inbox: %w", err)
	}
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

// hasCalendarInvite checks if the message contains a calendar invite (.ics attachment)
func (s *Session) hasCalendarInvite(parsed *parser.ParsedMessage) bool {
	if parsed == nil {
		return false
	}

	for _, att := range parsed.Attachments {
		// Check content type
		contentType := strings.ToLower(att.ContentType)
		if strings.Contains(contentType, "text/calendar") ||
			strings.Contains(contentType, "application/ics") {
			return true
		}

		// Check filename extension
		filename := strings.ToLower(att.Filename)
		if strings.HasSuffix(filename, ".ics") {
			return true
		}
	}

	return false
}
