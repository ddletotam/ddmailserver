package server

import (
	"errors"
	"log"

	"github.com/emersion/go-imap/backend"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/search"
)

var errNotImplemented = errors.New("not implemented")

// User represents an authenticated IMAP user
type User struct {
	username      string
	userID        int64
	database      *db.DB
	searchIndexer *search.Indexer
}

// Username returns the username
func (u *User) Username() string {
	return u.username
}

// ListMailboxes returns a list of mailboxes
func (u *User) ListMailboxes(subscribed bool) ([]backend.Mailbox, error) {
	log.Printf("Listing mailboxes for user %s (subscribed: %v)", u.username, subscribed)

	// Get only local folders (account_id IS NULL)
	folders, err := u.database.GetLocalFoldersByUser(u.userID)
	if err != nil {
		log.Printf("Failed to get folders: %v", err)
		return nil, err
	}

	var mailboxes []backend.Mailbox

	// Add folders - INBOX first if present, then others
	inboxFound := false
	for _, folder := range folders {
		if folder.Name == "INBOX" {
			// Add INBOX first
			mailboxes = append([]backend.Mailbox{&Mailbox{
				name:          "INBOX",
				user:          u,
				database:      u.database,
				folderID:      folder.ID,
				searchIndexer: u.searchIndexer,
			}}, mailboxes...)
			inboxFound = true
		} else {
			mailboxes = append(mailboxes, &Mailbox{
				name:          folder.Name,
				user:          u,
				database:      u.database,
				folderID:      folder.ID,
				searchIndexer: u.searchIndexer,
			})
		}
	}

	// Create INBOX if not found
	if !inboxFound {
		inbox, err := u.database.GetOrCreateLocalInbox(u.userID)
		if err != nil {
			log.Printf("Failed to create INBOX: %v", err)
		} else {
			mailboxes = append([]backend.Mailbox{&Mailbox{
				name:          "INBOX",
				user:          u,
				database:      u.database,
				folderID:      inbox.ID,
				searchIndexer: u.searchIndexer,
			}}, mailboxes...)
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
		log.Printf("Found mailbox %s (folder ID: %d)", name, folder.ID)
		return &Mailbox{
			name:          folder.Name,
			user:          u,
			database:      u.database,
			folderID:      folder.ID,
			searchIndexer: u.searchIndexer,
		}, nil
	}

	// If INBOX not found, create it
	if name == "INBOX" {
		inbox, err := u.database.GetOrCreateLocalInbox(u.userID)
		if err != nil {
			return nil, err
		}
		log.Printf("Created INBOX for user %s (folder ID: %d)", u.username, inbox.ID)
		return &Mailbox{
			name:          "INBOX",
			user:          u,
			database:      u.database,
			folderID:      inbox.ID,
			searchIndexer: u.searchIndexer,
		}, nil
	}

	log.Printf("Mailbox %s not found for user %s", name, u.username)
	return nil, backend.ErrNoSuchMailbox
}

// CreateMailbox creates a new mailbox
func (u *User) CreateMailbox(name string) error {
	log.Printf("Creating mailbox %s for user %s", name, u.username)

	// Check if folder already exists
	_, err := u.database.GetLocalFolderByName(u.userID, name)
	if err == nil {
		return errors.New("mailbox already exists")
	}

	// Create new local folder
	folder := &models.Folder{
		UserID:    u.userID,
		AccountID: 0, // Local folder (will be NULL in DB)
		Name:      name,
		Path:      name,
		Type:      "custom",
		UIDNext:   1,
	}

	if err := u.database.CreateFolder(folder); err != nil {
		log.Printf("Failed to create mailbox: %v", err)
		return err
	}

	log.Printf("Created mailbox %s (folder ID: %d) for user %s", name, folder.ID, u.username)
	return nil
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
