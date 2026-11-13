package web

import (
	"log"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/outbox"
)

// outboxService accepts queued messages: fixes their wire form, puts the
// sender's copy into Sent and tells the clients (package outbox).
func (s *Server) outboxService() *outbox.Service {
	return outbox.New(s.database, s.notifyHub)
}

// acceptOutbox runs outbox acceptance for a message the handler has just
// queued (row and attachments stored). A failure leaves the message queued —
// it is sent anyway, and the send task finishes the acceptance — so it is
// only logged.
func (s *Server) acceptOutbox(msg *models.OutboxMessage) {
	if err := s.outboxService().Accept(msg); err != nil {
		log.Printf("Outbox message %d queued, but accepting it failed (it is sent anyway): %v", msg.ID, err)
	}
}

// deliveryErrorText is the reason a message is (still) not delivered, in the
// user's language — the user's own setting, else the server's default
// locale. Empty for no reason.
func (s *Server) deliveryErrorText(user *models.User, reason string) string {
	if reason == "" || s.i18nManager == nil {
		return ""
	}
	locale := ""
	if user != nil {
		locale = user.Language
	}
	if locale == "" && s.i18n != nil {
		locale = s.i18n.GetLocale()
	}
	return s.i18nManager.Get(locale).T(outbox.ReasonKey(reason))
}
