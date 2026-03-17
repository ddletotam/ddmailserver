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

// DirectSendTask sends email directly via MX lookup (for local domain senders)
type DirectSendTask struct {
	outboxMessage *models.OutboxMessage
	database      *db.DB
	hostname      string
	priority      int
}

// NewDirectSendTask creates a new direct send task
func NewDirectSendTask(outboxMessage *models.OutboxMessage, database *db.DB, hostname string) *DirectSendTask {
	return &DirectSendTask{
		outboxMessage: outboxMessage,
		database:      database,
		hostname:      hostname,
		priority:      1,
	}
}

func (t *DirectSendTask) Type() task.Type {
	return task.TypeSMTP
}

func (t *DirectSendTask) Priority() int {
	return t.priority
}

func (t *DirectSendTask) String() string {
	return fmt.Sprintf("Direct send message %d from %s to %s", t.outboxMessage.ID, t.outboxMessage.From, t.outboxMessage.To)
}

func (t *DirectSendTask) Execute(ctx context.Context) error {
	log.Printf("Direct sending message %d from %s", t.outboxMessage.ID, t.outboxMessage.From)

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if err := t.database.UpdateOutboxMessageStatus(t.outboxMessage.ID, "sending", ""); err != nil {
		log.Printf("Failed to update status to sending: %v", err)
	}

	recipients := parseRecipientsFromOutbox(t.outboxMessage)
	if len(recipients) == 0 {
		return fmt.Errorf("no recipients found")
	}

	var emailData []byte
	if len(t.outboxMessage.RawEmail) > 0 {
		emailData = t.outboxMessage.RawEmail
	} else {
		return fmt.Errorf("no raw email data for direct delivery")
	}

	err := SendDirect(t.outboxMessage.From, recipients, emailData, t.hostname)

	if err != nil {
		if err := t.database.IncrementOutboxMessageRetries(t.outboxMessage.ID, err.Error()); err != nil {
			log.Printf("Failed to increment retries: %v", err)
		}

		if t.outboxMessage.Retries+1 >= maxRetries {
			log.Printf("Message %d exceeded max retries, marking as failed", t.outboxMessage.ID)
			if err := t.database.UpdateOutboxMessageStatus(t.outboxMessage.ID, "failed", err.Error()); err != nil {
				log.Printf("Failed to mark message as failed: %v", err)
			}
		} else {
			if err := t.database.UpdateOutboxMessageStatus(t.outboxMessage.ID, "pending", err.Error()); err != nil {
				log.Printf("Failed to mark message as pending: %v", err)
			}
		}

		return fmt.Errorf("direct send failed: %w", err)
	}

	if err := t.database.MarkOutboxMessageSent(t.outboxMessage.ID); err != nil {
		log.Printf("Failed to mark message as sent: %v", err)
		return err
	}

	log.Printf("Message %d sent directly via MX", t.outboxMessage.ID)
	return nil
}

func parseRecipientsFromOutbox(msg *models.OutboxMessage) []string {
	var recipients []string
	for _, addr := range splitAddresses(msg.To) {
		if addr != "" {
			recipients = append(recipients, addr)
		}
	}
	for _, addr := range splitAddresses(msg.Cc) {
		if addr != "" {
			recipients = append(recipients, addr)
		}
	}
	for _, addr := range splitAddresses(msg.Bcc) {
		if addr != "" {
			recipients = append(recipients, addr)
		}
	}
	return recipients
}

func splitAddresses(s string) []string {
	if s == "" {
		return nil
	}
	var addrs []string
	for _, part := range splitComma(s) {
		email := extractEmailAddr(part)
		if email != "" {
			addrs = append(addrs, email)
		}
	}
	return addrs
}

func splitComma(s string) []string {
	var parts []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

func extractEmailAddr(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "<"); idx != -1 {
		end := strings.Index(s, ">")
		if end > idx {
			return s[idx+1 : end]
		}
	}
	if strings.Contains(s, "@") {
		return s
	}
	return ""
}
