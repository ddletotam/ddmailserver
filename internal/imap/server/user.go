package server

import (
	"errors"
	"log"

	"github.com/emersion/go-imap/backend"
	"github.com/yourusername/mailserver/internal/db"
)

var errNotImplemented = errors.New("not implemented")

// User represents an authenticated IMAP user
type User struct {
	username string
	userID   int64
	database *db.DB
}

// Username returns the username
func (u *User) Username() string {
	return u.username
}

// ListMailboxes returns a list of mailboxes
func (u *User) ListMailboxes(subscribed bool) ([]backend.Mailbox, error) {
	log.Printf("Listing mailboxes for user %s (subscribed: %v)", u.username, subscribed)

	// Get all folders for this user
	folders, err := u.database.GetFoldersByUser(u.userID)
	if err != nil {
		log.Printf("Failed to get folders: %v", err)
		return nil, err
	}

	mailboxes := make([]backend.Mailbox, len(folders))
	for i, folder := range folders {
		mailboxes[i] = &Mailbox{
			name:     folder.Name,
			user:     u,
			database: u.database,
			folderID: folder.ID,
		}
	}

	log.Printf("Found %d mailboxes for user %s", len(mailboxes), u.username)
	return mailboxes, nil
}

// GetMailbox returns a mailbox by name
func (u *User) GetMailbox(name string) (backend.Mailbox, error) {
	log.Printf("Getting mailbox %s for user %s", name, u.username)

	// Try to find folder by path
	// We need to search across all user's accounts
	folders, err := u.database.GetFoldersByUser(u.userID)
	if err != nil {
		return nil, err
	}

	for _, folder := range folders {
		if folder.Path == name || folder.Name == name {
			log.Printf("Found mailbox %s (folder ID: %d)", name, folder.ID)
			return &Mailbox{
				name:     folder.Name,
				user:     u,
				database: u.database,
				folderID: folder.ID,
			}, nil
		}
	}

	// If not found, check if this is "INBOX" and create virtual unified inbox
	if name == "INBOX" {
		log.Printf("Creating virtual INBOX for user %s", u.username)
		return &Mailbox{
			name:     "INBOX",
			user:     u,
			database: u.database,
			folderID: 0, // 0 means unified inbox across all accounts
		}, nil
	}

	log.Printf("Mailbox %s not found for user %s", name, u.username)
	return nil, backend.ErrNoSuchMailbox
}

// CreateMailbox creates a new mailbox
func (u *User) CreateMailbox(name string) error {
	log.Printf("CreateMailbox not implemented: %s", name)
	// TODO: Implement mailbox creation if needed
	return errNotImplemented
}

// DeleteMailbox deletes a mailbox
func (u *User) DeleteMailbox(name string) error {
	log.Printf("DeleteMailbox not implemented: %s", name)
	// TODO: Implement mailbox deletion if needed
	return errNotImplemented
}

// RenameMailbox renames a mailbox
func (u *User) RenameMailbox(existingName, newName string) error {
	log.Printf("RenameMailbox not implemented: %s -> %s", existingName, newName)
	// TODO: Implement mailbox renaming if needed
	return errNotImplemented
}

// Logout is called when user logs out
func (u *User) Logout() error {
	log.Printf("User %s logged out", u.username)
	return nil
}
