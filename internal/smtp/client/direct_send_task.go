package client

import (
	"context"
	"fmt"
	"log"
	"net/mail"
	"strings"

	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/dkimsign"
	"github.com/yourusername/mailserver/internal/logmask"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/parser"
	"github.com/yourusername/mailserver/internal/task"
)

// DirectSendTask sends email directly via MX lookup (for local domain senders)
type DirectSendTask struct {
	outboxMessage *models.OutboxMessage
	database      *db.DB
	hostname      string
	signer        *dkimsign.Signer // nil → send unsigned
	priority      int
	messageID     string // Message-ID for Sent dedup (from raw or generated)

	// notifySent fires once the copy is in Sent — see SendTask.SetSentNotifyFunc.
	notifySent func()
}

// SetSentNotifyFunc registers the «copy is in Sent» callback.
func (t *DirectSendTask) SetSentNotifyFunc(fn func()) {
	t.notifySent = fn
}

// NewDirectSendTask creates a new direct send task
func NewDirectSendTask(outboxMessage *models.OutboxMessage, database *db.DB, hostname string, signer *dkimsign.Signer) *DirectSendTask {
	return &DirectSendTask{
		outboxMessage: outboxMessage,
		database:      database,
		hostname:      hostname,
		signer:        signer,
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
	// Recipient addresses are personal data — mask them in worker logs.
	return fmt.Sprintf("Direct send message %d from %s to %s",
		t.outboxMessage.ID, logmask.Addr(t.outboxMessage.From), logmask.AddrList(t.outboxMessage.To))
}

func (t *DirectSendTask) Execute(ctx context.Context) error {
	log.Printf("Direct sending message %d from %s", t.outboxMessage.ID, logmask.Addr(t.outboxMessage.From))

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if err := t.database.UpdateOutboxMessageStatus(t.outboxMessage.ID, "sending", ""); err != nil {
		log.Printf("Failed to update status to sending: %v", err)
	}

	recipients := parseRecipientsFromOutbox(t.outboxMessage)
	if len(recipients) == 0 {
		// Terminal validation error: mark failed explicitly, otherwise the
		// row is stranded in status='sending' forever (the scheduler only
		// picks up 'pending').
		if err := t.database.UpdateOutboxMessageStatus(t.outboxMessage.ID, "failed", "no recipients"); err != nil {
			log.Printf("Failed to mark message as failed: %v", err)
		}
		return fmt.Errorf("no recipients found")
	}

	var emailData []byte
	if len(t.outboxMessage.RawEmail) > 0 {
		emailData = t.outboxMessage.RawEmail
		// Extract Message-ID from raw email for Sent dedup
		p := parser.New()
		if parsed, err := p.ParseBytes(emailData); err == nil {
			t.messageID = parsed.GetMessageID()
		}
	} else {
		// Плоское письмо (без threading-заголовков и вложений) приходит из
		// desktop-хендлера без raw_email — собираем RFC 5322 из полей, как
		// делает relay-путь. Раньше эта ветка возвращала ошибку ДО обновления
		// статуса, и сообщение застревало в status='sending' навсегда.
		emailData, t.messageID = constructOutboxEmail(t.database, t.outboxMessage)
	}

	// DKIM: sign with the From-domain key when one is configured. Without
	// the signature Gmail/Yandex either junk or reject direct delivery.
	emailData = t.signer.Sign(t.outboxMessage.From, emailData)

	err := SendDirect(t.outboxMessage.From, recipients, emailData, t.hostname)

	if err != nil {
		// The stored count decides when to give up: t.outboxMessage.Retries is
		// a copy taken when the task was created.
		retries, incErr := t.database.IncrementOutboxMessageRetries(t.outboxMessage.ID, err.Error())
		if incErr != nil {
			log.Printf("Failed to increment retries: %v", incErr)
			retries = t.outboxMessage.Retries + 1
		}

		if retries >= maxRetries {
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

	// Save to Sent folder — без копии диалог, начатый пользователем,
	// не существует в списке бесед, пока собеседник не ответит
	// (relay-путь через SendTask делает то же самое).
	saveToSentFolder(t.database, t.outboxMessage.UserID, emailData, t.messageID)
	if t.notifySent != nil {
		t.notifySent()
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

// splitAddresses reduces a header-style address list to the bare addr-specs the
// SMTP envelope takes. Shared by both send paths — the relay (SendTask) and
// direct MX (DirectSendTask).
//
// RCPT TO carries an address and nothing else. net/smtp writes what it is given
// straight into `RCPT TO:<%s>`, so a display name surviving this far produces
// nested angle brackets, and with a non-ASCII name a non-ASCII envelope on top
// of that; receivers answer 555 5.5.2 Syntax error, as they should.
//
// mail.ParseAddressList goes first because it is the only thing here that gets
// quoting right: a display name may legitimately contain a comma, and splitting
// on commas alone turns `"Doe, John" <j@d>` into two recipients that do not
// exist. It is all-or-nothing, though, so one malformed entry falls back to the
// naive split rather than costing every recipient in the list.
func splitAddresses(s string) []string {
	if s == "" {
		return nil
	}

	if parsed, err := mail.ParseAddressList(s); err == nil {
		addrs := make([]string, 0, len(parsed))
		for _, a := range parsed {
			if a.Address != "" {
				addrs = append(addrs, a.Address)
			}
		}
		if len(addrs) > 0 {
			return addrs
		}
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
		// Search for the closing bracket after the opening one, not from the
		// start of the string.
		if end := strings.Index(s[idx:], ">"); end > 0 {
			return strings.TrimSpace(s[idx+1 : idx+end])
		}
	}
	if strings.Contains(s, "@") {
		return s
	}
	return ""
}
