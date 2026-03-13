package mx

import (
	"log"
	"strings"

	"github.com/emersion/go-smtp"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/notify"
	"github.com/yourusername/mailserver/internal/parser"
)

// CalendarSyncTrigger is called to trigger immediate calendar sync for a user
type CalendarSyncTrigger func(userID int64)

// Backend implements SMTP backend for MX server
type Backend struct {
	database            *db.DB
	hub                 *notify.Hub
	analyzer            *parser.Analyzer
	calendarSyncTrigger CalendarSyncTrigger
}

// NewBackend creates a new MX SMTP backend
func NewBackend(database *db.DB) *Backend {
	return &Backend{
		database: database,
		analyzer: parser.NewAnalyzer(nil), // Use default config
	}
}

// NewBackendWithHub creates a new MX SMTP backend with notification hub
func NewBackendWithHub(database *db.DB, hub *notify.Hub) *Backend {
	return &Backend{
		database: database,
		hub:      hub,
		analyzer: parser.NewAnalyzer(nil), // Use default config
	}
}

// NewBackendWithAnalyzer creates a new MX SMTP backend with custom analyzer config
func NewBackendWithAnalyzer(database *db.DB, hub *notify.Hub, analyzerConfig *parser.AnalyzerConfig) *Backend {
	return &Backend{
		database: database,
		hub:      hub,
		analyzer: parser.NewAnalyzer(analyzerConfig),
	}
}

// NewBackendWithCalendarSync creates a new MX SMTP backend with calendar sync trigger
func NewBackendWithCalendarSync(database *db.DB, hub *notify.Hub, calendarSyncTrigger CalendarSyncTrigger) *Backend {
	return &Backend{
		database:            database,
		hub:                 hub,
		analyzer:            parser.NewAnalyzer(nil),
		calendarSyncTrigger: calendarSyncTrigger,
	}
}

// NewSession creates a new MX SMTP session
func (b *Backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	remoteAddr := c.Conn().RemoteAddr().String()
	log.Printf("MX: New connection from %s", remoteAddr)

	// Extract IP from remote address (format: "ip:port" or "[ipv6]:port")
	senderIP := extractIPFromAddr(remoteAddr)

	return &Session{
		database:            b.database,
		hub:                 b.hub,
		conn:                c,
		analyzer:            b.analyzer,
		senderIP:            senderIP,
		calendarSyncTrigger: b.calendarSyncTrigger,
	}, nil
}

// extractIPFromAddr extracts IP address from "ip:port" format
func extractIPFromAddr(addr string) string {
	// Handle IPv6 format [::1]:port
	if strings.HasPrefix(addr, "[") {
		end := strings.Index(addr, "]")
		if end > 0 {
			return addr[1:end]
		}
	}
	// Handle IPv4 format 1.2.3.4:port
	if idx := strings.LastIndex(addr, ":"); idx > 0 {
		return addr[:idx]
	}
	return addr
}
