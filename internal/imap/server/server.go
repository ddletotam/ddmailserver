package server

import (
	"crypto/tls"
	"log"

	"github.com/emersion/go-imap/server"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/notify"
)

// Server wraps the IMAP server
type Server struct {
	imapServer *server.Server
	addr       string
	tlsConfig  *tls.Config
}

// New creates a new IMAP server
func New(database *db.DB, addr string) *Server {
	// Create backend
	be := NewBackend(database)

	// Create IMAP server
	s := server.New(be)
	s.Addr = addr
	s.AllowInsecureAuth = true // Allow plain text auth (will be secured by TLS)

	log.Printf("IMAP server created, will listen on %s", addr)

	return &Server{
		imapServer: s,
		addr:       addr,
	}
}

// NewWithHub creates a new IMAP server with notification hub for IDLE support
func NewWithHub(database *db.DB, addr string, hub *notify.Hub) *Server {
	// Create backend with hub
	be := NewBackendWithHub(database, hub)

	// Create IMAP server
	s := server.New(be)
	s.Addr = addr
	s.AllowInsecureAuth = true

	log.Printf("IMAP server with IDLE support created, will listen on %s", addr)

	return &Server{
		imapServer: s,
		addr:       addr,
	}
}

// NewWithTLS creates a new IMAP server with TLS support
func NewWithTLS(database *db.DB, addr string, certFile, keyFile string) (*Server, error) {
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

	// Create IMAP server
	s := server.New(be)
	s.Addr = addr
	s.AllowInsecureAuth = true // Auth is secured by TLS
	s.TLSConfig = tlsConfig

	log.Printf("IMAP server with TLS created, will listen on %s", addr)

	return &Server{
		imapServer: s,
		addr:       addr,
		tlsConfig:  tlsConfig,
	}, nil
}

// NewWithTLSAndHub creates a new IMAP server with TLS and notification hub
func NewWithTLSAndHub(database *db.DB, addr string, certFile, keyFile string, hub *notify.Hub) (*Server, error) {
	// Load TLS certificate
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	// Create backend with hub
	be := NewBackendWithHub(database, hub)

	// Create IMAP server
	s := server.New(be)
	s.Addr = addr
	s.AllowInsecureAuth = true
	s.TLSConfig = tlsConfig

	log.Printf("IMAP server with TLS and IDLE support created, will listen on %s", addr)

	return &Server{
		imapServer: s,
		addr:       addr,
		tlsConfig:  tlsConfig,
	}, nil
}

// Start starts the IMAP server
func (s *Server) Start() error {
	log.Printf("Starting IMAP server on %s", s.addr)

	if err := s.imapServer.ListenAndServe(); err != nil {
		return err
	}

	return nil
}

// StartTLS starts the IMAP server with TLS
func (s *Server) StartTLS() error {
	log.Printf("Starting IMAP server with TLS on %s", s.addr)

	if err := s.imapServer.ListenAndServeTLS(); err != nil {
		return err
	}

	return nil
}

// Stop stops the IMAP server
func (s *Server) Stop() error {
	log.Printf("Stopping IMAP server")
	return s.imapServer.Close()
}
