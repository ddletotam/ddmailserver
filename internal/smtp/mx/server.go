package mx

import (
	"log"

	"github.com/emersion/go-smtp"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/notify"
)

// Server wraps the MX SMTP server for incoming mail
type Server struct {
	smtpServer *smtp.Server
	addr       string
}

// New creates a new MX SMTP server
func New(database *db.DB, addr string, hostname string) *Server {
	// Create backend
	be := NewBackend(database)

	// Create SMTP server
	s := smtp.NewServer(be)
	s.Addr = addr
	s.Domain = hostname
	s.AllowInsecureAuth = true           // No auth required for MX
	s.MaxMessageBytes = 25 * 1024 * 1024 // 25MB max message size
	s.MaxRecipients = 100

	log.Printf("MX server created, will listen on %s", addr)

	return &Server{
		smtpServer: s,
		addr:       addr,
	}
}

// NewWithHub creates a new MX SMTP server with notification hub for IDLE support
func NewWithHub(database *db.DB, addr string, hostname string, hub *notify.Hub) *Server {
	// Create backend with hub
	be := NewBackendWithHub(database, hub)

	// Create SMTP server
	s := smtp.NewServer(be)
	s.Addr = addr
	s.Domain = hostname
	s.AllowInsecureAuth = true           // No auth required for MX
	s.MaxMessageBytes = 25 * 1024 * 1024 // 25MB max message size
	s.MaxRecipients = 100

	log.Printf("MX server with IDLE notifications created, will listen on %s", addr)

	return &Server{
		smtpServer: s,
		addr:       addr,
	}
}

// Start starts the MX SMTP server
func (s *Server) Start() error {
	log.Printf("Starting MX server on %s", s.addr)

	if err := s.smtpServer.ListenAndServe(); err != nil {
		return err
	}

	return nil
}

// Stop stops the MX SMTP server
func (s *Server) Stop() error {
	log.Printf("Stopping MX server")
	return s.smtpServer.Close()
}
