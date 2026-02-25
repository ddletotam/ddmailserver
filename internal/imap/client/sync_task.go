package client

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-message/mail"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/task"
)

// SyncTask represents an IMAP synchronization task
type SyncTask struct {
	account  *models.Account
	database *db.DB
	priority int
}

// NewSyncTask creates a new IMAP sync task
func NewSyncTask(account *models.Account, database *db.DB) *SyncTask {
	return &SyncTask{
		account:  account,
		database: database,
		priority: 1,
	}
}

// Type returns the task type
func (t *SyncTask) Type() task.Type {
	return task.TypeIMAP
}

// Priority returns task priority
func (t *SyncTask) Priority() int {
	return t.priority
}

// String returns a human-readable description
func (t *SyncTask) String() string {
	return fmt.Sprintf("IMAP sync for %s (account %d)", t.account.Email, t.account.ID)
}

// Execute runs the synchronization
func (t *SyncTask) Execute(ctx context.Context) error {
	log.Printf("Starting sync for account %s", t.account.Email)

	// Create IMAP client
	client := &Client{account: t.account}

	// Connect to IMAP server
	if err := client.Connect(); err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	defer client.Disconnect()

	// Check context cancellation
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// List and sync folders
	folders, err := client.ListFolders()
	if err != nil {
		return fmt.Errorf("failed to list folders: %w", err)
	}

	log.Printf("Found %d folders for %s", len(folders), t.account.Email)

	// Sync each folder
	for _, folderInfo := range folders {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Skip some system folders
		if shouldSkipFolder(folderInfo.Name) {
			continue
		}

		if err := t.syncFolder(ctx, client, folderInfo); err != nil {
			log.Printf("Failed to sync folder %s: %v", folderInfo.Name, err)
			// Continue with other folders even if one fails
			continue
		}
	}

	// Update last sync time
	if err := t.database.UpdateAccountLastSync(t.account.ID, time.Now()); err != nil {
		log.Printf("Failed to update last sync time: %v", err)
	}

	log.Printf("Completed sync for account %s", t.account.Email)
	return nil
}

// syncFolder synchronizes a single folder
func (t *SyncTask) syncFolder(ctx context.Context, client *Client, folderInfo *imap.MailboxInfo) error {
	log.Printf("Syncing folder %s for %s", folderInfo.Name, t.account.Email)

	// Select the folder
	mbox, err := client.SelectFolder(folderInfo.Name)
	if err != nil {
		return err
	}

	// Get or create folder in database
	folder, err := t.getOrCreateFolder(folderInfo.Name)
	if err != nil {
		return err
	}

	// If mailbox is empty, nothing to do
	if mbox.Messages == 0 {
		log.Printf("Folder %s is empty", folderInfo.Name)
		return nil
	}

	// For now, sync only recent messages (last 100)
	// TODO: Implement incremental sync based on UIDs
	from := uint32(1)
	to := mbox.Messages
	if mbox.Messages > 100 {
		from = mbox.Messages - 99
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddRange(from, to)

	// Fetch messages
	section := &imap.BodySectionName{}
	items := []imap.FetchItem{
		imap.FetchEnvelope,
		imap.FetchFlags,
		imap.FetchUid,
		section.FetchItem(),
	}

	messages, err := client.FetchMessages(seqSet, items)
	if err != nil {
		return err
	}

	messageCount := 0
	for msg := range messages {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if err := t.saveMessage(msg, folder); err != nil {
			log.Printf("Failed to save message UID %d: %v", msg.Uid, err)
			// Continue with other messages
			continue
		}
		messageCount++
	}

	log.Printf("Synced %d messages from folder %s", messageCount, folderInfo.Name)
	return nil
}

// getOrCreateFolder gets or creates a folder in the database
func (t *SyncTask) getOrCreateFolder(path string) (*models.Folder, error) {
	// Try to find existing folder
	folder, err := t.database.GetFolderByPath(t.account.ID, path)
	if err == nil {
		return folder, nil
	}

	// Create new folder
	folderType := determineFolderType(path)
	folder = &models.Folder{
		UserID:    t.account.UserID,
		AccountID: t.account.ID,
		Name:      path,
		Path:      path,
		Type:      folderType,
		UIDNext:   1,
	}

	if err := t.database.CreateFolder(folder); err != nil {
		return nil, err
	}

	return folder, nil
}

// saveMessage saves a message to the database
func (t *SyncTask) saveMessage(imapMsg *imap.Message, folder *models.Folder) error {
	// Check if message already exists
	// TODO: Check by UID instead of fetching all messages

	var body string
	var bodyHTML string

	// Parse message body
	// Get the full message body
	var rfc822Body io.Reader
	for _, literal := range imapMsg.Body {
		// Take the first body section (should be the full message)
		rfc822Body = literal
		break
	}

	if rfc822Body != nil {
		mr, err := mail.CreateReader(rfc822Body)
		if err != nil {
			return fmt.Errorf("failed to create mail reader: %w", err)
		}

		// Process each part
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Printf("Error reading part: %v", err)
				break
			}

			switch h := p.Header.(type) {
			case *mail.InlineHeader:
				contentType, _, _ := h.ContentType()
				bodyBytes, _ := io.ReadAll(p.Body)

				if contentType == "text/plain" {
					body = string(bodyBytes)
				} else if contentType == "text/html" {
					bodyHTML = string(bodyBytes)
				}
			}
		}
	}

	// Create message model
	msg := &models.Message{
		AccountID: t.account.ID,
		UserID:    t.account.UserID,
		FolderID:  folder.ID,
		MessageID: imapMsg.Envelope.MessageId,
		Subject:   imapMsg.Envelope.Subject,
		From:      formatAddressList(imapMsg.Envelope.From),
		To:        formatAddressList(imapMsg.Envelope.To),
		Cc:        formatAddressList(imapMsg.Envelope.Cc),
		Bcc:       formatAddressList(imapMsg.Envelope.Bcc),
		ReplyTo:   formatAddressList(imapMsg.Envelope.ReplyTo),
		Date:      imapMsg.Envelope.Date,
		Body:      body,
		BodyHTML:  bodyHTML,
		UID:       imapMsg.Uid,
		Seen:      hasFlag(imapMsg.Flags, imap.SeenFlag),
		Flagged:   hasFlag(imapMsg.Flags, imap.FlaggedFlag),
		Answered:  hasFlag(imapMsg.Flags, imap.AnsweredFlag),
		Draft:     hasFlag(imapMsg.Flags, imap.DraftFlag),
		Deleted:   hasFlag(imapMsg.Flags, imap.DeletedFlag),
		InReplyTo: imapMsg.Envelope.InReplyTo,
	}

	// Save to database
	if err := t.database.CreateMessage(msg); err != nil {
		return err
	}

	return nil
}

// Helper functions

func shouldSkipFolder(name string) bool {
	// Skip some common system folders that might cause issues
	skipList := []string{"[Gmail]/All Mail", "[Gmail]/Trash", "[Gmail]/Spam"}
	for _, skip := range skipList {
		if name == skip {
			return true
		}
	}
	return false
}

func determineFolderType(path string) string {
	switch path {
	case "INBOX":
		return "inbox"
	case "Sent", "Sent Items", "[Gmail]/Sent Mail":
		return "sent"
	case "Drafts", "[Gmail]/Drafts":
		return "drafts"
	case "Trash", "[Gmail]/Trash":
		return "trash"
	case "Junk", "Spam", "[Gmail]/Spam":
		return "junk"
	case "Archive", "[Gmail]/All Mail":
		return "archive"
	default:
		return "custom"
	}
}

func formatAddressList(addresses []*imap.Address) string {
	if len(addresses) == 0 {
		return ""
	}

	result := ""
	for i, addr := range addresses {
		if i > 0 {
			result += ", "
		}
		if addr.PersonalName != "" {
			result += addr.PersonalName + " "
		}
		result += fmt.Sprintf("<%s@%s>", addr.MailboxName, addr.HostName)
	}
	return result
}

func hasFlag(flags []string, flag string) bool {
	for _, f := range flags {
		if f == flag {
			return true
		}
	}
	return false
}
