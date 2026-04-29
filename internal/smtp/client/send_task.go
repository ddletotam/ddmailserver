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
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/parser"
	"github.com/yourusername/mailserver/internal/task"
)

const maxRetries = 3

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
	return fmt.Sprintf("SMTP send message %d from %s to %s", t.outboxMessage.ID, t.outboxMessage.From, t.outboxMessage.To)
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

	// Prepare recipients
	recipients := t.parseRecipients()
	if len(recipients) == 0 {
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
		emailData = t.constructEmail()
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

// parseRecipients extracts all recipient email addresses
func (t *SendTask) parseRecipients() []string {
	var recipients []string

	// Add To recipients
	if t.outboxMessage.To != "" {
		recipients = append(recipients, t.splitEmails(t.outboxMessage.To)...)
	}

	// Add Cc recipients
	if t.outboxMessage.Cc != "" {
		recipients = append(recipients, t.splitEmails(t.outboxMessage.Cc)...)
	}

	// Add Bcc recipients
	if t.outboxMessage.Bcc != "" {
		recipients = append(recipients, t.splitEmails(t.outboxMessage.Bcc)...)
	}

	return recipients
}

// splitEmails splits a comma-separated list of emails
func (t *SendTask) splitEmails(emails string) []string {
	parts := strings.Split(emails, ",")
	var result []string
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// randomBoundary generates a random MIME boundary string.
func randomBoundary() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("----=_Part_%x", b)
}

// constructEmail builds an RFC 5322 email from message fields.
// If the database contains outbox_attachments for this message, they are
// included as MIME parts inside a multipart/mixed envelope.
func (t *SendTask) constructEmail() []byte {
	// Load attachments from DB
	var attachments []*models.OutboxAttachment
	if t.database != nil {
		atts, err := t.database.GetOutboxAttachmentsByMessageID(t.outboxMessage.ID)
		if err == nil {
			attachments = atts
		}
	}

	var email strings.Builder

	// Generate Message-ID
	domain := "localhost"
	if parts := strings.SplitN(t.outboxMessage.From, "@", 2); len(parts) == 2 {
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
	t.messageID = fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), hex.EncodeToString(randBytes), domain)

	// Headers
	email.WriteString(fmt.Sprintf("Message-ID: %s\r\n", t.messageID))
	email.WriteString(fmt.Sprintf("Date: %s\r\n", time.Now().Format("Mon, 02 Jan 2006 15:04:05 -0700")))
	email.WriteString(fmt.Sprintf("From: %s\r\n", t.outboxMessage.From))
	email.WriteString(fmt.Sprintf("To: %s\r\n", t.outboxMessage.To))
	if t.outboxMessage.Cc != "" {
		email.WriteString(fmt.Sprintf("Cc: %s\r\n", t.outboxMessage.Cc))
	}
	if t.outboxMessage.Subject != "" {
		email.WriteString(fmt.Sprintf("Subject: %s\r\n", t.outboxMessage.Subject))
	}
	email.WriteString("MIME-Version: 1.0\r\n")

	// Build body part
	var bodyBuf strings.Builder
	hasPlain := t.outboxMessage.Body != ""
	hasHTML := t.outboxMessage.BodyHTML != ""

	if hasPlain && hasHTML {
		altBoundary := randomBoundary()
		bodyBuf.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", altBoundary))
		bodyBuf.WriteString(fmt.Sprintf("--%s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", altBoundary, t.outboxMessage.Body))
		bodyBuf.WriteString(fmt.Sprintf("--%s\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s\r\n", altBoundary, t.outboxMessage.BodyHTML))
		bodyBuf.WriteString(fmt.Sprintf("--%s--\r\n", altBoundary))
	} else if hasHTML {
		bodyBuf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		bodyBuf.WriteString(t.outboxMessage.BodyHTML)
	} else {
		bodyBuf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		bodyBuf.WriteString(t.outboxMessage.Body)
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

	return []byte(email.String())
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
		Date:      parsed.Date,
		Body:      parsed.Body,
		BodyHTML:  parsed.BodyHTML,
		Size:      int64(len(emailData)),
		UID:       nextUID,
		Seen:      true, // Sent messages are always read
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if err := database.CreateMessage(msg); err != nil {
		log.Printf("saveToSentFolder: failed to save to Sent: %v", err)
		return
	}

	log.Printf("saveToSentFolder: saved message %s to Sent folder", messageID)
}
