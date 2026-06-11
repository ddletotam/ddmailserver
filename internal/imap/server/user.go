package server

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/emersion/go-imap/backend"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/search"
)

// User represents an authenticated IMAP user
type User struct {
	username       string
	userID         int64
	database       *db.DB
	searchIndexer  *search.Indexer
	bodyCache      *bodyCache
	backend        *Backend
	foldersEnsured bool
}

// Username returns the username
func (u *User) Username() string {
	return u.username
}

// mailbox builds a Mailbox bound to this user's session dependencies.
func (u *User) mailbox(name, folderType string, folderID int64) *Mailbox {
	return &Mailbox{
		name:          name,
		folderType:    folderType,
		user:          u,
		database:      u.database,
		folderID:      folderID,
		searchIndexer: u.searchIndexer,
		bodyCache:     u.bodyCache,
		backend:       u.backend,
	}
}

// ListMailboxes returns a list of mailboxes
func (u *User) ListMailboxes(subscribed bool) ([]backend.Mailbox, error) {
	log.Printf("Listing mailboxes for user %s (subscribed: %v)", u.username, subscribed)

	// Ensure default folders exist (once per session)
	if !u.foldersEnsured {
		if err := u.database.EnsureDefaultFolders(u.userID); err != nil {
			log.Printf("Failed to ensure default folders: %v", err)
		}
		u.foldersEnsured = true
	}

	// Get all local folders
	folders, err := u.database.GetLocalFoldersByUser(u.userID)
	if err != nil {
		log.Printf("Failed to get folders: %v", err)
		return nil, err
	}

	// If subscribed-only, get subscription set
	var subscribedIDs map[int64]bool
	if subscribed {
		subscribedIDs, err = u.database.GetSubscribedFolderIDs(u.userID)
		if err != nil {
			log.Printf("Failed to get subscriptions: %v", err)
			return nil, err
		}
	}

	var mailboxes []backend.Mailbox
	for _, folder := range folders {
		// Filter by subscription if requested
		if subscribed && !subscribedIDs[folder.ID] {
			continue
		}

		mb := u.mailbox(folder.Name, folder.Type, folder.ID)

		// INBOX goes first
		if folder.Type == "inbox" {
			mailboxes = append([]backend.Mailbox{mb}, mailboxes...)
		} else {
			mailboxes = append(mailboxes, mb)
		}
	}

	log.Printf("Found %d mailboxes for user %s", len(mailboxes), u.username)
	return mailboxes, nil
}

// GetMailbox returns a mailbox by name
func (u *User) GetMailbox(name string) (backend.Mailbox, error) {
	log.Printf("Getting mailbox %s for user %s", name, u.username)

	// Try to find local folder by name
	folder, err := u.database.GetLocalFolderByName(u.userID, name)
	if err == nil {
		return u.mailbox(folder.Name, folder.Type, folder.ID), nil
	}

	// INBOX auto-create
	if strings.EqualFold(name, "INBOX") {
		inbox, err := u.database.GetOrCreateLocalFolder(u.userID, "INBOX", "inbox")
		if err != nil {
			return nil, err
		}
		return u.mailbox("INBOX", "inbox", inbox.ID), nil
	}

	return nil, backend.ErrNoSuchMailbox
}

// inferFolderType maps well-known mailbox names to folder types
func inferFolderType(name string) string {
	lower := strings.ToLower(name)
	switch {
	case lower == "drafts" || lower == "draft":
		return "drafts"
	case lower == "archive" || lower == "archives":
		return "archive"
	case lower == "junk" || lower == "spam":
		return "junk"
	case lower == "sent" || lower == "sent messages" || lower == "sent items":
		return "sent"
	case lower == "trash" || lower == "deleted" || lower == "deleted items" || lower == "deleted messages":
		return "trash"
	default:
		return "custom"
	}
}

// protectedTypes cannot be created or deleted by clients
var protectedTypes = map[string]bool{
	"inbox": true,
	"sent":  true,
	"trash": true,
}

// CreateMailbox creates a new mailbox
func (u *User) CreateMailbox(name string) error {
	log.Printf("Creating mailbox %s for user %s", name, u.username)

	// Check if folder already exists
	existing, _ := u.database.GetFolderByNameAndUser(u.userID, name)
	if existing != nil {
		return errors.New("mailbox already exists")
	}

	folderType := inferFolderType(name)

	// Reject creating protected types (they are auto-created)
	if protectedTypes[folderType] {
		return fmt.Errorf("cannot create %s mailbox manually", folderType)
	}

	folder, err := u.database.GetOrCreateLocalFolder(u.userID, name, folderType)
	if err != nil {
		return err
	}

	log.Printf("Created mailbox %s (type=%s, id=%d) for user %s", name, folderType, folder.ID, u.username)
	return nil
}

// DeleteMailbox deletes a mailbox
func (u *User) DeleteMailbox(name string) error {
	log.Printf("Deleting mailbox %s for user %s", name, u.username)

	folder, err := u.database.GetLocalFolderByName(u.userID, name)
	if err != nil {
		return backend.ErrNoSuchMailbox
	}

	// Cannot delete protected folders
	if protectedTypes[folder.Type] {
		return fmt.Errorf("cannot delete %s folder", folder.Type)
	}

	if err := u.database.DeleteFolder(folder.ID); err != nil {
		return err
	}

	log.Printf("Deleted mailbox %s for user %s", name, u.username)
	return nil
}

// RenameMailbox renames a mailbox
func (u *User) RenameMailbox(existingName, newName string) error {
	log.Printf("Renaming mailbox %s -> %s for user %s", existingName, newName, u.username)

	folder, err := u.database.GetLocalFolderByName(u.userID, existingName)
	if err != nil {
		return backend.ErrNoSuchMailbox
	}

	// Check new name doesn't conflict
	existing, _ := u.database.GetFolderByNameAndUser(u.userID, newName)
	if existing != nil {
		return errors.New("destination mailbox already exists")
	}

	folder.Name = newName
	folder.Path = newName
	if err := u.database.UpdateFolder(folder); err != nil {
		return err
	}

	log.Printf("Renamed mailbox %s -> %s for user %s", existingName, newName, u.username)
	return nil
}

// Logout is called when user logs out
func (u *User) Logout() error {
	log.Printf("User %s logged out", u.username)
	return nil
}
