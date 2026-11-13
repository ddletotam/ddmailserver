package server

import (
	"log"

	"github.com/ddletotam/ddmailserver/internal/authlimit"
	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/notify"
	"github.com/emersion/go-smtp"
)

// Backend implements SMTP backend
type Backend struct {
	database *db.DB
	// authLimiter throttles failed AUTH attempts; nil disables throttling.
	authLimiter *authlimit.Limiter
	// hub and kickOutbox: see Server.SetNotifyHub / SetOutboxTrigger.
	hub        *notify.Hub
	kickOutbox func()
}

// NewBackend creates a new SMTP backend
func NewBackend(database *db.DB) *Backend {
	return &Backend{
		database: database,
	}
}

// NewSession creates a new SMTP session
func (b *Backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	log.Printf("New SMTP connection from %s", c.Conn().RemoteAddr())
	return &Session{
		database:    b.database,
		authLimiter: b.authLimiter,
		hub:         b.hub,
		kickOutbox:  b.kickOutbox,
		conn:        c,
	}, nil
}
