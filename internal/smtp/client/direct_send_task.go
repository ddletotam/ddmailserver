package client

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/dkimsign"
	"github.com/ddletotam/ddmailserver/internal/logmask"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/outbox"
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
	store         outboxStore
	hostname      string
	signer        *dkimsign.Signer // nil → send unsigned
	priority      int

	// local delivers to our own domains; nil sends everything through MX
	// (which, for our own domains, dials ourselves and fails behind NAT).
	local LocalDeliverer
	// tracker keeps the sender's copy and its delivery status; nil in tests
	// that do not look at them.
	tracker DeliveryTracker
	// sendExternal is SendDirectEach (replaceable in tests).
	sendExternal func(from string, to []string, msg []byte, hostname string) map[string]error
	// now is the clock for DSN dates (tests).
	now func() time.Time
}

// SetLocalDeliverer enables in-process delivery to the domains this server
// hosts, and bounces (DSN) into the sender's mailbox.
func (t *DirectSendTask) SetLocalDeliverer(d LocalDeliverer) {
	t.local = d
}

// SetDeliveryTracker sets where the delivery outcome is recorded.
func (t *DirectSendTask) SetDeliveryTracker(tr DeliveryTracker) {
	t.tracker = tr
}

// SetExternalSender replaces delivery via MX (SendDirectEach) — for tests
// that must not reach the network.
func (t *DirectSendTask) SetExternalSender(fn func(from string, to []string, msg []byte, hostname string) map[string]error) {
	t.sendExternal = fn
}

// NewDirectSendTask creates a new direct send task
func NewDirectSendTask(outboxMessage *models.OutboxMessage, database *db.DB, hostname string, signer *dkimsign.Signer) *DirectSendTask {
	t := &DirectSendTask{
		outboxMessage: outboxMessage,
		hostname:      hostname,
		signer:        signer,
		priority:      1,
		sendExternal:  SendDirectEach,
		now:           time.Now,
	}
	if database != nil {
		t.store = database
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
				Reason:     reasonFor(reject),
				Diagnostic: fmt.Sprintf("smtp; %d %s %s", reject.Code, reject.Status(), reject.Message),
				Detail:     reject.Error(),
			})
		default:
			r.pending = append(r.pending, pendingRecipient{address: addr, err: err})
		}
	}
	return r
}

func (t *DirectSendTask) Execute(ctx context.Context) error {
	msg := t.outboxMessage
	log.Printf("Direct sending message %d from %s", msg.ID, logmask.Addr(msg.From))

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Claim the row before anything else. Losing the claim means another
	// task already has it (or it is no longer due): sending anyway is how one
	// message used to go out twice. After the claim the send runs to the end
	// even if ctx is cancelled — abandoning it mid-way is what strands rows
	// in 'sending' and gets them re-sent on the next start.
	claimed, err := t.store.ClaimOutboxMessage(msg.ID)
	if err != nil {
		return fmt.Errorf("claim outbox message %d: %w", msg.ID, err)
	}
	if !claimed {
		log.Printf("Outbox message %d is not pending any more (taken by another task, sent or backing off) — skipping", msg.ID)
		return nil
	}

	// The wire form is fixed once (outbox.Service.Prepare): every attempt
	// sends the same bytes under the same Message-ID, and the copy in Sent
	// is that message. Failing to prepare is a database problem, not the
	// message's: back to the queue without spending a retry.
	if err := prepareOutbox(t.tracker, msg); err != nil {
		if uerr := t.store.UpdateOutboxMessageStatus(msg.ID, "pending", err.Error()); uerr != nil {
			log.Printf("Failed to mark message as pending: %v", uerr)
		}
		return fmt.Errorf("prepare outbox message %d: %w", msg.ID, err)
	}
	original := msg.RawEmail

	recipients := parseRecipientsFromOutbox(msg)
	if len(recipients) == 0 {
		// Terminal validation error: mark failed explicitly, otherwise the
		// row is stranded in status='sending' forever (the scheduler only
		// picks up 'pending').
		if err := t.store.UpdateOutboxMessageStatus(msg.ID, "failed", "no recipients"); err != nil {
			log.Printf("Failed to mark message as failed: %v", err)
		}
		record(t.tracker, msg, outbox.Delivery{Status: outbox.StatusFailed, Reason: outbox.ReasonOther})
		return fmt.Errorf("no recipients found")
	}

	// DKIM: sign with the From-domain key when one is configured. Without
	// the signature Gmail/Yandex either junk or reject direct delivery.
	// Only the wire copy is signed; the copy in Sent stays as accepted.
	emailData := t.signer.Sign(msg.From, original)

	parsed, perr := parser.New().ParseBytes(emailData)
	if perr != nil {
		parsed = &parser.ParsedMessage{RawData: emailData, RawSize: int64(len(emailData))}
	}

	route := routeRecipients(t.local, msg.UserID, recipients)
	delivered := route.self
	failed := route.failed
	pending := route.pending

	// Our own domains: straight into the mailbox, as the MX would. A retry
	// re-runs this for every local recipient; those who already have the
	// message come back as Duplicate and are not given a second copy.
	if len(route.local) > 0 {
		env := localdelivery.Envelope{From: envelopeAddress(msg.From), Trusted: true}
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
		results := t.sendExternal(msg.From, route.external, emailData, t.hostname)
		for _, addr := range route.external {
			err := results[addr]
			switch {
			case err == nil:
				delivered++
			case IsPermanent(err):
				failed = append(failed, failedRecipient(addr, err))
			default:
				pending = append(pending, pendingRecipient{address: addr, err: err})
			}
		}
	}

	if len(pending) > 0 {
		lastErr := pending[0].err
		// The stored count decides when to give up: msg.Retries is a copy
		// taken when the task was created.
		retries, incErr := t.store.IncrementOutboxMessageRetries(msg.ID, lastErr.Error())
		if incErr != nil {
			log.Printf("Failed to increment retries: %v", incErr)
			retries = msg.Retries + 1
		}
		if retries < maxRetries {
			if err := t.store.UpdateOutboxMessageStatus(msg.ID, "pending", lastErr.Error()); err != nil {
				log.Printf("Failed to mark message as pending: %v", err)
			}
			// Still pending; the reason tells the sender why it is taking
			// long. Recipients already delivered to keep their copy, and a
			// permanent failure of another recipient is reported when the
			// message is final.
			record(t.tracker, msg, pendingDelivery(lastErr))
			return fmt.Errorf("direct send failed: %w", lastErr)
		}
		log.Printf("Message %d exceeded max retries, giving up on %d recipient(s)", msg.ID, len(pending))
		for _, p := range pending {
			f := failedRecipient(p.address, p.err)
			if !strings.HasPrefix(f.Status, "5") {
				f.Status = "4.4.7"
			}
			if f.Reason == outbox.ReasonOther && f.Diagnostic == "" {
				// A local store error that never cleared up.
				f.Reason = outbox.ReasonTemporary
			}
			failed = append(failed, f)
		}
	}

	// Terminal: everyone is either delivered or definitely failed.
	if len(failed) > 0 {
		bounceToSender(t.local, t.hostname, msg, original, failed, t.now())
	}
	summary := failureSummary(failed)

	if delivered == 0 {
		log.Printf("Message %d could not be delivered to any recipient, marking as failed: %s", msg.ID, summary)
		if err := t.store.UpdateOutboxMessageStatus(msg.ID, "failed", summary); err != nil {
			log.Printf("Failed to mark message as failed: %v", err)
		}
		record(t.tracker, msg, failedDelivery(failed))
		return fmt.Errorf("direct send failed: %s", summary)
	}

	if err := t.store.MarkOutboxMessageSent(msg.ID); err != nil {
		log.Printf("Failed to mark message as sent: %v", err)
		return err
	}
	if summary != "" {
		// Sent to some, bounced for others: keep the reason on the row, and
		// the sender sees «not delivered» with the addresses that failed.
		if err := t.store.UpdateOutboxMessageStatus(msg.ID, "sent", summary); err != nil {
			log.Printf("Failed to record partial failure: %v", err)
		}
		record(t.tracker, msg, failedDelivery(failed))
	} else {
		record(t.tracker, msg, outbox.Delivery{Status: outbox.StatusDelivered})
	}

	log.Printf("Message %d delivered to %d recipient(s), %d failed", msg.ID, delivered, len(failed))
	return nil
}
