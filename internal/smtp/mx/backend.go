package mx

import (
	"log"

	"github.com/emersion/go-smtp"
	"github.com/yourusername/mailserver/internal/db"
)

// Backend implements SMTP backend for MX server
type Backend struct {
	database *db.DB
}

// NewBackend creates a new MX SMTP backend
func NewBackend(database *db.DB) *Backend {
	return &Backend{
		database: database,
	}
}

// NewSession creates a new MX SMTP session
func (b *Backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	log.Printf("MX: New connection from %s", c.Conn().RemoteAddr())
	return &Session{
		database: b.database,
		conn:     c,
	}, nil
}
