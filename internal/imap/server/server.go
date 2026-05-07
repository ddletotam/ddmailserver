package server

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/emersion/go-imap-idle"
	"github.com/emersion/go-imap/server"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/notify"
	"github.com/yourusername/mailserver/internal/search"
)

// Server wraps the IMAP server
type Server struct {
	imapServer *server.Server
	backend    *Backend
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
		backend:    be,
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
		backend:    be,
		addr:       addr,
	}
}

// NewWithBackend creates a new IMAP server with existing backend (for shared backend usage)
func NewWithBackend(be *Backend, addr string) *Server {
	// Create IMAP server with provided backend
	s := server.New(be)
	s.Addr = addr
	s.AllowInsecureAuth = true

	log.Printf("IMAP server with shared backend created, will listen on %s", addr)

	return &Server{
		imapServer: s,
		backend:    be,
		addr:       addr,
	}
}

// NewWithBackendTLS creates a new IMAP server with existing backend and TLS support
func NewWithBackendTLS(be *Backend, addr string, certFile, keyFile string) (*Server, error) {
	// Load TLS certificate
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	// Create IMAP server with provided backend
	s := server.New(be)
	s.Addr = addr
	s.AllowInsecureAuth = true
	s.TLSConfig = tlsConfig

	log.Printf("IMAP server with shared backend and TLS created, will listen on %s", addr)

	return &Server{
		imapServer: s,
		backend:    be,
		addr:       addr,
		tlsConfig:  tlsConfig,
	}, nil
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
		backend:    be,
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
	s.AutoLogout = 30 * time.Minute // Disconnect idle clients after 30 min (RFC 3501 minimum)
	// s.Debug = os.Stderr // Uncomment for raw IMAP protocol debugging
	// Enable IDLE extension for push notifications
	s.Enable(idle.NewExtension())
	// Enable METADATA extension for DDMail client (identities, etc.)
	s.Enable(NewMetadataExtension())
	// Enable UIDPLUS (RFC 4315) + SPECIAL-USE (RFC 6154)
	s.Enable(NewUIDPLUSExtension())
	log.Printf("IMAP server with TLS, IDLE, UIDPLUS extensions enabled, will listen on %s", addr)

	return &Server{
		imapServer: s,
		backend:    be,
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

// StartTLS starts the IMAP server with TLS and TCP keepalive
func (s *Server) StartTLS() error {
	log.Printf("Starting IMAP server with TLS on %s", s.addr)

	// Create TCP listener with keepalive to detect dead connections (VPN drops etc.)
	tcpListener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	// Wrap with keepalive
	keepaliveListener := &tcpKeepAliveListener{tcpListener.(*net.TCPListener)}

	// Wrap with TLS
	tlsListener := tls.NewListener(keepaliveListener, s.tlsConfig)

	log.Printf("IMAP TLS server with TCP keepalive (30s) listening on %s", s.addr)
	return s.imapServer.Serve(tlsListener)
}

// tcpKeepAliveListener wraps a TCP listener to enable keepalive on accepted connections
type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln *tcpKeepAliveListener) Accept() (net.Conn, error) {
	tc, err := ln.TCPListener.AcceptTCP()
	if err != nil {
		return nil, err
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(30 * time.Second)
	return tc, nil
}

// Stop stops the IMAP server
func (s *Server) Stop() error {
	log.Printf("Stopping IMAP server")
	return s.imapServer.Close()
}

// SetSearchIndexer sets the Meilisearch indexer for full-text search
func (s *Server) SetSearchIndexer(indexer *search.Indexer) {
	if s.backend != nil {
		s.backend.SetSearchIndexer(indexer)
	}
}
