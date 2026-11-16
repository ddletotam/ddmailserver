package mx

import (
	"crypto/tls"
	"log"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/notify"
	"github.com/emersion/go-smtp"
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

// NewWithHubAndCalendarSync creates a new MX SMTP server with notification hub and calendar sync trigger
func NewWithHubAndCalendarSync(database *db.DB, addr string, hostname string, hub *notify.Hub, calendarSyncTrigger CalendarSyncTrigger) *Server {
	// Create backend with hub and calendar sync trigger
	be := NewBackendWithCalendarSync(database, hub, calendarSyncTrigger)

	// Create SMTP server
	s := smtp.NewServer(be)
	s.Addr = addr
	s.Domain = hostname
	s.AllowInsecureAuth = true           // No auth required for MX
	s.MaxMessageBytes = 25 * 1024 * 1024 // 25MB max message size
	s.MaxRecipients = 100

	log.Printf("MX server with IDLE notifications and calendar sync created, will listen on %s", addr)

	return &Server{
		smtpServer: s,
		addr:       addr,
	}
}

// EnableSTARTTLS offers STARTTLS on the MX port with the server certificate.
//
// Without it every message from Gmail, Mail.ru and the rest arrived in
// plaintext (EHLO listed no STARTTLS — found 2026-11-16). TLS here is
// opportunistic, as on any public MX: a sender that cannot do TLS still
// delivers in the clear; the certificate is not required to match the MX
// host name, senders without MTA-STS/DANE do not check it. The certificate
// is read once — the certbot deploy hook restarts the service on renewal.
func (s *Server) EnableSTARTTLS(certFile, keyFile string) error {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}
	s.smtpServer.TLSConfig = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	log.Printf("MX server: STARTTLS enabled")
	return nil
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
