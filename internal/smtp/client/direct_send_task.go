package client

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/mail"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/dkimsign"
	"github.com/ddletotam/ddmailserver/internal/logmask"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/parser"
	"github.com/ddletotam/ddmailserver/internal/smtp/localdelivery"
	"github.com/ddletotam/ddmailserver/internal/task"
)

// outboxStore is what DirectSendTask needs from the outbox table.
type outboxStore interface {
	ClaimOutboxMessage(id int64) (bool, error)
	UpdateOutboxMessageStatus(id int64, status string, lastError string) error
	IncrementOutboxMessageRetries(id int64, lastError string) (int, error)
	MarkOutboxMessageSent(id int64) error
}

// LocalDeliverer delivers into mailboxes of domains hosted on this server
// (localdelivery.Deliverer).
type LocalDeliverer interface {
	Lookup(email string) (*localdelivery.Recipient, error)
	Deliver(env localdelivery.Envelope, raw []byte, parsed *parser.ParsedMessage, rcpts []*localdelivery.Recipient) ([]localdelivery.Result, []error)
}

// DirectSendTask delivers a message sent from a local-domain mailbox: to
// recipients on our own domains in-process, to everyone else via their MX.
type DirectSendTask struct {
	outboxMessage *models.OutboxMessage
	database      *db.DB
	store         outboxStore
	hostname      string
	signer        *dkimsign.Signer // nil → send unsigned
	priority      int
	messageID     string // Message-ID for Sent dedup (from raw or generated)

	// local delivers to our own domains; nil sends everything through MX
	// (which, for our own domains, dials ourselves and fails behind NAT).
	local LocalDeliverer
	// sendExternal is SendDirectEach (replaceable in tests).
	sendExternal func(from string, to []string, msg []byte, hostname string) map[string]error
	// saveSent stores the sender's copy in Sent (replaceable in tests).
	saveSent func(userID int64, emailData []byte, messageID string)
	// now is the clock for DSN dates (tests).
	now func() time.Time

	// notifySent fires once the copy is in Sent — see SendTask.SetSentNotifyFunc.
	notifySent func()
}

// SetSentNotifyFunc registers the «copy is in Sent» callback.
func (t *DirectSendTask) SetSentNotifyFunc(fn func()) {
	t.notifySent = fn
}

// SetLocalDeliverer enables in-process delivery to the domains this server
// hosts, and bounces (DSN) into the sender's mailbox.
func (t *DirectSendTask) SetLocalDeliverer(d LocalDeliverer) {
	t.local = d
}

// NewDirectSendTask creates a new direct send task
func NewDirectSendTask(outboxMessage *models.OutboxMessage, database *db.DB, hostname string, signer *dkimsign.Signer) *DirectSendTask {
	t := &DirectSendTask{
		outboxMessage: outboxMessage,
		database:      database,
		hostname:      hostname,
		signer:        signer,
		priority:      1,
		sendExternal:  SendDirectEach,
		now:           time.Now,
	}
	if database != nil {
		t.store = database
		t.saveSent = func(userID int64, emailData []byte, messageID string) {
			saveToSentFolder(database, userID, emailData, messageID)
		}
	}
	return t
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

// pendingRecipient is a recipient whose delivery failed temporarily.
type pendingRecipient struct {
	address string
	err     error
}

// routing is where each recipient of a message goes.
type routing struct {
	local    []*localdelivery.Recipient // our domains, another user's mailbox
	self     int                        // our domains, the sender's own user: the Sent copy is theirs
	external []string                   // other domains: via MX
	failed   []FailedRecipient          // permanently undeliverable
	pending  []pendingRecipient         // could not be routed now (DB error)
}

// routeRecipients sorts the recipients of a message sent by senderUserID.
// Duplicates (the same address in To and Cc) are routed once.
//
// A recipient owned by the sender's own user is not delivered to its INBOX:
// messages are unique per (user, Message-ID), so that user can hold one copy
// only, and the copy in Sent — which the conversation view is built from —
// is that copy.
func routeRecipients(local LocalDeliverer, senderUserID int64, recipients []string) routing {
	var r routing
	seen := make(map[string]bool, len(recipients))
	for _, raw := range recipients {
		addr := envelopeAddress(raw)
		key := strings.ToLower(addr)
		if addr == "" || seen[key] {
			continue
		}
		seen[key] = true

		if local == nil {
			r.external = append(r.external, addr)
			continue
		}
		rcpt, err := local.Lookup(addr)
		var reject *localdelivery.RejectError
		switch {
		case err == nil:
			if rcpt.Mailbox.UserID == senderUserID {
				r.self++
			} else {
				r.local = append(r.local, rcpt)
			}
		case errors.Is(err, localdelivery.ErrNotLocal):
			r.external = append(r.external, addr)
		case errors.As(err, &reject):
			r.failed = append(r.failed, FailedRecipient{
				Address:    addr,
				Status:     reject.Status(),
				Diagnostic: fmt.Sprintf("smtp; %d %s %s", reject.Code, reject.Status(), reject.Message),
			})
		default:
			r.pending = append(r.pending, pendingRecipient{address: addr, err: err})
		}
	}
	return r
}

func (t *DirectSendTask) Execute(ctx context.Context) error {
	log.Printf("Direct sending message %d from %s", t.outboxMessage.ID, logmask.Addr(t.outboxMessage.From))

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Claim the row before anything else. Losing the claim means another
	// task already has it (or it is no longer due): sending anyway is how one
	// message used to go out twice. After the claim the send runs to the end
	// even if ctx is cancelled — abandoning it mid-way is what strands rows
	// in 'sending' and gets them re-sent on the next start.
	claimed, err := t.store.ClaimOutboxMessage(t.outboxMessage.ID)
	if err != nil {
		return fmt.Errorf("claim outbox message %d: %w", t.outboxMessage.ID, err)
	}
	if !claimed {
		log.Printf("Outbox message %d is not pending any more (taken by another task, sent or backing off) — skipping", t.outboxMessage.ID)
		return nil
	}

	recipients := parseRecipientsFromOutbox(t.outboxMessage)
	if len(recipients) == 0 {
		// Terminal validation error: mark failed explicitly, otherwise the
		// row is stranded in status='sending' forever (the scheduler only
		// picks up 'pending').
		if err := t.store.UpdateOutboxMessageStatus(t.outboxMessage.ID, "failed", "no recipients"); err != nil {
			log.Printf("Failed to mark message as failed: %v", err)
		}
		return fmt.Errorf("no recipients found")
	}

	var emailData []byte
	if len(t.outboxMessage.RawEmail) > 0 {
		emailData = t.outboxMessage.RawEmail
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

	parsed, perr := parser.New().ParseBytes(emailData)
	if perr != nil {
		parsed = &parser.ParsedMessage{RawData: emailData, RawSize: int64(len(emailData))}
	}
	if t.messageID == "" {
		// Message-ID for the Sent dedup.
		t.messageID = parsed.GetMessageID()
	}

	route := routeRecipients(t.local, t.outboxMessage.UserID, recipients)
	delivered := route.self
	failed := route.failed
	pending := route.pending

	// Our own domains: straight into the mailbox, as the MX would. A retry
	// re-runs this for every local recipient; those who already have the
	// message come back as Duplicate and are not given a second copy.
	if len(route.local) > 0 {
		env := localdelivery.Envelope{From: envelopeAddress(t.outboxMessage.From), Trusted: true}
		if _, d := localdelivery.SplitAddress(env.From); d != "" {
			env.FromDomain = d
		}
		results, errs := t.local.Deliver(env, emailData, parsed, route.local)
		for i, res := range results {
			if res == localdelivery.Failed {
				pending = append(pending, pendingRecipient{address: route.local[i].Email, err: errs[i]})
				continue
			}
			delivered++
		}
	}

	// Everyone else: via MX.
	if len(route.external) > 0 {
		results := t.sendExternal(t.outboxMessage.From, route.external, emailData, t.hostname)
		for _, addr := range route.external {
			err := results[addr]
			switch {
			case err == nil:
				delivered++
			case IsPermanent(err):
				status, diag := deliveryStatus(err)
				failed = append(failed, FailedRecipient{Address: addr, Status: status, Diagnostic: diag})
			default:
				pending = append(pending, pendingRecipient{address: addr, err: err})
			}
		}
	}

	if len(pending) > 0 {
		lastErr := pending[0].err
		// The stored count decides when to give up: t.outboxMessage.Retries is
		// a copy taken when the task was created.
		retries, incErr := t.store.IncrementOutboxMessageRetries(t.outboxMessage.ID, lastErr.Error())
		if incErr != nil {
			log.Printf("Failed to increment retries: %v", incErr)
			retries = t.outboxMessage.Retries + 1
		}
		if retries < maxRetries {
			if err := t.store.UpdateOutboxMessageStatus(t.outboxMessage.ID, "pending", lastErr.Error()); err != nil {
				log.Printf("Failed to mark message as pending: %v", err)
			}
			return fmt.Errorf("direct send failed: %w", lastErr)
		}
		log.Printf("Message %d exceeded max retries, giving up on %d recipient(s)", t.outboxMessage.ID, len(pending))
		for _, p := range pending {
			status, diag := deliveryStatus(p.err)
			if !strings.HasPrefix(status, "5") {
				status, diag = "4.4.7", "Delivery time expired: "+p.err.Error()
			}
			failed = append(failed, FailedRecipient{Address: p.address, Status: status, Diagnostic: diag})
		}
	}

	// Terminal: everyone is either delivered or definitely failed.
	if len(failed) > 0 {
		t.bounce(emailData, failed)
	}
	summary := failureSummary(failed)

	if delivered == 0 {
		log.Printf("Message %d could not be delivered to any recipient, marking as failed", t.outboxMessage.ID)
		if err := t.store.UpdateOutboxMessageStatus(t.outboxMessage.ID, "failed", summary); err != nil {
			log.Printf("Failed to mark message as failed: %v", err)
		}
		return fmt.Errorf("direct send failed: %s", summary)
	}

	if err := t.store.MarkOutboxMessageSent(t.outboxMessage.ID); err != nil {
		log.Printf("Failed to mark message as sent: %v", err)
		return err
	}
	if summary != "" {
		// Sent to some, bounced for others: keep the reason on the row.
		if err := t.store.UpdateOutboxMessageStatus(t.outboxMessage.ID, "sent", summary); err != nil {
			log.Printf("Failed to record partial failure: %v", err)
		}
	}

	// Save to Sent folder — без копии диалог, начатый пользователем,
	// не существует в списке бесед, пока собеседник не ответит
	// (relay-путь через SendTask делает то же самое).
	if t.saveSent != nil {
		t.saveSent(t.outboxMessage.UserID, emailData, t.messageID)
	}
	if t.notifySent != nil {
		t.notifySent()
	}

	log.Printf("Message %d delivered to %d recipient(s), %d failed", t.outboxMessage.ID, delivered, len(failed))
	return nil
}

// bounce puts a delivery status notification for failed into the sender's
// own mailbox. The sender of a direct-send message is always a local
// mailbox, so the DSN never leaves the server.
func (t *DirectSendTask) bounce(original []byte, failed []FailedRecipient) {
	if t.local == nil {
		log.Printf("Message %d: no local deliverer, bounce for %d recipient(s) not generated", t.outboxMessage.ID, len(failed))
		return
	}
	sender := envelopeAddress(t.outboxMessage.From)
	dsn, _ := buildDSN(t.hostname, sender, original, failed, t.now())
	parsed, err := parser.New().ParseBytes(dsn)
	if err != nil {
		log.Printf("Message %d: failed to parse own DSN: %v", t.outboxMessage.ID, err)
		return
	}
	rcpt := &localdelivery.Recipient{
		Email:   strings.ToLower(sender),
		Mailbox: &models.Mailbox{UserID: t.outboxMessage.UserID},
	}
	env := localdelivery.Envelope{Trusted: true}
	results, errs := t.local.Deliver(env, dsn, parsed, []*localdelivery.Recipient{rcpt})
	if results[0] == localdelivery.Failed {
		log.Printf("Message %d: failed to deliver bounce to sender: %v", t.outboxMessage.ID, errs[0])
		return
	}
	log.Printf("Message %d: bounce for %d recipient(s) delivered to the sender", t.outboxMessage.ID, len(failed))
}

// failureSummary is the outbox last_error for permanently failed recipients.
// Addresses are masked: last_error is shown in logs and the admin UI.
func failureSummary(failed []FailedRecipient) string {
	if len(failed) == 0 {
		return ""
	}
	parts := make([]string, 0, len(failed))
	for _, f := range failed {
		parts = append(parts, fmt.Sprintf("%s: %s (%s)", logmask.Addr(f.Address), f.Status, f.Diagnostic))
	}
	return "undeliverable: " + strings.Join(parts, "; ")
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
