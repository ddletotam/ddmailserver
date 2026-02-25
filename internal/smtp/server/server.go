package server

import (
	"log"

	"github.com/emersion/go-smtp"
	"github.com/yourusername/mailserver/internal/db"
)

// Server wraps the SMTP server
type Server struct {
	smtpServer *smtp.Server
	addr       string
}

// New creates a new SMTP server
func New(database *db.DB, addr string) *Server {
	// Create backend
	be := NewBackend(database)

	// Create SMTP server
	s := smtp.NewServer(be)
	s.Addr = addr
	s.Domain = "localhost" // TODO: Make configurable
	s.AllowInsecureAuth = true // Allow plain text auth for development
	s.MaxMessageBytes = 10 * 1024 * 1024 // 10MB max message size
	s.MaxRecipients = 50
	// TODO: Add TLS support for production

	log.Printf("SMTP server created, will listen on %s", addr)

	return &Server{
		smtpServer: s,
		addr:       addr,
	}
}

// Start starts the SMTP server
func (s *Server) Start() error {
	log.Printf("Starting SMTP server on %s", s.addr)

	if err := s.smtpServer.ListenAndServe(); err != nil {
		return err
	}

	return nil
}

// Stop stops the SMTP server
func (s *Server) Stop() error {
	log.Printf("Stopping SMTP server")
	return s.smtpServer.Close()
}
