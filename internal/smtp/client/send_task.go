package client

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/authfail"
	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/logmask"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/outbox"
	"github.com/ddletotam/ddmailserver/internal/task"
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

	// tracker keeps the sender's copy in Sent and its delivery status.
	tracker DeliveryTracker
	// local puts the bounce (DSN) into the sender's mailbox; nil — no bounce.
	local LocalDeliverer
	// hostname names this server in the bounce (MAILER-DAEMON@hostname).
	hostname string
}

// SetDeliveryTracker sets where the delivery outcome is recorded.
//
// Sending is asynchronous: the client is told «отправлено» when the message
// is queued. The copy in Sent — and with it the conversation — exists from
// that moment (outbox.Service.Accept); the tracker is how the outcome reaches
// that copy and the user's clients (contract §1).
func (t *SendTask) SetDeliveryTracker(tr DeliveryTracker) {
	t.tracker = tr
}

// SetBounce enables the delivery status notification into the sender's own
// mailbox when the relay refuses the message or cannot be reached until the
// retries run out.
func (t *SendTask) SetBounce(local LocalDeliverer, hostname string) {
	t.local = local
	t.hostname = hostname
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
	msg := t.outboxMessage
	log.Printf("Sending message %d via account %s", msg.ID, t.account.Email)

	// Check context cancellation
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// The provider rejects this account's SMTP password and the pause is not
	// over: leave the message pending, untouched. The scheduler filters such
	// messages already; this is the arbiter when the pause just expired.
	guard := authfail.NewGuard(nil)
	if t.database != nil {
		guard = authfail.NewGuard(t.database)
	}
	sub := authfail.AccountSMTP(t.account)
	if !guard.Allow(sub) {
		return nil
	}

	// Claim the row before anything else. Losing the claim means another
	// task already has it (or it is no longer due): sending anyway is how one
	// message used to go out twice. After the claim the send runs to the end
	// even if ctx is cancelled — abandoning it mid-way is what strands rows
	// in 'sending' and gets them re-sent on the next start.
	claimed, err := t.database.ClaimOutboxMessage(msg.ID)
	if err != nil {
		return fmt.Errorf("claim outbox message %d: %w", msg.ID, err)
	}
	if !claimed {
		log.Printf("Outbox message %d is not pending any more (taken by another task, sent or backing off) — skipping", msg.ID)
		return nil
	}

	// The wire form is fixed once (outbox.Service.Prepare): every attempt
	// sends the same bytes under the same Message-ID — the one the copy in
	// Sent has, and the one the provider's own Sent copy comes back with on
	// sync, which is what glues the two into one message.
	if err := prepareOutbox(t.tracker, msg); err != nil {
		if uerr := t.database.UpdateOutboxMessageStatus(msg.ID, "pending", err.Error()); uerr != nil {
			log.Printf("Failed to mark message as pending: %v", uerr)
		}
		return fmt.Errorf("prepare outbox message %d: %w", msg.ID, err)
	}

	// Create SMTP client
	client := New(t.account)

	// Prepare recipients. Shared with the direct-MX path: both need addr-specs
	// with the display names stripped, and this used to be the one place that
	// handed RCPT TO the whole "Name <addr>" string.
	recipients := parseRecipientsFromOutbox(msg)
	if len(recipients) == 0 {
		// Terminal validation error: without the explicit failed status the
		// message would be stranded in status='sending' forever (the
		// scheduler only picks up 'pending').
		if err := t.database.UpdateOutboxMessageStatus(msg.ID, "failed", "no recipients"); err != nil {
			log.Printf("Failed to mark message as failed: %v", err)
		}
		record(t.tracker, msg, outbox.Delivery{Status: outbox.StatusFailed, Reason: outbox.ReasonOther})
		return fmt.Errorf("no recipients found")
	}

	// Send email
	err = client.Send(msg.From, recipients, msg.RawEmail)

	if err == nil || client.AuthPassed() {
		guard.Accepted(sub)
	}
	if err != nil && authfail.Is(err) {
		// The password, not the message: keep it pending without spending
		// a retry — six rejected logins would otherwise mark a perfectly
		// good message failed. It goes out when the pause ends or the
		// password is edited (which lifts the pause at once).
		guard.Rejected(sub, err)
		reason := "deferred: the provider rejected the SMTP password of this account; " +
			"the message will be sent once the password is updated (" + err.Error() + ")"
		if dbErr := t.database.UpdateOutboxMessageStatus(msg.ID, "pending", reason); dbErr != nil {
			log.Printf("Failed to mark message as deferred: %v", dbErr)
		}
		record(t.tracker, msg, outbox.Delivery{Status: outbox.StatusPending, Reason: outbox.ReasonRelayAuth})
		return fmt.Errorf("failed to send: %w", err)
	}

	if err != nil {
		// Increment retries. The stored count decides when to give up —
		// msg.Retries is a copy taken when the task was created and says
		// nothing about attempts made since.
		retries, incErr := t.database.IncrementOutboxMessageRetries(msg.ID, err.Error())
		if incErr != nil {
			log.Printf("Failed to increment retries: %v", incErr)
			retries = msg.Retries + 1
		}

		// Give up now on a permanent refusal (5xx after AUTH, e.g. "555
		// syntax error" or "550 message blocked"): five more attempts over
		// 17 hours would get the same answer.
		if retries >= maxRetries || IsPermanent(err) {
			log.Printf("Message %d exceeded max retries or was refused, marking as failed: %v", msg.ID, err)
			if uerr := t.database.UpdateOutboxMessageStatus(msg.ID, "failed", err.Error()); uerr != nil {
				log.Printf("Failed to mark message as failed: %v", uerr)
			}
			// The relay took the message from nobody: one refusal (a
			// recipient, the sender, the content) ends the transaction, so
			// every recipient failed for the same reason.
			failed := make([]FailedRecipient, 0, len(recipients))
			for _, rcpt := range envelopeAddresses(recipients) {
				f := failedRecipient(rcpt, err)
				if !strings.HasPrefix(f.Status, "5") {
					f.Status = "4.4.7"
				}
				failed = append(failed, f)
			}
			bounceToSender(t.local, t.hostname, msg, msg.RawEmail, failed, time.Now())
			record(t.tracker, msg, failedDelivery(failed))
		} else {
			// Mark as pending for retry
			if uerr := t.database.UpdateOutboxMessageStatus(msg.ID, "pending", err.Error()); uerr != nil {
				log.Printf("Failed to mark message as pending: %v", uerr)
			}
			record(t.tracker, msg, pendingDelivery(err))
		}

		return fmt.Errorf("failed to send: %w", err)
	}

	// Mark as sent
	if err := t.database.MarkOutboxMessageSent(msg.ID); err != nil {
		log.Printf("Failed to mark message as sent: %v", err)
		return err
	}
	// «Delivered» here means the provider's SMTP took it; a failure further
	// on comes back from the provider as an ordinary bounce letter into the
	// external mailbox.
	record(t.tracker, msg, outbox.Delivery{Status: outbox.StatusDelivered})

	log.Printf("Message %d sent successfully", msg.ID)
	return nil
}

// randomBoundary generates a random MIME boundary string.
func randomBoundary() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("----=_Part_%x", b)
}
