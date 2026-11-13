package client

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/logmask"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/outbox"
	"github.com/ddletotam/ddmailserver/internal/parser"
	"github.com/ddletotam/ddmailserver/internal/smtp/localdelivery"
)

// DeliveryTracker is the sender's side of a send: the message's fixed wire
// form, its copy in Sent and its delivery status (outbox.Service).
//
// The copy is made when the message is accepted, by the send handler — not
// here, after delivery, as it used to be. A send task only makes it for a row
// accepted by a server version that did not (Prepare reports such a row as
// fresh); otherwise it just reports the outcome.
type DeliveryTracker interface {
	Prepare(msg *models.OutboxMessage) (fresh bool, err error)
	SaveSentCopy(msg *models.OutboxMessage) error
	Record(msg *models.OutboxMessage, d outbox.Delivery)
}

// prepareOutbox fixes the wire form and Message-ID of msg before an attempt.
// Without a tracker (tests) nothing is stored: a row without a wire form is
// composed from its fields for this attempt only.
func prepareOutbox(tracker DeliveryTracker, msg *models.OutboxMessage) error {
	if tracker == nil {
		if len(msg.RawEmail) == 0 {
			msg.RawEmail = outbox.Compose(msg, nil, time.Now())
		}
		if msg.MessageID == "" {
			if parsed, err := parser.New().ParseBytes(msg.RawEmail); err == nil {
				msg.MessageID = parsed.MessageID
			}
		}
		return nil
	}
	fresh, err := tracker.Prepare(msg)
	if err != nil {
		return err
	}
	if fresh {
		// Accepted before the copy in Sent was made at acceptance: make it
		// now, so the conversation shows up and the outcome has somewhere to
		// be recorded. A failure here does not stop the delivery.
		if err := tracker.SaveSentCopy(msg); err != nil {
			log.Printf("Outbox message %d: copy to Sent: %v", msg.ID, err)
		}
	}
	return nil
}

// record reports an outcome to the tracker, if there is one.
func record(tracker DeliveryTracker, msg *models.OutboxMessage, d outbox.Delivery) {
	if tracker != nil {
		tracker.Record(msg, d)
	}
}

// pendingDelivery is the status of a message that will be retried after err.
func pendingDelivery(err error) outbox.Delivery {
	return outbox.Delivery{Status: outbox.StatusPending, Reason: reasonFor(err)}
}

// failedDelivery is the final status of a message that failed for failed.
func failedDelivery(failed []FailedRecipient) outbox.Delivery {
	d := outbox.Delivery{Status: outbox.StatusFailed, Reason: reasonOrOther(failed[0].Reason)}
	for _, f := range failed {
		d.Failed = append(d.Failed, f.Address)
	}
	return d
}

// bounceToSender puts a delivery status notification for failed into the
// sender's own local INBOX. The sender of any outbox message is a user of
// this server, so the DSN never leaves it. Both send paths use it: direct
// delivery, and a relay whose provider refused the message or could not be
// reached until the retries ran out (the provider bounces what it accepted
// and failed later by itself, into the external mailbox).
func bounceToSender(local LocalDeliverer, hostname string, msg *models.OutboxMessage, original []byte, failed []FailedRecipient, now time.Time) {
	if local == nil {
		log.Printf("Message %d: no local deliverer, bounce for %d recipient(s) not generated", msg.ID, len(failed))
		return
	}
	sender := envelopeAddress(msg.From)
	dsn, _ := buildDSN(hostname, sender, original, msg.MessageID, failed, now)
	parsed, err := parser.New().ParseBytes(dsn)
	if err != nil {
		log.Printf("Message %d: failed to parse own DSN: %v", msg.ID, err)
		return
	}
	rcpt := &localdelivery.Recipient{
		Email:   strings.ToLower(sender),
		Mailbox: &models.Mailbox{UserID: msg.UserID},
	}
	env := localdelivery.Envelope{Trusted: true}
	results, errs := local.Deliver(env, dsn, parsed, []*localdelivery.Recipient{rcpt})
	if results[0] == localdelivery.Failed {
		log.Printf("Message %d: failed to deliver bounce to sender: %v", msg.ID, errs[0])
		return
	}
	log.Printf("Message %d: bounce for %d recipient(s) delivered to the sender", msg.ID, len(failed))
}

// failureSummary is the outbox last_error for permanently failed recipients:
// the raw errors, for the operator. Addresses are masked: last_error is shown
// in logs and the admin UI.
func failureSummary(failed []FailedRecipient) string {
	if len(failed) == 0 {
		return ""
	}
	parts := make([]string, 0, len(failed))
	for _, f := range failed {
		detail := f.Diagnostic
		if detail == "" {
			detail = f.Detail
		}
		parts = append(parts, fmt.Sprintf("%s: %s %s (%s)", logmask.Addr(f.Address), f.Status, f.Reason, detail))
	}
	return "undeliverable: " + strings.Join(parts, "; ")
}
