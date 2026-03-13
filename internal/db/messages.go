package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
	"github.com/yourusername/mailserver/internal/models"
)

// CreateMessage creates a new message
func (db *DB) CreateMessage(msg *models.Message) error {
	msg.CreatedAt = time.Now()
	msg.UpdatedAt = time.Now()

	// Default spam status if not set
	if msg.SpamStatus == "" {
		msg.SpamStatus = "clean"
	}

	query := `
		INSERT INTO messages (
			account_id, user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
			date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
			in_reply_to, message_references, spam_score, spam_status, spam_reasons,
			remote_uid, remote_folder, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30)
		RETURNING id
	`

	// Use NULL for account_id = 0 (local delivery)
	var accountID sql.NullInt64
	if msg.AccountID > 0 {
		accountID.Int64 = msg.AccountID
		accountID.Valid = true
	}

	// Use NULL for remote_uid = 0 (local messages)
	var remoteUID sql.NullInt64
	if msg.RemoteUID > 0 {
		remoteUID.Int64 = int64(msg.RemoteUID)
		remoteUID.Valid = true
	}

	// Default remote_folder to INBOX if not set
	remoteFolder := msg.RemoteFolder
	if remoteFolder == "" {
		remoteFolder = "INBOX"
	}

	err := db.QueryRow(
		query,
		accountID, msg.UserID, msg.FolderID, msg.MessageID, msg.Subject,
		msg.From, msg.To, msg.Cc, msg.Bcc, msg.ReplyTo,
		msg.Date, msg.Body, msg.BodyHTML, msg.Attachments, msg.Size,
		msg.UID, msg.Seen, msg.Flagged, msg.Answered, msg.Draft, msg.Deleted,
		msg.InReplyTo, msg.MessageReferences, msg.SpamScore, msg.SpamStatus, msg.SpamReasons,
		remoteUID, remoteFolder, msg.CreatedAt, msg.UpdatedAt,
	).Scan(&msg.ID)

	if err != nil {
		return fmt.Errorf("failed to create message: %w", err)
	}

	return nil
}

// GetMessagesByFolder retrieves messages in a folder (excludes soft deleted)
// IMPORTANT: Order by UID ASC for correct IMAP sequence number mapping
func (db *DB) GetMessagesByFolder(folderID int64, limit, offset int) ([]*models.Message, error) {
	query := `
		SELECT id, COALESCE(account_id, 0), user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, COALESCE(spam_score, 0), COALESCE(spam_status, 'clean'), COALESCE(spam_reasons, ''),
		       COALESCE(remote_uid, 0), COALESCE(remote_folder, 'INBOX'), created_at, updated_at
		FROM messages
		WHERE folder_id = $1 AND deleted = false AND (soft_deleted = false OR soft_deleted IS NULL)
		ORDER BY uid ASC
		LIMIT $2 OFFSET $3
	`

	rows, err := db.Query(query, folderID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to get messages: %w", err)
	}
	defer rows.Close()

	return scanMessages(rows)
}

// GetMessagesByUser retrieves all messages for a user
func (db *DB) GetMessagesByUser(userID int64, limit, offset int) ([]*models.Message, error) {
	query := `
		SELECT id, COALESCE(account_id, 0), user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, COALESCE(spam_score, 0), COALESCE(spam_status, 'clean'), COALESCE(spam_reasons, ''),
		       COALESCE(remote_uid, 0), COALESCE(remote_folder, 'INBOX'), created_at, updated_at
		FROM messages
		WHERE user_id = $1 AND deleted = false AND (soft_deleted = false OR soft_deleted IS NULL)
		ORDER BY date DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := db.Query(query, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to get messages: %w", err)
	}
	defer rows.Close()

	return scanMessages(rows)
}

// GetMessageByID retrieves a message by ID
func (db *DB) GetMessageByID(id int64) (*models.Message, error) {
	msg := &models.Message{}
	query := `
		SELECT id, COALESCE(account_id, 0), user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, COALESCE(spam_score, 0), COALESCE(spam_status, 'clean'), COALESCE(spam_reasons, ''),
		       COALESCE(remote_uid, 0), COALESCE(remote_folder, 'INBOX'), created_at, updated_at
		FROM messages
		WHERE id = $1
	`

	err := db.QueryRow(query, id).Scan(
		&msg.ID, &msg.AccountID, &msg.UserID, &msg.FolderID, &msg.MessageID, &msg.Subject,
		&msg.From, &msg.To, &msg.Cc, &msg.Bcc, &msg.ReplyTo,
		&msg.Date, &msg.Body, &msg.BodyHTML, &msg.Attachments, &msg.Size,
		&msg.UID, &msg.Seen, &msg.Flagged, &msg.Answered, &msg.Draft, &msg.Deleted,
		&msg.InReplyTo, &msg.MessageReferences, &msg.SpamScore, &msg.SpamStatus, &msg.SpamReasons,
		&msg.RemoteUID, &msg.RemoteFolder, &msg.CreatedAt, &msg.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("message not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get message: %w", err)
	}

	return msg, nil
}

// UpdateMessageFlags updates message flags
func (db *DB) UpdateMessageFlags(id int64, seen, flagged, answered, deleted bool) error {
	query := `
		UPDATE messages
		SET seen = $1, flagged = $2, answered = $3, deleted = $4, updated_at = $5
		WHERE id = $6
	`

	_, err := db.Exec(query, seen, flagged, answered, deleted, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to update message flags: %w", err)
	}

	return nil
}

// UpdateMessage updates a message
func (db *DB) UpdateMessage(msg *models.Message) error {
	msg.UpdatedAt = time.Now()

	query := `
		UPDATE messages SET
			seen = $1, flagged = $2, answered = $3, draft = $4, deleted = $5, updated_at = $6
		WHERE id = $7
	`

	_, err := db.Exec(query, msg.Seen, msg.Flagged, msg.Answered, msg.Draft, msg.Deleted, msg.UpdatedAt, msg.ID)
	if err != nil {
		return fmt.Errorf("failed to update message: %w", err)
	}

	return nil
}

// DeleteMessage deletes a message
func (db *DB) DeleteMessage(id int64) error {
	query := `DELETE FROM messages WHERE id = $1`
	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete message: %w", err)
	}
	return nil
}

// SearchMessages searches messages by query
func (db *DB) SearchMessages(userID int64, query string, limit, offset int) ([]*models.Message, error) {
	searchQuery := `
		SELECT id, COALESCE(account_id, 0), user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, COALESCE(spam_score, 0), COALESCE(spam_status, 'clean'), COALESCE(spam_reasons, ''),
		       COALESCE(remote_uid, 0), COALESCE(remote_folder, 'INBOX'), created_at, updated_at
		FROM messages
		WHERE user_id = $1 AND deleted = false AND (soft_deleted = false OR soft_deleted IS NULL)
		AND (subject ILIKE $2 OR from_addr ILIKE $2 OR to_addr ILIKE $2 OR body ILIKE $2)
		ORDER BY date DESC
		LIMIT $3 OFFSET $4
	`

	rows, err := db.Query(searchQuery, userID, "%"+query+"%", limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to search messages: %w", err)
	}
	defer rows.Close()

	return scanMessages(rows)
}

// Helper function to scan multiple messages
func scanMessages(rows *sql.Rows) ([]*models.Message, error) {
	var messages []*models.Message

	for rows.Next() {
		msg := &models.Message{}
		err := rows.Scan(
			&msg.ID, &msg.AccountID, &msg.UserID, &msg.FolderID, &msg.MessageID, &msg.Subject,
			&msg.From, &msg.To, &msg.Cc, &msg.Bcc, &msg.ReplyTo,
			&msg.Date, &msg.Body, &msg.BodyHTML, &msg.Attachments, &msg.Size,
			&msg.UID, &msg.Seen, &msg.Flagged, &msg.Answered, &msg.Draft, &msg.Deleted,
			&msg.InReplyTo, &msg.MessageReferences, &msg.SpamScore, &msg.SpamStatus, &msg.SpamReasons,
			&msg.RemoteUID, &msg.RemoteFolder, &msg.CreatedAt, &msg.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan message: %w", err)
		}
		messages = append(messages, msg)
	}

	return messages, nil
}

// GetMaxUIDForFolder returns the maximum UID for messages in a folder
// Returns 0 if no messages exist in the folder
func (db *DB) GetMaxUIDForFolder(folderID int64) (uint32, error) {
	var maxUID sql.NullInt64
	query := `SELECT MAX(uid) FROM messages WHERE folder_id = $1`

	err := db.QueryRow(query, folderID).Scan(&maxUID)
	if err != nil {
		return 0, fmt.Errorf("failed to get max UID: %w", err)
	}

	if !maxUID.Valid {
		return 0, nil
	}

	return uint32(maxUID.Int64), nil
}

// DeleteMessagesByFolder deletes all messages in a folder
// Used when UIDVALIDITY changes (folder was recreated on server)
func (db *DB) DeleteMessagesByFolder(folderID int64) (int64, error) {
	query := `DELETE FROM messages WHERE folder_id = $1`

	result, err := db.Exec(query, folderID)
	if err != nil {
		return 0, fmt.Errorf("failed to delete messages: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get affected rows: %w", err)
	}

	return count, nil
}

// GetMessageCountByFolder returns the count of non-deleted messages in a folder
func (db *DB) GetMessageCountByFolder(folderID int64) (uint32, error) {
	var count int64
	query := `SELECT COUNT(*) FROM messages WHERE folder_id = $1 AND deleted = false AND (soft_deleted = false OR soft_deleted IS NULL)`

	err := db.QueryRow(query, folderID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count messages: %w", err)
	}

	return uint32(count), nil
}

// MessageExistsByMessageID checks if a message with given message_id exists for user
func (db *DB) MessageExistsByMessageID(userID int64, messageID string) (bool, error) {
	if messageID == "" {
		return false, nil
	}

	var exists bool
	query := `SELECT EXISTS(SELECT 1 FROM messages WHERE user_id = $1 AND message_id = $2)`

	err := db.QueryRow(query, userID, messageID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check message existence: %w", err)
	}

	return exists, nil
}

// UpdateMessageRemoteUID updates the remote_uid and remote_folder for a message that doesn't have them set
// Returns true if the message was updated, false if it already had remote_uid or doesn't exist
func (db *DB) UpdateMessageRemoteUID(userID int64, messageID string, remoteUID uint32, remoteFolder string) (bool, error) {
	if messageID == "" || remoteUID == 0 {
		return false, nil
	}

	query := `
		UPDATE messages
		SET remote_uid = $1, remote_folder = $2, updated_at = $3
		WHERE user_id = $4 AND message_id = $5 AND (remote_uid IS NULL OR remote_uid = 0)
	`

	result, err := db.Exec(query, remoteUID, remoteFolder, time.Now(), userID, messageID)
	if err != nil {
		return false, fmt.Errorf("failed to update message remote_uid: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to get rows affected: %w", err)
	}

	return rows > 0, nil
}

// GetNextUIDForFolder returns the next UID for a folder and increments it atomically
func (db *DB) GetNextUIDForFolder(folderID int64) (uint32, error) {
	var uid uint32
	query := `UPDATE folders SET uid_next = uid_next + 1 WHERE id = $1 RETURNING uid_next - 1`

	err := db.QueryRow(query, folderID).Scan(&uid)
	if err != nil {
		return 0, fmt.Errorf("failed to get next UID: %w", err)
	}

	return uid, nil
}

// SoftDeleteMessage marks a message as soft deleted (moves to vault)
func (db *DB) SoftDeleteMessage(id int64) error {
	query := `
		UPDATE messages
		SET soft_deleted = true, soft_deleted_at = $1, original_folder_id = folder_id, updated_at = $1
		WHERE id = $2
	`

	_, err := db.Exec(query, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to soft delete message: %w", err)
	}

	return nil
}

// SoftDeleteMessagesByUIDs marks messages as soft deleted by UIDs in a folder
func (db *DB) SoftDeleteMessagesByUIDs(folderID int64, uids []uint32) (int64, error) {
	if len(uids) == 0 {
		return 0, nil
	}

	query := `
		UPDATE messages
		SET soft_deleted = true, soft_deleted_at = $1, original_folder_id = folder_id, updated_at = $1
		WHERE folder_id = $2 AND uid = ANY($3) AND deleted = true
	`

	result, err := db.Exec(query, time.Now(), folderID, pq.Array(uids))
	if err != nil {
		return 0, fmt.Errorf("failed to soft delete messages: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get affected rows: %w", err)
	}

	return count, nil
}

// HardDeleteMessagesByUIDs permanently deletes messages by UIDs in a folder
func (db *DB) HardDeleteMessagesByUIDs(folderID int64, uids []uint32) (int64, error) {
	if len(uids) == 0 {
		return 0, nil
	}

	query := `DELETE FROM messages WHERE folder_id = $1 AND uid = ANY($2) AND deleted = true`

	result, err := db.Exec(query, folderID, pq.Array(uids))
	if err != nil {
		return 0, fmt.Errorf("failed to hard delete messages: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get affected rows: %w", err)
	}

	return count, nil
}

// RestoreFromVault restores a soft-deleted message to its original folder
func (db *DB) RestoreFromVault(id int64) error {
	query := `
		UPDATE messages
		SET soft_deleted = false, soft_deleted_at = NULL, folder_id = COALESCE(original_folder_id, folder_id),
		    original_folder_id = NULL, deleted = false, updated_at = $1
		WHERE id = $2 AND soft_deleted = true
	`

	result, err := db.Exec(query, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to restore message from vault: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get affected rows: %w", err)
	}

	if count == 0 {
		return fmt.Errorf("message not found in vault")
	}

	return nil
}

// GetVaultMessages retrieves soft-deleted messages for a user
func (db *DB) GetVaultMessages(userID int64, limit, offset int) ([]*models.Message, error) {
	query := `
		SELECT id, COALESCE(account_id, 0), user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, COALESCE(spam_score, 0), COALESCE(spam_status, 'clean'), COALESCE(spam_reasons, ''),
		       COALESCE(remote_uid, 0), COALESCE(remote_folder, 'INBOX'), created_at, updated_at
		FROM messages
		WHERE user_id = $1 AND soft_deleted = true
		ORDER BY soft_deleted_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := db.Query(query, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to get vault messages: %w", err)
	}
	defer rows.Close()

	return scanMessages(rows)
}

// PurgeVaultMessages permanently deletes messages that have been in vault longer than given duration
func (db *DB) PurgeVaultMessages(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	query := `DELETE FROM messages WHERE soft_deleted = true AND soft_deleted_at < $1`

	result, err := db.Exec(query, cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to purge vault messages: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get affected rows: %w", err)
	}

	return count, nil
}

// GetDeletedMessagesByFolder retrieves messages marked as deleted in a folder (for EXPUNGE)
// This includes messages with deleted=true flag, excluding soft_deleted
func (db *DB) GetDeletedMessagesByFolder(folderID int64) ([]*models.Message, error) {
	query := `
		SELECT id, COALESCE(account_id, 0), user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, COALESCE(spam_score, 0), COALESCE(spam_status, 'clean'), COALESCE(spam_reasons, ''),
		       COALESCE(remote_uid, 0), COALESCE(remote_folder, 'INBOX'), created_at, updated_at
		FROM messages
		WHERE folder_id = $1 AND deleted = true AND (soft_deleted = false OR soft_deleted IS NULL)
		ORDER BY uid ASC
	`

	rows, err := db.Query(query, folderID)
	if err != nil {
		return nil, fmt.Errorf("failed to get deleted messages: %w", err)
	}
	defer rows.Close()

	return scanMessages(rows)
}

// GetTotalMessageCount returns total count of all messages
func (db *DB) GetTotalMessageCount() (int, error) {
	var count int
	query := `SELECT COUNT(*) FROM messages`

	err := db.QueryRow(query).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count messages: %w", err)
	}

	return count, nil
}

// GetMessagesForIndexing retrieves messages for search indexing with pagination
func (db *DB) GetMessagesForIndexing(limit, offset int) ([]*models.Message, error) {
	query := `
		SELECT id, COALESCE(account_id, 0), user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, COALESCE(spam_score, 0), COALESCE(spam_status, 'clean'), COALESCE(spam_reasons, ''),
		       created_at, updated_at, COALESCE(soft_deleted, false)
		FROM messages
		ORDER BY id ASC
		LIMIT $1 OFFSET $2
	`

	rows, err := db.Query(query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to get messages for indexing: %w", err)
	}
	defer rows.Close()

	var messages []*models.Message
	for rows.Next() {
		msg := &models.Message{}
		err := rows.Scan(
			&msg.ID, &msg.AccountID, &msg.UserID, &msg.FolderID, &msg.MessageID, &msg.Subject,
			&msg.From, &msg.To, &msg.Cc, &msg.Bcc, &msg.ReplyTo,
			&msg.Date, &msg.Body, &msg.BodyHTML, &msg.Attachments, &msg.Size,
			&msg.UID, &msg.Seen, &msg.Flagged, &msg.Answered, &msg.Draft, &msg.Deleted,
			&msg.InReplyTo, &msg.MessageReferences, &msg.SpamScore, &msg.SpamStatus, &msg.SpamReasons,
			&msg.CreatedAt, &msg.UpdatedAt, &msg.SoftDeleted,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan message: %w", err)
		}
		messages = append(messages, msg)
	}

	return messages, nil
}

// GetMessagesByIDs retrieves messages by their IDs (preserving order)
func (db *DB) GetMessagesByIDs(ids []int64) ([]*models.Message, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	query := `
		SELECT id, COALESCE(account_id, 0), user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, COALESCE(spam_score, 0), COALESCE(spam_status, 'clean'), COALESCE(spam_reasons, ''),
		       COALESCE(remote_uid, 0), COALESCE(remote_folder, 'INBOX'), created_at, updated_at
		FROM messages
		WHERE id = ANY($1) AND deleted = false AND (soft_deleted = false OR soft_deleted IS NULL)
		ORDER BY date DESC
	`

	rows, err := db.Query(query, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("failed to get messages by IDs: %w", err)
	}
	defer rows.Close()

	return scanMessages(rows)
}

// GetSoftDeletedMessages retrieves soft-deleted messages for a user (vault)
func (db *DB) GetSoftDeletedMessages(userID int64, limit, offset int) ([]*models.Message, error) {
	query := `
		SELECT id, COALESCE(account_id, 0), user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, COALESCE(spam_score, 0), COALESCE(spam_status, 'clean'), COALESCE(spam_reasons, ''),
		       COALESCE(remote_uid, 0), COALESCE(remote_folder, 'INBOX'), created_at, updated_at
		FROM messages
		WHERE user_id = $1 AND soft_deleted = true
		ORDER BY soft_deleted_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := db.Query(query, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to get soft-deleted messages: %w", err)
	}
	defer rows.Close()

	return scanMessages(rows)
}

// HardDeleteMessage permanently deletes a message by ID
func (db *DB) HardDeleteMessage(id int64) error {
	query := `DELETE FROM messages WHERE id = $1`

	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to hard delete message: %w", err)
	}

	return nil
}

// RestoreSoftDeletedMessage restores a soft-deleted message to its original folder
func (db *DB) RestoreSoftDeletedMessage(id int64) error {
	return db.RestoreFromVault(id)
}

// CopyMessageToFolder copies a message to another folder with a new UID
func (db *DB) CopyMessageToFolder(msgID, destFolderID int64) (uint32, error) {
	// Verify source message exists
	_, err := db.GetMessageByID(msgID)
	if err != nil {
		return 0, fmt.Errorf("failed to get source message: %w", err)
	}

	// Get destination folder for UID assignment
	destFolder, err := db.GetFolderByID(destFolderID)
	if err != nil {
		return 0, fmt.Errorf("failed to get destination folder: %w", err)
	}

	// Assign new UID
	newUID := destFolder.UIDNext
	destFolder.UIDNext++

	// Update folder's UIDNext
	if err := db.UpdateFolderUIDInfo(destFolderID, destFolder.UIDNext, destFolder.UIDValidity); err != nil {
		return 0, fmt.Errorf("failed to update folder UID: %w", err)
	}

	// Create copy in destination folder
	query := `
		INSERT INTO messages (
			account_id, user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
			date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
			in_reply_to, message_references, created_at, updated_at
		)
		SELECT
			account_id, user_id, $1, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
			date, body, body_html, attachments, size, $2, seen, flagged, answered, draft, false,
			in_reply_to, message_references, $3, $3
		FROM messages WHERE id = $4
		RETURNING id
	`

	var newMsgID int64
	err = db.QueryRow(query, destFolderID, newUID, time.Now(), msgID).Scan(&newMsgID)
	if err != nil {
		return 0, fmt.Errorf("failed to copy message: %w", err)
	}

	return newUID, nil
}
