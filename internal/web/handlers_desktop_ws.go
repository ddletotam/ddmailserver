package web

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/notify"
	"github.com/gorilla/websocket"
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true }, // Desktop client, no CORS
}

// WSEvent is the JSON frame sent to the client over WebSocket.
type WSEvent struct {
	Type       string `json:"type"`                  // "new_message", "message_sent", "delivery_status", "flags_changed", "expunge", "calendar_updated", "identities_changed", "auth_failed", "dialog_folders_changed", "identity_groups_changed"
	UserID     int64  `json:"user_id"`               // For filtering
	Folder     string `json:"folder,omitempty"`      // Mailbox name (mail events)
	Count      uint32 `json:"count,omitempty"`       // Message count for EXISTS (mail events)
	CalendarID int64  `json:"calendar_id,omitempty"` // Affected calendar (calendar events)
	// Toast content for new_message (the LAST new message of a batch).
	From      string `json:"from,omitempty"`
	Subject   string `json:"subject,omitempty"`
	MessageID int64  `json:"message_id,omitempty"` // = the client's native-mode uid
	NewCount  int    `json:"new_count,omitempty"`
	// auth_failed: whose credentials the provider rejects and until when
	// the server pauses logging in (contract §5д-тер).
	Identity      string `json:"identity,omitempty"`
	Name          string `json:"name,omitempty"`
	Service       string `json:"service,omitempty"`
	Since         int64  `json:"since,omitempty"`
	NextAttemptAt int64  `json:"next_attempt_at,omitempty"`
	// message_sent / delivery_status: the Message-ID of the outgoing message
	// (its stable identity, as message_id in conversation refs; message_id
	// above is messages.id).
	RFCMessageID string `json:"rfc_message_id,omitempty"`
	// delivery_status: the message's delivery state now — the same fields as
	// in the message body DTO (contract §1).
	DeliveryStatus string   `json:"delivery_status,omitempty"`
	DeliveryReason string   `json:"delivery_reason,omitempty"`
	DeliveryError  string   `json:"delivery_error,omitempty"`
	DeliveryFailed []string `json:"delivery_failed,omitempty"`
}

// wsToken returns the JWT of a WebSocket handshake. Native clients (desktop
// tungstenite, Android OkHttp) can set headers on the handshake, so the
// Authorization header comes first: a token in the query string ends up in
// proxy access logs. The ?token= query is kept for clients that still send it
// (and for browsers, whose WebSocket API cannot set headers).
func wsToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if parts := strings.SplitN(h, " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			if t := strings.TrimSpace(parts[1]); t != "" {
				return t
			}
		}
	}
	return r.URL.Query().Get("token")
}

// HandleDesktopWebSocket upgrades to WebSocket and streams push events.
// Auth: JWT in the Authorization header (preferred) or the ?token= query.
func (s *Server) HandleDesktopWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.notifyHub == nil {
		http.Error(w, "push not available", http.StatusServiceUnavailable)
		return
	}

	tokenStr := wsToken(r)
	if tokenStr == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}

	claims, err := ValidateToken(tokenStr, s.jwtSecret)
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	user, err := s.database.GetUserByID(claims.UserID)
	if err != nil || user.IsBanned() {
		http.Error(w, "user not found", http.StatusUnauthorized)
		return
	}

	// Upgrade to WebSocket
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	log.Printf("WebSocket: user %s connected", user.Username)

	// Subscribe to events for this user
	eventCh := s.notifyHub.Subscribe(user.ID)
	defer s.notifyHub.Unsubscribe(user.ID, eventCh)

	// Ping ticker to keep connection alive
	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	// Read pump — drain client messages (we don't expect any, but must read to detect close)
	closeCh := make(chan struct{})
	go func() {
		defer close(closeCh)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Write pump — forward events + pings
	for {
		select {
		case event, ok := <-eventCh:
			if !ok {
				return // Channel closed
			}
			wsEvent := WSEvent{
				Type:       string(event.Type),
				UserID:     event.UserID,
				Folder:     event.Mailbox,
				Count:      event.Count,
				CalendarID: event.CalendarID,
				From:       event.From,
				Subject:    event.Subject,
				MessageID:  event.MessageID,
				NewCount:   event.NewCount,

				Identity:      event.Identity,
				Name:          event.SubjectName,
				Service:       event.Service,
				Since:         event.Since,
				NextAttemptAt: event.NextAttemptAt,

				RFCMessageID:   event.RFCMessageID,
				DeliveryStatus: event.DeliveryStatus,
				DeliveryReason: event.DeliveryReason,
				DeliveryFailed: event.DeliveryFailed,
			}
			if event.Type == notify.EventDeliveryStatus {
				wsEvent.DeliveryError = s.deliveryErrorText(user, event.DeliveryReason)
			}
			data, _ := json.Marshal(wsEvent)
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				log.Printf("WebSocket: write error for user %s: %v", user.Username, err)
				return
			}
			log.Printf("WebSocket: pushed %s event to user %s (folder=%s)", event.Type, user.Username, event.Mailbox)

		case <-pingTicker.C:
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-closeCh:
			log.Printf("WebSocket: user %s disconnected", user.Username)
			return
		}
	}
}
