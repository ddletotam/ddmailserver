// Package outbox is the sender's side of outgoing mail: what happens between
// «Отправить» and the delivery outcome, apart from SMTP itself.
//
// A message is accepted (Service.Accept) by the send handlers — POST
// /api/desktop/v1/send, the web compose form and SMTP submission — right
// after its outbox row is stored. Accepting fixes the wire form and the
// Message-ID of the message and puts the sender's copy into Sent with
// delivery status «pending», so the conversation with a new correspondent
// exists the moment the server has the message, not when (if ever) the
// recipient's server takes it. The send tasks (internal/smtp/client) then
// report the outcome with Service.Record, which updates the copy and tells
// the user's clients (WebSocket event delivery_status).
package outbox

import "strings"

// Delivery status of an outgoing message (messages.delivery_status).
// The empty status means «unknown»: messages sent before the status existed,
// and every message that is not an outgoing copy. Clients treat it as
// delivered.
const (
	StatusUnknown   = ""
	StatusPending   = "pending"   // queued or being sent, including between retries
	StatusDelivered = "delivered" // accepted by every recipient's server (relay: by the provider's SMTP)
	StatusFailed    = "failed"    // not delivered to at least one recipient; final
)

// Reason codes (messages.delivery_reason). The text shown to people is the
// i18n key ReasonKey(code); the raw SMTP / network error stays in the log and
// in outbox_messages.last_error.
const (
	ReasonUnreachable        = "unreachable"         // the recipient's server does not answer or refuses connections
	ReasonDomainNotFound     = "domain_not_found"    // the recipient's domain does not exist
	ReasonNoMailDomain       = "no_mail_domain"      // the domain declares it takes no mail (null MX)
	ReasonMailboxUnavailable = "mailbox_unavailable" // no such mailbox, or it is disabled
	ReasonMailboxFull        = "mailbox_full"        // the recipient's mailbox is over quota
	ReasonRejected           = "rejected"            // the recipient's server refused the message
	ReasonTemporary          = "temporary"           // only temporary refusals until retries ran out
	ReasonRelayAuth          = "relay_auth"          // pending: the provider rejects the account's SMTP password
	ReasonOther              = "other"               // anything else
)

// ReasonKey is the i18n key of the human-readable text for a reason code.
func ReasonKey(reason string) string {
	return "delivery.reason." + reason
}

// Delivery is the outcome of (an attempt at) delivering one message.
type Delivery struct {
	Status string
	Reason string
	// Failed lists the addresses the message was not delivered to.
	Failed []string
}

// failedList is Delivery.Failed as stored in messages.delivery_failed.
func (d Delivery) failedList() string {
	return strings.Join(d.Failed, ", ")
}

// SplitFailed parses messages.delivery_failed back into addresses.
func SplitFailed(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
