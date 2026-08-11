package notify

import (
	"log"
	"sync"
)

// EventType represents the type of notification event
type EventType string

const (
	EventNewMessage   EventType = "new_message"
	EventFlagsChanged EventType = "flags_changed"
	EventExpunge      EventType = "expunge"

	// EventMessageSent — our own outgoing message has landed in Sent.
	//
	// Deliberately not EventNewMessage: this must refresh the conversation list
	// without a toast or a sound, since the user just pressed Send and knows.
	// Sending is asynchronous, so the copy in Sent appears well after the client
	// was told «отправлено» — without this the client could only guess when to
	// look, and a reply sent from a different address had no conversation to
	// jump to yet (contract §1).
	EventMessageSent EventType = "message_sent"

	EventCalendarUpdated EventType = "calendar_updated"
)

// Event represents a notification about a user-visible change.
// Mail and calendar events share the same hub so a single WS subscription
// covers both. Mail-only fields stay zeroed for calendar events and vice
// versa — JSON serialization in WSEvent uses omitempty to keep frames lean.
type Event struct {
	UserID   int64
	Type     EventType
	Username string // User's login name (for IMAP filtering)

	// Mail fields
	FolderID int64
	Count    uint32 // Total message count (for EXISTS)
	Mailbox  string // Mailbox name (e.g., "INBOX")
	// Toast content for the desktop client (zero for non-mail events).
	// For batch syncs these describe the LAST new message of the batch.
	From      string // From header of the new message
	Subject   string
	MessageID int64 // messages.id — the client's native-mode uid
	NewCount  int   // how many NEW messages this event describes (>=1)

	// Calendar fields
	CalendarID int64 // Affected calendar (0 means "any calendar for this user")
}

// Hub manages pub/sub for mailbox notifications
type Hub struct {
	subscribers map[int64][]chan Event // userID -> channels
	global      []chan Event           // subscribers for all events
	mu          sync.RWMutex
}

// NewHub creates a new notification hub
func NewHub() *Hub {
	return &Hub{
		subscribers: make(map[int64][]chan Event),
		global:      make([]chan Event, 0),
	}
}

// Subscribe creates a channel to receive events for a specific user
// Returns a channel that will receive events
func (h *Hub) Subscribe(userID int64) chan Event {
	h.mu.Lock()
	defer h.mu.Unlock()

	ch := make(chan Event, 100) // Buffered to avoid blocking
	h.subscribers[userID] = append(h.subscribers[userID], ch)

	log.Printf("NotifyHub: User %d subscribed (total subscribers: %d)", userID, len(h.subscribers[userID]))
	return ch
}

// SubscribeAll creates a channel to receive all events (for IMAP backend)
func (h *Hub) SubscribeAll() chan Event {
	h.mu.Lock()
	defer h.mu.Unlock()

	ch := make(chan Event, 100)
	h.global = append(h.global, ch)

	log.Printf("NotifyHub: Global subscriber added (total: %d)", len(h.global))
	return ch
}

// Unsubscribe removes a subscription
func (h *Hub) Unsubscribe(userID int64, ch chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	channels := h.subscribers[userID]
	for i, c := range channels {
		if c == ch {
			h.subscribers[userID] = append(channels[:i], channels[i+1:]...)
			close(ch)
			log.Printf("NotifyHub: User %d unsubscribed", userID)
			return
		}
	}
}

// UnsubscribeAll removes a global subscription
func (h *Hub) UnsubscribeAll(ch chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for i, c := range h.global {
		if c == ch {
			h.global = append(h.global[:i], h.global[i+1:]...)
			close(ch)
			log.Printf("NotifyHub: Global subscriber removed")
			return
		}
	}
}

// Publish sends an event to all relevant subscribers
func (h *Hub) Publish(event Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	log.Printf("NotifyHub: Publishing %s event for user %d, folder %d, count %d",
		event.Type, event.UserID, event.FolderID, event.Count)

	// Send to user-specific subscribers
	if channels, ok := h.subscribers[event.UserID]; ok {
		for _, ch := range channels {
			select {
			case ch <- event:
			default:
				log.Printf("NotifyHub: Channel full, dropping event for user %d", event.UserID)
			}
		}
	}

	// Send to global subscribers (IMAP backend)
	for _, ch := range h.global {
		select {
		case ch <- event:
		default:
			log.Printf("NotifyHub: Global channel full, dropping event")
		}
	}
}
