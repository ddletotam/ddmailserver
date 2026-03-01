package mx

import (
	"log"

	"github.com/emersion/go-smtp"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/notify"
)

// Backend implements SMTP backend for MX server
type Backend struct {
	database *db.DB
	hub      *notify.Hub
}

// NewBackend creates a new MX SMTP backend
func NewBackend(database *db.DB) *Backend {
	return &Backend{
		database: database,
	}
}

// NewBackendWithHub creates a new MX SMTP backend with notification hub
func NewBackendWithHub(database *db.DB, hub *notify.Hub) *Backend {
	return &Backend{
		database: database,
		hub:      hub,
	}
}

// NewSession creates a new MX SMTP session
func (b *Backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	log.Printf("MX: New connection from %s", c.Conn().RemoteAddr())
	return &Session{
		database: b.database,
		hub:      b.hub,
		conn:     c,
	}, nil
}
