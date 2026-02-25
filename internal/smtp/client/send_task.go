package client

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/task"
)

const maxRetries = 3

// SendTask represents an SMTP send task
type SendTask struct {
	outboxMessage *models.OutboxMessage
	account       *models.Account
	database      *db.DB
	priority      int
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

// constructEmail builds an RFC 5322 email from message fields
func (t *SendTask) constructEmail() []byte {
	var email strings.Builder

	// Headers
	email.WriteString(fmt.Sprintf("From: %s\r\n", t.outboxMessage.From))
	email.WriteString(fmt.Sprintf("To: %s\r\n", t.outboxMessage.To))

	if t.outboxMessage.Cc != "" {
		email.WriteString(fmt.Sprintf("Cc: %s\r\n", t.outboxMessage.Cc))
	}

	if t.outboxMessage.Subject != "" {
		email.WriteString(fmt.Sprintf("Subject: %s\r\n", t.outboxMessage.Subject))
	}

	email.WriteString("MIME-Version: 1.0\r\n")

	// Body
	if t.outboxMessage.BodyHTML != "" && t.outboxMessage.Body != "" {
		// Multipart: both HTML and plain text
		boundary := "boundary-mailserver-12345"
		email.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary))
		email.WriteString("\r\n")

		// Plain text part
		email.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		email.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		email.WriteString("\r\n")
		email.WriteString(t.outboxMessage.Body)
		email.WriteString("\r\n\r\n")

		// HTML part
		email.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		email.WriteString("Content-Type: text/html; charset=utf-8\r\n")
		email.WriteString("\r\n")
		email.WriteString(t.outboxMessage.BodyHTML)
		email.WriteString("\r\n\r\n")

		email.WriteString(fmt.Sprintf("--%s--\r\n", boundary))
	} else if t.outboxMessage.BodyHTML != "" {
		// HTML only
		email.WriteString("Content-Type: text/html; charset=utf-8\r\n")
		email.WriteString("\r\n")
		email.WriteString(t.outboxMessage.BodyHTML)
	} else {
		// Plain text only
		email.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		email.WriteString("\r\n")
		email.WriteString(t.outboxMessage.Body)
	}

	return []byte(email.String())
}
