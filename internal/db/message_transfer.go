package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// Row-level building blocks of IMAP COPY/MOVE. The rules — when a copy is a
// move, what moving into Trash means upstream — live in
// internal/service/messages.Transfer, which composes these in one transaction.

// ClaimFolderUID hands out the next UID of the user's folder (atomically: the
// folders row stays locked until the transaction ends, so concurrent COPY,
// MOVE and APPEND into it never get the same UID — RFC 3501 §2.3.1.1) and
// reports the folder's type. ErrNotFound when the folder is not the user's.
func (tx *Tx) ClaimFolderUID(userID, folderID int64) (uint32, string, error) {
	var uid int64
	var folderType string
	err := tx.QueryRow(`UPDATE folders SET uid_next = uid_next + 1
		WHERE id = $1 AND user_id = $2
		RETURNING uid_next - 1, type`, folderID, userID).Scan(&uid, &folderType)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	if err != nil {
		return 0, "", fmt.Errorf("claim UID in folder %d: %w", folderID, err)
	}
	return uint32(uid), folderType, nil
}

// MoveMessageRow puts the message into another folder under a new UID. The
// row — and with it the Message-ID identity, attachments and the source-server
// pointer — stays the same. \Deleted is cleared: a deleted-flagged row is
// invisible in every folder view, so carrying it along would lose the message.
func (tx *Tx) MoveMessageRow(messageID, folderID int64, uid uint32) error {
	res, err := tx.Exec(`UPDATE messages
		SET folder_id = $1, uid = $2, deleted = false, updated_at = $3
		WHERE id = $4`, folderID, uid, timeutil.Now(), messageID)
	if err != nil {
		return fmt.Errorf("move message %d to folder %d: %w", messageID, folderID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("move message %d: %w", messageID, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CopyMessageRow inserts a copy of the message (with its attachments) into
// another folder under the given UID and returns the new row's id. Only valid
// for a message without a Message-ID: one with an ID is unique per user
// (migrations 041/042) and cannot exist twice.
func (tx *Tx) CopyMessageRow(messageID, folderID int64, uid uint32) (int64, error) {
	now := timeutil.Now()
	var newID int64
	err := tx.QueryRow(`
		INSERT INTO messages (
			account_id, user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
			date, date_tz, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
			in_reply_to, message_references, raw_email, created_at, updated_at
		)
		SELECT
			account_id, user_id, $1, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
			date, date_tz, body, body_html, attachments, size, $2, seen, flagged, answered, draft, false,
			in_reply_to, message_references, raw_email, $3, $3
		FROM messages WHERE id = $4
		RETURNING id`, folderID, uid, now, messageID).Scan(&newID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("copy message %d to folder %d: %w", messageID, folderID, err)
	}
	if _, err := tx.Exec(`
		INSERT INTO attachments (message_id, filename, content_type, size, data, content_id, is_inline, created_at)
		SELECT $1, filename, content_type, size, data, content_id, is_inline, $2
		FROM attachments WHERE message_id = $3`, newID, now, messageID); err != nil {
		return 0, fmt.Errorf("copy attachments of message %d: %w", messageID, err)
	}
	return newID, nil
}
