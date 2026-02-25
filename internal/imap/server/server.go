package server

import (
	"log"

	"github.com/emersion/go-imap/server"
	"github.com/yourusername/mailserver/internal/db"
)

// Server wraps the IMAP server
type Server struct {
	imapServer *server.Server
	addr       string
}

// New creates a new IMAP server
func New(database *db.DB, addr string) *Server {
	// Create backend
	be := NewBackend(database)

	// Create IMAP server
	s := server.New(be)
	s.Addr = addr
	s.AllowInsecureAuth = true // Allow plain text auth for development
	// TODO: Add TLS support for production

	log.Printf("IMAP server created, will listen on %s", addr)

	return &Server{
		imapServer: s,
		addr:       addr,
	}
}

// Start starts the IMAP server
func (s *Server) Start() error {
	log.Printf("Starting IMAP server on %s", s.addr)

	if err := s.imapServer.ListenAndServe(); err != nil {
		return err
	}

	return nil
}

// Stop stops the IMAP server
func (s *Server) Stop() error {
	log.Printf("Stopping IMAP server")
	return s.imapServer.Close()
}
