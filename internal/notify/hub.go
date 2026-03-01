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
)

// Event represents a mailbox change notification
type Event struct {
	UserID   int64
	FolderID int64
	Type     EventType
	Count    uint32 // Total message count (for EXISTS)
	Username string // User's login name (for IMAP filtering)
	Mailbox  string // Mailbox name (e.g., "INBOX")
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
