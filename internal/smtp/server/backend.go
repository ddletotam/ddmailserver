package server

import (
	"log"

	"github.com/emersion/go-smtp"
	"github.com/yourusername/mailserver/internal/authlimit"
	"github.com/yourusername/mailserver/internal/db"
)

// Backend implements SMTP backend
type Backend struct {
	database *db.DB
	// authLimiter throttles failed AUTH attempts; nil disables throttling.
	authLimiter *authlimit.Limiter
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
		conn:        c,
	}, nil
}
