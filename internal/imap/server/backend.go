package server

import (
	"errors"
	"log"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/notify"
	"github.com/yourusername/mailserver/internal/search"
	"golang.org/x/crypto/bcrypt"
)

// Backend implements IMAP backend with BackendUpdater support for IDLE
type Backend struct {
	database      *db.DB
	hub           *notify.Hub
	updates       chan backend.Update
	searchIndexer *search.Indexer
}

// NewBackend creates a new IMAP backend
func NewBackend(database *db.DB) *Backend {
	return &Backend{
		database: database,
		updates:  make(chan backend.Update, 100),
	}
}

// NewBackendWithHub creates a new IMAP backend with notification hub for IDLE support
func NewBackendWithHub(database *db.DB, hub *notify.Hub) *Backend {
	b := &Backend{
		database: database,
		hub:      hub,
		updates:  make(chan backend.Update, 100),
	}

	// Start listening for notifications if hub is provided
	if hub != nil {
		go b.listenNotifications()
	}

	return b
}

// Updates returns the channel for sending updates to clients (implements BackendUpdater)
func (b *Backend) Updates() <-chan backend.Update {
	log.Printf("IMAP Backend: Updates() method called by go-imap server")
	return b.updates
}

// listenNotifications listens for events from NotifyHub and converts to IMAP updates
func (b *Backend) listenNotifications() {
	if b.hub == nil {
		return
	}

	ch := b.hub.SubscribeAll()
	log.Printf("IMAP Backend: Started listening for notifications")

	for event := range ch {
		log.Printf("IMAP Backend: Received %s event for user %s, mailbox %s, count %d",
			event.Type, event.Username, event.Mailbox, event.Count)

		switch event.Type {
		case notify.EventNewMessage:
			// Create MailboxUpdate with new message count
			// Use empty username and mailbox to broadcast to ALL connections
			// IMPORTANT: Must set Items[StatusMessages] for EXISTS to be written!
			status := &imap.MailboxStatus{
				Name:     event.Mailbox,
				Messages: event.Count,
				Items:    map[imap.StatusItem]interface{}{imap.StatusMessages: nil},
			}
			update := &backend.MailboxUpdate{
				Update:        backend.NewUpdate("", ""),
				MailboxStatus: status,
			}

			// Log channel status before sending
			log.Printf("IMAP Backend: Channel len=%d, cap=%d before send", len(b.updates), cap(b.updates))

			// Send with timeout to detect if channel is being consumed
			select {
			case b.updates <- update:
				log.Printf("IMAP Backend: Sent EXISTS update (messages: %d) - channel len now=%d",
					event.Count, len(b.updates))
			case <-time.After(5 * time.Second):
				log.Printf("IMAP Backend: TIMEOUT - channel not being read! len=%d", len(b.updates))
			}
		}
	}
}

// SetSearchIndexer sets the Meilisearch indexer for full-text search
func (b *Backend) SetSearchIndexer(indexer *search.Indexer) {
	b.searchIndexer = indexer
}

// Login authenticates a user
func (b *Backend) Login(connInfo *imap.ConnInfo, username, password string) (backend.User, error) {
	log.Printf("IMAP login attempt for user: %s", username)

	// Get user from database
	user, err := b.database.GetUserByUsername(username)
	if err != nil {
		log.Printf("User not found: %s", username)
		return nil, errors.New("invalid credentials")
	}

	// Verify password using bcrypt
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		log.Printf("Invalid password for user: %s", username)
		return nil, errors.New("invalid credentials")
	}

	log.Printf("User %s logged in successfully", username)

	return &User{
		username:      username,
		userID:        user.ID,
		database:      b.database,
		searchIndexer: b.searchIndexer,
	}, nil
}
