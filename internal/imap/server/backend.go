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
)

// Backend implements IMAP backend with BackendUpdater support for IDLE
type Backend struct {
	database      *db.DB
	hub           *notify.Hub
	updates       chan backend.Update
	searchIndexer *search.Indexer
	bodyCache     *bodyCache
}

// NewBackend creates a new IMAP backend
func NewBackend(database *db.DB) *Backend {
	return &Backend{
		database:  database,
		updates:   make(chan backend.Update, 100),
		bodyCache: newBodyCache(),
	}
}

// NewBackendWithHub creates a new IMAP backend with notification hub for IDLE support
func NewBackendWithHub(database *db.DB, hub *notify.Hub) *Backend {
	b := &Backend{
		database:  database,
		hub:       hub,
		updates:   make(chan backend.Update, 100),
		bodyCache: newBodyCache(),
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
			// IMPORTANT: Must set Items[StatusMessages] for EXISTS to be written!
			status := &imap.MailboxStatus{
				Name:     event.Mailbox,
				Messages: event.Count,
				Items:    map[imap.StatusItem]interface{}{imap.StatusMessages: nil},
			}
			// Try both: targeted update for the specific user, and broadcast
			// go-imap routes updates by matching username to authenticated connections
			update := &backend.MailboxUpdate{
				Update:        backend.NewUpdate(event.Username, ""),
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

// sendUpdate pushes an update to connected sessions. go-imap routes it to
// every connection of the matching user with the matching mailbox selected —
// including the originator, which is what RFC 3501 expects for untagged
// EXPUNGE/FETCH (the server core suppresses its own responses whenever a
// backend exposes an Updates() channel, so the backend MUST emit these).
func (b *Backend) sendUpdate(update backend.Update) {
	select {
	case b.updates <- update:
	case <-time.After(5 * time.Second):
		log.Printf("IMAP Backend: update channel not consumed, dropping %T", update)
	}
}

// notifyExpunge sends untagged EXPUNGE updates for the given sequence
// numbers. seqNums MUST be in descending order so each value stays valid
// while earlier (higher) ones are being applied by the client.
func (b *Backend) notifyExpunge(username, mailbox string, seqNums []uint32) {
	for _, n := range seqNums {
		b.sendUpdate(&backend.ExpungeUpdate{
			Update: backend.NewUpdate(username, mailbox),
			SeqNum: n,
		})
	}
}

// notifyFlags sends an untagged FETCH (FLAGS UID) update after a flag change
// so every session of the user — and the non-silent originator — sees it.
func (b *Backend) notifyFlags(username, mailbox string, seqNum, uid uint32, flags []string) {
	msg := imap.NewMessage(seqNum, []imap.FetchItem{imap.FetchFlags, imap.FetchUid})
	msg.Flags = flags
	msg.Uid = uid
	b.sendUpdate(&backend.MessageUpdate{
		Update:  backend.NewUpdate(username, mailbox),
		Message: msg,
	})
}

// Login authenticates a user
func (b *Backend) Login(connInfo *imap.ConnInfo, username, password string) (backend.User, error) {
	log.Printf("IMAP login attempt for user: %s", username)

	// Accepts the account password or an application password; also strips the
	// @domain part, which New Outlook and some other clients insist on sending.
	user, err := b.database.AuthenticateProtocol(username, password)
	if err != nil {
		log.Printf("IMAP login failed for user: %s", username)
		return nil, errors.New("invalid credentials")
	}
	username = user.Username

	log.Printf("User %s logged in successfully", username)

	return &User{
		username:      username,
		userID:        user.ID,
		database:      b.database,
		searchIndexer: b.searchIndexer,
		bodyCache:     b.bodyCache,
		backend:       b,
	}, nil
}
