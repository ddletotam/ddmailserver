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

	// Get user's local INBOX (all messages go here)
	localInbox, err := t.database.GetOrCreateLocalInbox(t.account.UserID)
	if err != nil {
		return fmt.Errorf("failed to get local inbox: %w", err)
	}

	// Sync only INBOX from remote server
	// All messages go to user's local INBOX
	if err := t.syncRemoteInbox(ctx, client, localInbox); err != nil {
		log.Printf("Failed to sync INBOX: %v", err)
	}

	// Update last sync time
	if err := t.database.UpdateAccountLastSync(t.account.ID, time.Now()); err != nil {
		log.Printf("Failed to update last sync time: %v", err)
	}

	log.Printf("Completed sync for account %s", t.account.Email)
	return nil
}

// syncRemoteInbox syncs INBOX from remote server to user's local INBOX
func (t *SyncTask) syncRemoteInbox(ctx context.Context, client *Client, localInbox *models.Folder) error {
	log.Printf("Syncing remote INBOX for %s to local inbox (folder %d)", t.account.Email, localInbox.ID)

	// Select INBOX on remote server
	mbox, err := client.SelectFolder("INBOX")
	if err != nil {
		return err
	}

	// If mailbox is empty, nothing to do
	if mbox.Messages == 0 {
		log.Printf("Remote INBOX is empty for %s", t.account.Email)
		return nil
	}

	// Fetch all messages (we use message_id for deduplication, not UIDs)
	// For simplicity, fetch last 100 messages on each sync
	// TODO: implement proper incremental sync tracking per account
	uidSet := new(imap.SeqSet)
	uidSet.AddRange(1, 0) // Fetch all

	// Fetch messages by UID
	section := &imap.BodySectionName{}
	items := []imap.FetchItem{
		imap.FetchEnvelope,
		imap.FetchFlags,
		imap.FetchUid,
		section.FetchItem(),
	}

	messages, fetchDone := client.FetchMessagesByUID(uidSet, items)

	messageCount := 0
	skippedCount := 0
	for msg := range messages {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		saved, err := t.saveMessageToInbox(msg, localInbox)
		if err != nil {
			log.Printf("Failed to save message: %v", err)
			continue
		}
		if saved {
			messageCount++
		} else {
			skippedCount++
		}
	}

	// Check for fetch errors
	if err := <-fetchDone; err != nil {
		return fmt.Errorf("IMAP fetch failed: %w", err)
	}

	log.Printf("Synced %d new messages from %s (skipped %d duplicates)", messageCount, t.account.Email, skippedCount)
	return nil
}

// saveMessageToInbox saves a message to user's local INBOX with deduplication
func (t *SyncTask) saveMessageToInbox(imapMsg *imap.Message, inbox *models.Folder) (bool, error) {
	// Check if message already exists by message_id
	messageID := imapMsg.Envelope.MessageId
	if messageID != "" {
		exists, err := t.database.MessageExistsByMessageID(t.account.UserID, messageID)
		if err != nil {
			return false, err
		}
		if exists {
			return false, nil // Skip duplicate
		}
	}

	// Parse message body
	var body string
	var bodyHTML string

	var rfc822Body io.Reader
	for _, literal := range imapMsg.Body {
		rfc822Body = literal
		break
	}

	if rfc822Body != nil {
		mr, err := mail.CreateReader(rfc822Body)
		if err == nil {
			for {
				p, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
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
	}

	// Get next local UID
	localUID, err := t.database.GetNextUIDForFolder(inbox.ID)
	if err != nil {
		return false, fmt.Errorf("failed to get next UID: %w", err)
	}

	// Create message
	msg := &models.Message{
		AccountID: t.account.ID,
		UserID:    t.account.UserID,
		FolderID:  inbox.ID,
		MessageID: messageID,
		Subject:   imapMsg.Envelope.Subject,
		From:      formatAddressList(imapMsg.Envelope.From),
		To:        formatAddressList(imapMsg.Envelope.To),
		Cc:        formatAddressList(imapMsg.Envelope.Cc),
		Bcc:       formatAddressList(imapMsg.Envelope.Bcc),
		ReplyTo:   formatAddressList(imapMsg.Envelope.ReplyTo),
		Date:      imapMsg.Envelope.Date,
		Body:      body,
		BodyHTML:  bodyHTML,
		UID:       localUID,
		Seen:      hasFlag(imapMsg.Flags, imap.SeenFlag),
		Flagged:   hasFlag(imapMsg.Flags, imap.FlaggedFlag),
		Answered:  hasFlag(imapMsg.Flags, imap.AnsweredFlag),
		Draft:     hasFlag(imapMsg.Flags, imap.DraftFlag),
		Deleted:   hasFlag(imapMsg.Flags, imap.DeletedFlag),
		InReplyTo: imapMsg.Envelope.InReplyTo,
	}

	if err := t.database.CreateMessage(msg); err != nil {
		return false, err
	}

	return true, nil
}

// Helper functions

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
