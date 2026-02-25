package server

import (
	"errors"
	"log"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/yourusername/mailserver/internal/db"
)

// Backend implements IMAP backend
type Backend struct {
	database *db.DB
}

// NewBackend creates a new IMAP backend
func NewBackend(database *db.DB) *Backend {
	return &Backend{
		database: database,
	}
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

	// TODO: Implement proper password hashing verification
	// For now, doing simple comparison (THIS IS NOT SECURE - FIX THIS!)
	if user.PasswordHash != password {
		log.Printf("Invalid password for user: %s", username)
		return nil, errors.New("invalid credentials")
	}

	log.Printf("User %s logged in successfully", username)

	return &User{
		username: username,
		userID:   user.ID,
		database: b.database,
	}, nil
}
