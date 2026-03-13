package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/emersion/go-imap"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/parser"
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

	// PEEK to avoid marking messages as seen on source server
	section := &imap.BodySectionName{Peek: true}
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
	// Skip messages with no envelope data (corrupted or incomplete fetch)
	if imapMsg.Envelope == nil {
		log.Printf("IMAP sync: Skipping message UID %d - no envelope data", imapMsg.Uid)
		return false, nil
	}

	// Validate that we have at least some basic envelope data
	// A message without From and Subject is likely corrupted
	if len(imapMsg.Envelope.From) == 0 && imapMsg.Envelope.Subject == "" {
		log.Printf("IMAP sync: Skipping message UID %d - empty envelope (no from, no subject)", imapMsg.Uid)
		return false, nil
	}

	// Get or generate message_id for deduplication
	messageID := imapMsg.Envelope.MessageId
	if messageID == "" {
		// Generate synthetic message_id from content hash
		// This ensures deduplication even for messages without Message-ID header
		h := sha256.New()
		h.Write([]byte(imapMsg.Envelope.Subject))
		h.Write([]byte(formatAddressList(imapMsg.Envelope.From)))
		h.Write([]byte(imapMsg.Envelope.Date.Format(time.RFC3339)))
		h.Write([]byte(fmt.Sprintf("%d", imapMsg.Uid)))
		messageID = fmt.Sprintf("<%s@generated.local>", hex.EncodeToString(h.Sum(nil))[:32])
	}

	// Check if message already exists by message_id
	exists, err := t.database.MessageExistsByMessageID(t.account.UserID, messageID)
	if err != nil {
		return false, err
	}
	if exists {
		// Message exists - try to update remote_uid if missing (for bidirectional sync)
		if imapMsg.Uid > 0 {
			updated, err := t.database.UpdateMessageRemoteUID(t.account.UserID, messageID, imapMsg.Uid, "INBOX")
			if err != nil {
				log.Printf("IMAP sync: Failed to update remote_uid for %s: %v", messageID, err)
			} else if updated {
				log.Printf("IMAP sync: Updated remote_uid=%d for existing message %s", imapMsg.Uid, messageID)
			}
		}
		return false, nil // Skip duplicate (content already exists)
	}

	// Parse message body using new parser
	var body string
	var bodyHTML string
	var attachments []parser.ParsedAttachment

	var rfc822Body io.Reader
	for _, literal := range imapMsg.Body {
		rfc822Body = literal
		break
	}

	if rfc822Body != nil {
		p := parser.New()
		parsed, err := p.Parse(rfc822Body)
		if err == nil {
			body = parsed.Body
			bodyHTML = parsed.BodyHTML
			attachments = parsed.Attachments

			// Log embedded messages if any
			if len(parsed.EmbeddedMessages) > 0 {
				log.Printf("IMAP sync: Message contains %d embedded message(s)", len(parsed.EmbeddedMessages))
			}
		} else {
			log.Printf("IMAP sync: Failed to parse message body: %v", err)
		}
	}

	// Get next local UID
	localUID, err := t.database.GetNextUIDForFolder(inbox.ID)
	if err != nil {
		return false, fmt.Errorf("failed to get next UID: %w", err)
	}

	// Use envelope date, fall back to current time if zero/invalid
	msgDate := imapMsg.Envelope.Date.UTC()
	if msgDate.Year() < 1970 {
		// Date is likely corrupted (e.g., 0001-01-01), use current time
		msgDate = time.Now().UTC()
		log.Printf("IMAP sync: Message UID %d has invalid date, using current time", imapMsg.Uid)
	}

	// Create message with sanitized UTF-8 strings
	msg := &models.Message{
		AccountID:    t.account.ID,
		UserID:       t.account.UserID,
		FolderID:     inbox.ID,
		MessageID:    messageID,
		Subject:      parser.SanitizeUTF8(imapMsg.Envelope.Subject),
		From:         parser.SanitizeUTF8(formatAddressList(imapMsg.Envelope.From)),
		To:           parser.SanitizeUTF8(formatAddressList(imapMsg.Envelope.To)),
		Cc:           parser.SanitizeUTF8(formatAddressList(imapMsg.Envelope.Cc)),
		Bcc:          parser.SanitizeUTF8(formatAddressList(imapMsg.Envelope.Bcc)),
		ReplyTo:      parser.SanitizeUTF8(formatAddressList(imapMsg.Envelope.ReplyTo)),
		Date:         msgDate,
		Body:         parser.SanitizeUTF8(body),
		BodyHTML:     parser.SanitizeUTF8(bodyHTML),
		UID:          localUID,
		Seen:         hasFlag(imapMsg.Flags, imap.SeenFlag),
		Flagged:      hasFlag(imapMsg.Flags, imap.FlaggedFlag),
		Answered:     hasFlag(imapMsg.Flags, imap.AnsweredFlag),
		Draft:        hasFlag(imapMsg.Flags, imap.DraftFlag),
		Deleted:      hasFlag(imapMsg.Flags, imap.DeletedFlag),
		InReplyTo:    parser.SanitizeUTF8(imapMsg.Envelope.InReplyTo),
		RemoteUID:    imapMsg.Uid, // Store remote UID for bidirectional sync
		RemoteFolder: "INBOX",     // Currently we only sync INBOX
	}

	if err := t.database.CreateMessage(msg); err != nil {
		return false, err
	}

	// Save attachments
	for _, att := range attachments {
		attachment := &models.Attachment{
			MessageID:   msg.ID,
			Filename:    att.Filename,
			ContentType: att.ContentType,
			Size:        att.Size,
			Data:        att.Data,
		}
		if err := t.database.CreateAttachment(attachment); err != nil {
			log.Printf("IMAP sync: Failed to save attachment %s: %v", att.Filename, err)
		}
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
