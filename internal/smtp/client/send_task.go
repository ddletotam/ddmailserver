package client

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/logmask"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/parser"
	"github.com/yourusername/mailserver/internal/task"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// maxRetries is the total number of delivery attempts before an outbox
// message is marked failed. IncrementOutboxMessageRetries schedules the
// next attempt with growing backoff (1m → 5m → 15m → 1h → 4h → 12h), so a
// temporarily-refusing MX gets ~17.5 hours of retries, not three attempts
// in as many minutes.
const maxRetries = 6

// SendTask represents an SMTP send task
type SendTask struct {
	outboxMessage *models.OutboxMessage
	account       *models.Account
	database      *db.DB
	priority      int
	messageID     string // Generated Message-ID for Sent dedup
}

// NewSendTask creates a new SMTP send task
func NewSendTask(outboxMessage *models.OutboxMessage, account *models.Account, database *db.DB) *SendTask {
	return &SendTask{
		outboxMessage: outboxMessage,
		account:       account,
		database:      database,
		priority:      1,
	}
}

// Type returns the task type
func (t *SendTask) Type() task.Type {
	return task.TypeSMTP
}

// Priority returns task priority
func (t *SendTask) Priority() int {
	return t.priority
}

// String returns a human-readable description
func (t *SendTask) String() string {
	// Recipient addresses are personal data — mask them in worker logs.
	return fmt.Sprintf("SMTP send message %d from %s to %s",
		t.outboxMessage.ID, logmask.Addr(t.outboxMessage.From), logmask.AddrList(t.outboxMessage.To))
}

// Execute runs the send operation
func (t *SendTask) Execute(ctx context.Context) error {
	log.Printf("Sending message %d via account %s", t.outboxMessage.ID, t.account.Email)

	// Check context cancellation
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Update status to sending
	if err := t.database.UpdateOutboxMessageStatus(t.outboxMessage.ID, "sending", ""); err != nil {
		log.Printf("Failed to update status to sending: %v", err)
	}

	// Create SMTP client
	client := New(t.account)

	// Prepare recipients. Shared with the direct-MX path: both need addr-specs
	// with the display names stripped, and this used to be the one place that
	// handed RCPT TO the whole "Name <addr>" string.
	recipients := parseRecipientsFromOutbox(t.outboxMessage)
	if len(recipients) == 0 {
		// Terminal validation error: without the explicit failed status the
		// message would be stranded in status='sending' forever (the
		// scheduler only picks up 'pending').
		if err := t.database.UpdateOutboxMessageStatus(t.outboxMessage.ID, "failed", "no recipients"); err != nil {
			log.Printf("Failed to mark message as failed: %v", err)
		}
		return fmt.Errorf("no recipients found")
	}

	// Use raw email if available, otherwise construct from fields
	var emailData []byte
	if len(t.outboxMessage.RawEmail) > 0 {
		emailData = t.outboxMessage.RawEmail
		// Extract Message-ID from raw email for Sent dedup
		p := parser.New()
		if parsed, err := p.ParseBytes(emailData); err == nil {
			t.messageID = parsed.GetMessageID()
		}
	} else {
		emailData, t.messageID = constructOutboxEmail(t.database, t.outboxMessage)
	}

	// Send email
	err := client.Send(t.outboxMessage.From, recipients, emailData)

	if err != nil {
		// Increment retries
		if err := t.database.IncrementOutboxMessageRetries(t.outboxMessage.ID, err.Error()); err != nil {
			log.Printf("Failed to increment retries: %v", err)
		}

		// Check if we should mark as failed
		if t.outboxMessage.Retries+1 >= maxRetries {
			log.Printf("Message %d exceeded max retries, marking as failed", t.outboxMessage.ID)
			if err := t.database.UpdateOutboxMessageStatus(t.outboxMessage.ID, "failed", err.Error()); err != nil {
				log.Printf("Failed to mark message as failed: %v", err)
			}
		} else {
			// Mark as pending for retry
			if err := t.database.UpdateOutboxMessageStatus(t.outboxMessage.ID, "pending", err.Error()); err != nil {
				log.Printf("Failed to mark message as pending: %v", err)
			}
		}

		return fmt.Errorf("failed to send: %w", err)
	}

	// Mark as sent
	if err := t.database.MarkOutboxMessageSent(t.outboxMessage.ID); err != nil {
		log.Printf("Failed to mark message as sent: %v", err)
		return err
	}

	// Save to Sent folder
	saveToSentFolder(t.database, t.outboxMessage.UserID, emailData, t.messageID)

	log.Printf("Message %d sent successfully", t.outboxMessage.ID)
	return nil
}

// randomBoundary generates a random MIME boundary string.
func randomBoundary() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("----=_Part_%x", b)
}

// constructOutboxEmail builds an RFC 5322 email from outbox message fields.
// If the database contains outbox_attachments for this message, they are
// included as MIME parts inside a multipart/mixed envelope. Shared by the
// relay (SendTask) and direct-MX (DirectSendTask) paths. Returns the wire
// bytes and the generated Message-ID (used for Sent-folder dedup).
func constructOutboxEmail(database *db.DB, msg *models.OutboxMessage) ([]byte, string) {
	// Load attachments from DB
	var attachments []*models.OutboxAttachment
	if database != nil {
		atts, err := database.GetOutboxAttachmentsByMessageID(msg.ID)
		if err == nil {
			attachments = atts
		}
	}

	var email strings.Builder

	// Generate Message-ID
	domain := "localhost"
	if parts := strings.SplitN(msg.From, "@", 2); len(parts) == 2 {
		// Extract domain from "Name <user@domain>" or "user@domain"
		d := parts[1]
		d = strings.TrimRight(d, ">")
		d = strings.TrimSpace(d)
		if d != "" {
			domain = d
		}
	}
	randBytes := make([]byte, 8)
	rand.Read(randBytes)
	messageID := fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), hex.EncodeToString(randBytes), domain)

	// Headers. Free-text (Subject) and display-names (From/To/Cc) are
	// RFC 2047-encoded so the header section stays 7-bit ASCII — otherwise a
	// Cyrillic Subject forces relays to require SMTPUTF8, which legacy
	// receivers reject (see parser.EncodeHeaderWord).
	email.WriteString(fmt.Sprintf("Message-ID: %s\r\n", messageID))
	email.WriteString(fmt.Sprintf("Date: %s\r\n", time.Now().Format("Mon, 02 Jan 2006 15:04:05 -0700")))
	email.WriteString(fmt.Sprintf("From: %s\r\n", parser.EncodeAddressHeader(msg.From)))
	email.WriteString(fmt.Sprintf("To: %s\r\n", parser.EncodeAddressHeader(msg.To)))
	if msg.Cc != "" {
		email.WriteString(fmt.Sprintf("Cc: %s\r\n", parser.EncodeAddressHeader(msg.Cc)))
	}
	if msg.Subject != "" {
		email.WriteString(fmt.Sprintf("Subject: %s\r\n", parser.EncodeHeaderWord(msg.Subject)))
	}
	email.WriteString("MIME-Version: 1.0\r\n")

	// Build body part
	var bodyBuf strings.Builder
	hasPlain := msg.Body != ""
	hasHTML := msg.BodyHTML != ""

	if hasPlain && hasHTML {
		altBoundary := randomBoundary()
		bodyBuf.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", altBoundary))
		bodyBuf.WriteString(fmt.Sprintf("--%s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", altBoundary, msg.Body))
		bodyBuf.WriteString(fmt.Sprintf("--%s\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s\r\n", altBoundary, msg.BodyHTML))
		bodyBuf.WriteString(fmt.Sprintf("--%s--\r\n", altBoundary))
	} else if hasHTML {
		bodyBuf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		bodyBuf.WriteString(msg.BodyHTML)
	} else {
		bodyBuf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		bodyBuf.WriteString(msg.Body)
	}

	if len(attachments) > 0 {
		mixedBoundary := randomBoundary()
		email.WriteString(fmt.Sprintf("Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n", mixedBoundary))

		// Body part
		email.WriteString(fmt.Sprintf("--%s\r\n%s\r\n", mixedBoundary, bodyBuf.String()))

		// Attachment parts
		for _, att := range attachments {
			email.WriteString(fmt.Sprintf("--%s\r\n", mixedBoundary))
			email.WriteString(fmt.Sprintf("Content-Type: %s; name=\"%s\"\r\n", att.ContentType, att.Filename))
			email.WriteString("Content-Transfer-Encoding: base64\r\n")
			email.WriteString(fmt.Sprintf("Content-Disposition: attachment; filename=\"%s\"\r\n\r\n", att.Filename))
			encoded := base64.StdEncoding.EncodeToString(att.Data)
			for i := 0; i < len(encoded); i += 76 {
				end := i + 76
				if end > len(encoded) {
					end = len(encoded)
				}
				email.WriteString(encoded[i:end])
				email.WriteString("\r\n")
			}
		}
		email.WriteString(fmt.Sprintf("--%s--\r\n", mixedBoundary))
	} else {
		// No attachments — write body directly
		email.WriteString(bodyBuf.String())
	}

	return []byte(email.String()), messageID
}

// saveToSentFolder saves a copy of a sent email to the user's Sent folder
func saveToSentFolder(database *db.DB, userID int64, emailData []byte, messageID string) {
	sentFolder, err := database.GetLocalFolderByType(userID, "sent")
	if err != nil || sentFolder == nil {
		log.Printf("saveToSentFolder: no Sent folder for user %d", userID)
		return
	}

	// Check dedup
	exists, err := database.MessageExistsInFolder(sentFolder.ID, messageID)
	if err == nil && exists {
		log.Printf("saveToSentFolder: message %s already in Sent, skipping", messageID)
		return
	}

	// Parse the email
	p := parser.New()
	parsed, err := p.ParseBytes(emailData)
	if err != nil {
		log.Printf("saveToSentFolder: failed to parse email: %v", err)
		return
	}

	// Get next UID
	nextUID, err := database.GetNextUIDForFolder(sentFolder.ID)
	if err != nil {
		log.Printf("saveToSentFolder: failed to get UID: %v", err)
		return
	}

	// Convert addresses to strings
	fromStr := ""
	if parsed.From != nil {
		fromStr = parsed.From.String()
	}
	toStrs := make([]string, 0, len(parsed.To))
	for _, a := range parsed.To {
		toStrs = append(toStrs, a.String())
	}
	ccStrs := make([]string, 0, len(parsed.Cc))
	for _, a := range parsed.Cc {
		ccStrs = append(ccStrs, a.String())
	}

	msg := &models.Message{
		UserID:    userID,
		FolderID:  sentFolder.ID,
		MessageID: parsed.GetMessageID(),
		Subject:   parsed.Subject,
		From:      fromStr,
		To:        strings.Join(toStrs, ", "),
		Cc:        strings.Join(ccStrs, ", "),
		Date:      timeutil.ToMs(parsed.Date),
		Body:      parsed.Body,
		BodyHTML:  parsed.BodyHTML,
		RawEmail:  emailData, // store the real RFC-822 so View Source isn't a stitched fake
		Size:      int64(len(emailData)),
		UID:       nextUID,
		Seen:      true, // Sent messages are always read
		CreatedAt: timeutil.Now(),
		UpdatedAt: timeutil.Now(),
	}

	if err := database.CreateMessage(msg); err != nil {
		log.Printf("saveToSentFolder: failed to save to Sent: %v", err)
		return
	}

	// Save attachments (including inline images with Content-ID)
	for _, att := range parsed.Attachments {
		contentID := strings.Trim(att.ContentID, "<>")
		attachment := &models.Attachment{
			MessageID:   msg.ID,
			ContentID:   contentID,
			Filename:    att.Filename,
			ContentType: att.ContentType,
			Size:        int(att.Size),
			IsInline:    att.IsInline,
			Data:        att.Data,
		}
		if err := database.CreateAttachment(attachment); err != nil {
			log.Printf("saveToSentFolder: failed to save attachment %s: %v", att.Filename, err)
		}
	}
	if len(parsed.Attachments) > 0 {
		database.UpdateMessageAttachmentCount(msg.ID, len(parsed.Attachments))
	}

	log.Printf("saveToSentFolder: saved message %s to Sent folder (attachments: %d)", messageID, len(parsed.Attachments))
}
