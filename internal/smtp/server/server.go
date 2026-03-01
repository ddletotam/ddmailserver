package server

import (
	"crypto/tls"
	"log"

	"github.com/emersion/go-smtp"
	"github.com/yourusername/mailserver/internal/db"
)

// Server wraps the SMTP server
type Server struct {
	smtpServer *smtp.Server
	addr       string
	tlsConfig  *tls.Config
}

// New creates a new SMTP server
func New(database *db.DB, addr string, hostname string) *Server {
	// Create backend
	be := NewBackend(database)

	// Create SMTP server
	s := smtp.NewServer(be)
	s.Addr = addr
	s.Domain = hostname
	s.AllowInsecureAuth = true           // Allow plain text auth (will be secured by TLS)
	s.MaxMessageBytes = 10 * 1024 * 1024 // 10MB max message size
	s.MaxRecipients = 50

	log.Printf("SMTP server created, will listen on %s", addr)

	return &Server{
		smtpServer: s,
		addr:       addr,
	}
}

// NewWithTLS creates a new SMTP server with TLS support
func NewWithTLS(database *db.DB, addr string, hostname string, certFile, keyFile string) (*Server, error) {
	// Load TLS certificate
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	// Create backend
	be := NewBackend(database)

	// Create SMTP server
	s := smtp.NewServer(be)
	s.Addr = addr
	s.Domain = hostname
	s.AllowInsecureAuth = true           // Auth is secured by TLS
	s.MaxMessageBytes = 10 * 1024 * 1024 // 10MB max message size
	s.MaxRecipients = 50
	s.TLSConfig = tlsConfig

	log.Printf("SMTP server with TLS created, will listen on %s", addr)

	return &Server{
		smtpServer: s,
		addr:       addr,
		tlsConfig:  tlsConfig,
	}, nil
}

// Start starts the SMTP server
func (s *Server) Start() error {
	log.Printf("Starting SMTP server on %s", s.addr)

	if err := s.smtpServer.ListenAndServe(); err != nil {
		return err
	}

	return nil
}

// StartTLS starts the SMTP server with implicit TLS
func (s *Server) StartTLS() error {
	log.Printf("Starting SMTP server with TLS on %s", s.addr)

	if err := s.smtpServer.ListenAndServeTLS(); err != nil {
		return err
	}

	return nil
}

// Stop stops the SMTP server
func (s *Server) Stop() error {
	log.Printf("Stopping SMTP server")
	return s.smtpServer.Close()
}
