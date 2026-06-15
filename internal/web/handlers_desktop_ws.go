package web

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true }, // Desktop client, no CORS
}

// WSEvent is the JSON frame sent to the client over WebSocket.
type WSEvent struct {
	Type       string `json:"type"`                  // "new_message", "flags_changed", "expunge", "calendar_updated"
	UserID     int64  `json:"user_id"`               // For filtering
	Folder     string `json:"folder,omitempty"`      // Mailbox name (mail events)
	Count      uint32 `json:"count,omitempty"`       // Message count for EXISTS (mail events)
	CalendarID int64  `json:"calendar_id,omitempty"` // Affected calendar (calendar events)
	// Toast content for new_message (the LAST new message of a batch).
	From      string `json:"from,omitempty"`
	Subject   string `json:"subject,omitempty"`
	MessageID int64  `json:"message_id,omitempty"` // = the client's native-mode uid
	NewCount  int    `json:"new_count,omitempty"`
}

// HandleDesktopWebSocket upgrades to WebSocket and streams push events.
// Auth: JWT passed as ?token= query parameter (WebSocket can't send headers).
func (s *Server) HandleDesktopWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.notifyHub == nil {
		http.Error(w, "push not available", http.StatusServiceUnavailable)
		return
	}

	// Authenticate via query param (WebSocket clients can't set Authorization header)
	tokenStr := r.URL.Query().Get("token")
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
