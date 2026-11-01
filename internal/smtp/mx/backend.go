package mx

import (
	"fmt"
	"log"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/calendar"
	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/notify"
	"github.com/ddletotam/ddmailserver/internal/parser"
	"github.com/ddletotam/ddmailserver/internal/smtp/localdelivery"
	"github.com/emersion/go-smtp"
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
		directory: b.database,
		deliverer: localdelivery.New(b.database, b.hub, b.analyzer, b.calendarSyncTrigger, ITIPHandler(b.database)),
		senderIP:  senderIP,
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

// ITIPHandler returns the iTIP hook for local delivery: calendar messages
// (REQUEST/CANCEL/REPLY/COUNTER as .ics) are applied to the calendar and the
// mail copy is dropped, so they don't surface in the conversation list.
// Local delivery has no account_id (=0), so routing falls to the recipient
// address: a source that authenticates as it, or a local calendar. If
// neither exists the invite is left as mail rather than filed into an
// arbitrary calendar.
func ITIPHandler(database *db.DB) localdelivery.ITIPFunc {
	return func(parsed *parser.ParsedMessage, msgID, userID int64, recipient string) (bool, error) {
		handler := calendar.NewIncomingHandler(database)
		processed, err := handler.ProcessAndDispatch(parsed, userID, 0, []string{recipient})
		if err != nil || !processed {
			return false, err
		}
		if err := database.HardDeleteMessage(msgID); err != nil {
			return false, fmt.Errorf("drop iTIP-consumed message %d: %w", msgID, err)
		}
		return true, nil
	}
}
