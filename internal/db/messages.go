package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// CreateMessage creates a new message
func (db *DB) CreateMessage(msg *models.Message) error {
	msg.CreatedAt = timeutil.Now()
	msg.UpdatedAt = timeutil.Now()

	// Default spam status if not set
	if msg.SpamStatus == "" {
		msg.SpamStatus = "clean"
	}

	query := `
		INSERT INTO messages (
			account_id, user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
			date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
			in_reply_to, message_references, spam_score, spam_status, spam_reasons,
			is_spam, spam_rule_id, remote_uid, remote_folder, raw_email, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33)
		ON CONFLICT (user_id, message_id) WHERE COALESCE(message_id, '') <> '' DO NOTHING
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

	// Use NULL for spam_rule_id if not set
	var spamRuleID sql.NullInt64
	if msg.SpamRuleID != nil {
		spamRuleID.Int64 = *msg.SpamRuleID
		spamRuleID.Valid = true
	}

	// Default remote_folder to INBOX if not set
	remoteFolder := msg.RemoteFolder
	if remoteFolder == "" {
		remoteFolder = "INBOX"
	}

	// raw_email may be nil for legacy callers; PostgreSQL stores nil []byte as NULL.
	err := db.QueryRow(
		query,
		accountID, msg.UserID, msg.FolderID, msg.MessageID, msg.Subject,
		msg.From, msg.To, msg.Cc, msg.Bcc, msg.ReplyTo,
		msg.Date, msg.Body, msg.BodyHTML, msg.Attachments, msg.Size,
		msg.UID, msg.Seen, msg.Flagged, msg.Answered, msg.Draft, msg.Deleted,
		msg.InReplyTo, msg.MessageReferences, msg.SpamScore, msg.SpamStatus, msg.SpamReasons,
		msg.IsSpam, spamRuleID, remoteUID, remoteFolder, msg.RawEmail, msg.CreatedAt, msg.UpdatedAt,
	).Scan(&msg.ID)

	// ON CONFLICT DO NOTHING returns no row when (user_id, message_id) already
	// exists — a duplicate (e.g. a race between concurrent syncs). The DB
	// constraint is the dedup guarantee; callers treat this as "skip".
	if err == sql.ErrNoRows {
		return ErrDuplicateMessage
	}
	if err != nil {
		return fmt.Errorf("failed to create message: %w", err)
	}

	return nil
}

// GetMessagesByFolder retrieves messages in a folder (excludes soft deleted and spam)
// IMPORTANT: Order by UID ASC for correct IMAP sequence number mapping
func (db *DB) GetMessagesByFolder(folderID int64, limit, offset int) ([]*models.Message, error) {
	query := `
		SELECT id, COALESCE(account_id, 0), user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, COALESCE(spam_score, 0), COALESCE(spam_status, 'clean'), COALESCE(spam_reasons, ''),
		       COALESCE(remote_uid, 0), COALESCE(remote_folder, 'INBOX'), created_at, updated_at
		FROM messages
		WHERE folder_id = $1 AND deleted = false AND (soft_deleted = false OR soft_deleted IS NULL)
		      AND (is_spam = false OR is_spam IS NULL)
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

// GetMessagesByFolderMeta is like GetMessagesByFolder but DOES NOT load the
// heavy body/body_html columns (they come back as empty strings). Use it for
// anything that only needs metadata — sequence-number mapping, flag updates,
// copy/move, and FETCHes that don't request a body. Loading full bodies for
// the whole folder on every FETCH is O(folder) per call and turns a per-message
// sync into O(N²); this keeps those paths cheap.
func (db *DB) GetMessagesByFolderMeta(folderID int64, limit, offset int) ([]*models.Message, error) {
	query := `
		SELECT id, COALESCE(account_id, 0), user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, '' AS body, '' AS body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, COALESCE(spam_score, 0), COALESCE(spam_status, 'clean'), COALESCE(spam_reasons, ''),
		       COALESCE(remote_uid, 0), COALESCE(remote_folder, 'INBOX'), created_at, updated_at
		FROM messages
		WHERE folder_id = $1 AND deleted = false AND (soft_deleted = false OR soft_deleted IS NULL)
		      AND (is_spam = false OR is_spam IS NULL)
		ORDER BY uid ASC
		LIMIT $2 OFFSET $3
	`

	rows, err := db.Query(query, folderID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to get folder message metadata: %w", err)
	}
	defer rows.Close()

	return scanMessages(rows)
}

// GetFolderStatusCounts returns the total, unseen and recent message counts for
// a folder in a single COUNT query — used by IMAP STATUS so we don't load every
// message body just to count. recentSinceMs is the cutoff for the RECENT count
// (messages created after it).
func (db *DB) GetFolderStatusCounts(folderID int64, recentSinceMs int64) (total, unseen, recent uint32, err error) {
	query := `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE seen = false),
		       COUNT(*) FILTER (WHERE created_at > $2)
		FROM messages
		WHERE folder_id = $1 AND deleted = false AND (soft_deleted = false OR soft_deleted IS NULL)
		      AND (is_spam = false OR is_spam IS NULL)
	`
	var t, u, r int64
	if err = db.QueryRow(query, folderID, recentSinceMs).Scan(&t, &u, &r); err != nil {
		return 0, 0, 0, fmt.Errorf("failed to get folder status counts: %w", err)
	}
	return uint32(t), uint32(u), uint32(r), nil
}

// GetMessagesByUser retrieves all messages for a user (excludes spam)
func (db *DB) GetMessagesByUser(userID int64, limit, offset int) ([]*models.Message, error) {
	query := `
		SELECT id, COALESCE(account_id, 0), user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, COALESCE(spam_score, 0), COALESCE(spam_status, 'clean'), COALESCE(spam_reasons, ''),
		       COALESCE(remote_uid, 0), COALESCE(remote_folder, 'INBOX'), created_at, updated_at
		FROM messages
		WHERE user_id = $1 AND deleted = false AND (soft_deleted = false OR soft_deleted IS NULL)
		      AND (is_spam = false OR is_spam IS NULL)
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

// ErrNotFound is returned when a record is not found OR when the requested
// record is not accessible to the user (we don't distinguish to avoid leaking
// existence information).
var ErrNotFound = errors.New("not found")

// ErrDuplicateMessage is returned by CreateMessage when a row with the same
// (user_id, message_id) already exists. Callers treat it as a benign skip.
var ErrDuplicateMessage = errors.New("duplicate message (user_id, message_id)")

// GetMessageByIDForUser retrieves a message by ID, but only if it belongs to
// the given user. Returns ErrNotFound for both "no such message" and "exists
// but belongs to someone else" — handlers should treat both identically.
func (db *DB) GetMessageByIDForUser(id, userID int64) (*models.Message, error) {
	msg, err := db.GetMessageByID(id)
	if err != nil {
		return nil, err
	}
	if msg == nil || msg.UserID != userID {
		return nil, ErrNotFound
	}
	return msg, nil
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

	_, err := db.Exec(query, seen, flagged, answered, deleted, timeutil.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to update message flags: %w", err)
	}

	return nil
}

// UpdateMessageFlag sets a single flag on a message.
func (db *DB) UpdateMessageFlag(id int64, flag string, value bool) error {
	query := fmt.Sprintf(`UPDATE messages SET %s = $1, updated_at = $2 WHERE id = $3`, flag)
	_, err := db.Exec(query, value, timeutil.Now(), id)
	return err
}

// UpdateMessage updates a message
func (db *DB) UpdateMessage(msg *models.Message) error {
	msg.UpdatedAt = timeutil.Now()

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

// UpdateMessageAttachmentCount updates the attachment count for a message
func (db *DB) UpdateMessageAttachmentCount(id int64, count int) error {
	query := `UPDATE messages SET attachments = $1 WHERE id = $2`
	_, err := db.Exec(query, count, id)
	if err != nil {
		return fmt.Errorf("failed to update attachment count: %w", err)
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
		AND (is_spam = false OR is_spam IS NULL)
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

// GetMessageCountByFolder returns the count of non-deleted, non-spam messages in a folder
func (db *DB) GetMessageCountByFolder(folderID int64) (uint32, error) {
	var count int64
	query := `SELECT COUNT(*) FROM messages WHERE folder_id = $1 AND deleted = false AND (soft_deleted = false OR soft_deleted IS NULL) AND (is_spam = false OR is_spam IS NULL)`

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

// GetMessageByMessageID retrieves a message by RFC 5322 Message-ID header for a user
func (db *DB) GetMessageByMessageID(userID int64, messageID string) (*models.Message, error) {
	if messageID == "" {
		return nil, fmt.Errorf("message_id is empty")
	}

	msg := &models.Message{}
	query := `
		SELECT id, COALESCE(account_id, 0), user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, COALESCE(spam_score, 0), COALESCE(spam_status, 'clean'), COALESCE(spam_reasons, ''),
		       COALESCE(remote_uid, 0), COALESCE(remote_folder, 'INBOX'), created_at, updated_at
		FROM messages
		WHERE user_id = $1 AND message_id = $2
		LIMIT 1
	`

	err := db.QueryRow(query, userID, messageID).Scan(
		&msg.ID, &msg.AccountID, &msg.UserID, &msg.FolderID, &msg.MessageID, &msg.Subject,
		&msg.From, &msg.To, &msg.Cc, &msg.Bcc, &msg.ReplyTo,
		&msg.Date, &msg.Body, &msg.BodyHTML, &msg.Attachments, &msg.Size,
		&msg.UID, &msg.Seen, &msg.Flagged, &msg.Answered, &msg.Draft, &msg.Deleted,
		&msg.InReplyTo, &msg.MessageReferences, &msg.SpamScore, &msg.SpamStatus, &msg.SpamReasons,
		&msg.RemoteUID, &msg.RemoteFolder, &msg.CreatedAt, &msg.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get message by message_id: %w", err)
	}

	return msg, nil
}

// ReclassifyMessageFromRemoteSpam re-tags an existing local message
// when the upstream provider has moved it to a spam/junk folder. We
// always refresh remote_uid + remote_folder (so future delete-sync
// targets the right path), and we flip is_spam to the supplied value —
// the caller has already consulted the user's whitelist to decide
// whether the rescue applies. spam_status / spam_reasons get a marker
// noting the upstream classification when isSpam is true.
//
// Scoped to accountID: the row's identity is (user_id, Message-ID), but the
// same email delivered to SEVERAL of the user's sources shares that one row —
// only the account that created the row may reclassify it (see the ownership
// note on RefreshExistingFromRemote).
func (db *DB) ReclassifyMessageFromRemoteSpam(userID, accountID int64, messageID string, remoteUID uint32, remoteFolder string, isSpam bool) error {
	if messageID == "" {
		return nil
	}
	now := timeutil.Now()
	if isSpam {
		reason := fmt.Sprintf(`["classified spam by upstream (%s)"]`, remoteFolder)
		_, err := db.Exec(
			`UPDATE messages SET
			   remote_uid = $1, remote_folder = $2,
			   is_spam = true, spam_status = 'spam', spam_reasons = $3,
			   updated_at = $4
			 WHERE user_id = $5 AND message_id = $6 AND account_id = $7 AND
			       (is_spam = false OR is_spam IS NULL)`,
			remoteUID, remoteFolder, reason, now, userID, messageID, accountID,
		)
		if err != nil {
			return fmt.Errorf("reclassify as spam: %w", err)
		}
		return nil
	}
	// Rescue path: whitelist rule pulled the message back. Make sure the
	// row reflects "not spam" and the remote_folder pointer is still up
	// to date in case the user's other client moves it back.
	_, err := db.Exec(
		`UPDATE messages SET
		   remote_uid = $1, remote_folder = $2,
		   is_spam = false, updated_at = $3
		 WHERE user_id = $4 AND message_id = $5 AND account_id = $6`,
		remoteUID, remoteFolder, now, userID, messageID, accountID,
	)
	if err != nil {
		return fmt.Errorf("rescue from spam: %w", err)
	}
	return nil
}

// RefreshExistingFromRemote brings an already-existing local message
// in line with what the upstream IMAP server currently has:
//
//   - remote_uid + remote_folder always get refreshed so flag-sync /
//     delete-sync continue to target the right path.
//   - Flags (\Seen / \Flagged / \Answered) are overwritten from the
//     remote side — UNLESS the message still has an unpushed row in
//     flag_sync_queue. Upstream is authoritative only for state we have
//     already told it about: while our own change sits in the queue,
//     upstream's answer is simply the stale "before" picture, and
//     copying it back destroys the user's action. That was the
//     «прочитал письмо, флажок пропал и через минуту вернулся» bug —
//     the pull runs every cycle, the push waits for a worker slot, and
//     whoever ran last won. Local pending change wins until the push
//     succeeds (the worker deletes the queue row) or the entry is
//     dropped; only then does upstream get a say again.
//   - If `downgradeSpam` is true and the row was marked is_spam=true
//     by our analyzer (not by an explicit user spam-rule), clear it
//     and drop the spam metadata. Used by the inbox-pull path: a
//     message landing in the upstream INBOX is the strongest possible
//     "not spam" signal, overriding our prior verdict.
//
// ВЛАДЕЛЕЦ: обновление скоуплено `account_id = accountID`. Идентичность
// строки — (user_id, Message-ID), поэтому письмо, доставленное сразу в
// несколько источников пользователя (рассылка на два ящика, форвардинг),
// живёт ОДНОЙ строкой. Без скоупа каждый аккаунт-совладелец по очереди
// утверждал флаги СВОЕЙ копии на общей строке: прочитанное локально письмо
// каждые полминуты сбрасывалось обратно в unseen тем аккаунтом, чья копия
// не прочитана, а remote_uid переписывался на чужую копию — и очередь
// flag_sync пушила флаги на uid ДРУГОГО сервера. Теперь строку зеркалит
// только создавший её аккаунт; копии в остальных источниках дрейфуют
// независимо (это осознанный компромисс агрегации).
func (db *DB) RefreshExistingFromRemote(
	userID, accountID int64,
	messageID string,
	remoteUID uint32,
	remoteFolder string,
	seen, flagged, answered bool,
	downgradeSpam bool,
) (bool, error) {
	if messageID == "" {
		return false, nil
	}
	now := timeutil.Now()
	// No-op guard: this runs for EVERY existing message on EVERY sync
	// cycle. Writing unconditionally rewrote the whole mailbox each cycle
	// and bumped updated_at on unchanged rows — which both churns the DB
	// and poisons updated_at as a change signal (the desktop delta sync
	// saw every conversation as "changed" every cycle). Only write when
	// something actually differs. The returned bool is that same signal
	// (RowsAffected > 0) — the sync task uses it to publish flags_changed
	// pushes only when a remote-side change actually landed.
	//
	// `pending` (an unpushed flag_sync_queue row for this message) freezes the
	// three flag columns: they keep their local value, and they're excluded
	// from the no-op guard too — otherwise a locally-read message whose remote
	// still says unseen would match the guard on every single cycle, rewrite
	// itself to the same values, bump updated_at and fire a bogus
	// flags_changed push. remote_uid / remote_folder / spam columns keep
	// refreshing regardless: the queue only claims ownership of flags.
	if downgradeSpam {
		res, err := db.Exec(
			`WITH target AS (
			     SELECT m.id,
			            EXISTS (SELECT 1 FROM flag_sync_queue q WHERE q.message_id = m.id) AS pending
			       FROM messages m
			      WHERE m.user_id = $7 AND m.message_id = $8 AND m.account_id = $9
			 )
			 UPDATE messages m SET
			   remote_uid = $1, remote_folder = $2,
			   seen     = CASE WHEN t.pending THEN m.seen     ELSE $3 END,
			   flagged  = CASE WHEN t.pending THEN m.flagged  ELSE $4 END,
			   answered = CASE WHEN t.pending THEN m.answered ELSE $5 END,
			   is_spam = false, spam_status = 'clean',
			   spam_reasons = '', spam_rule_id = NULL,
			   updated_at = $6
			 FROM target t
			 WHERE m.id = t.id
			   AND (m.remote_uid IS DISTINCT FROM $1
			     OR m.remote_folder IS DISTINCT FROM $2
			     OR m.is_spam IS DISTINCT FROM false
			     OR (NOT t.pending
			         AND (m.seen IS DISTINCT FROM $3
			           OR m.flagged IS DISTINCT FROM $4
			           OR m.answered IS DISTINCT FROM $5)))`,
			remoteUID, remoteFolder, seen, flagged, answered, now, userID, messageID, accountID,
		)
		if err != nil {
			return false, fmt.Errorf("refresh + downgrade: %w", err)
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}
	res, err := db.Exec(
		`WITH target AS (
		     SELECT m.id,
		            EXISTS (SELECT 1 FROM flag_sync_queue q WHERE q.message_id = m.id) AS pending
		       FROM messages m
		      WHERE m.user_id = $7 AND m.message_id = $8 AND m.account_id = $9
		 )
		 UPDATE messages m SET
		   remote_uid = $1, remote_folder = $2,
		   seen     = CASE WHEN t.pending THEN m.seen     ELSE $3 END,
		   flagged  = CASE WHEN t.pending THEN m.flagged  ELSE $4 END,
		   answered = CASE WHEN t.pending THEN m.answered ELSE $5 END,
		   updated_at = $6
		 FROM target t
		 WHERE m.id = t.id
		   AND (m.remote_uid IS DISTINCT FROM $1
		     OR m.remote_folder IS DISTINCT FROM $2
		     OR (NOT t.pending
		         AND (m.seen IS DISTINCT FROM $3
		           OR m.flagged IS DISTINCT FROM $4
		           OR m.answered IS DISTINCT FROM $5)))`,
		remoteUID, remoteFolder, seen, flagged, answered, now, userID, messageID, accountID,
	)
	if err != nil {
		return false, fmt.Errorf("refresh existing: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// BackfillEncodedHeaders runs `decode` over every row whose Subject or
// address-header column still contains an RFC 2047 encoded-word
// (`=?charset?B?...?=`-style). Pulled out as a one-shot startup pass:
// when the decoder gets fixed or extended, this lets us catch up the
// already-persisted rows without truncating the table. Idempotent —
// rows whose decode produces the same string are left untouched, and
// strings that don't contain `=?` are skipped at query time.
//
// Caller passes the decoder so the db package doesn't have to import
// the parser package and risk an import cycle.
func (db *DB) BackfillEncodedHeaders(decode func(string) string) (int, error) {
	rows, err := db.Query(`
		SELECT id, subject, from_addr, to_addr, cc, bcc, reply_to
		FROM messages
		WHERE subject  LIKE '%=?%?=%'
		   OR from_addr LIKE '%=?%?=%'
		   OR to_addr   LIKE '%=?%?=%'
		   OR cc        LIKE '%=?%?=%'
		   OR bcc       LIKE '%=?%?=%'
		   OR reply_to  LIKE '%=?%?=%'
	`)
	if err != nil {
		return 0, fmt.Errorf("scan encoded rows: %w", err)
	}
	defer rows.Close()

	type rowInfo struct {
		id                                  int64
		subject, from, to, cc, bcc, replyTo string
	}
	var batch []rowInfo
	for rows.Next() {
		var r rowInfo
		if err := rows.Scan(&r.id, &r.subject, &r.from, &r.to, &r.cc, &r.bcc, &r.replyTo); err != nil {
			return 0, fmt.Errorf("scan row: %w", err)
		}
		batch = append(batch, r)
	}
	rows.Close()

	updated := 0
	for _, r := range batch {
		newSubject := decode(r.subject)
		newFrom := decode(r.from)
		newTo := decode(r.to)
		newCC := decode(r.cc)
		newBCC := decode(r.bcc)
		newReplyTo := decode(r.replyTo)
		if newSubject == r.subject && newFrom == r.from && newTo == r.to &&
			newCC == r.cc && newBCC == r.bcc && newReplyTo == r.replyTo {
			continue
		}
		if _, err := db.Exec(
			`UPDATE messages SET
			   subject = $1, from_addr = $2, to_addr = $3, cc = $4,
			   bcc = $5, reply_to = $6, updated_at = $7
			 WHERE id = $8`,
			newSubject, newFrom, newTo, newCC, newBCC, newReplyTo, timeutil.Now(), r.id,
		); err != nil {
			return updated, fmt.Errorf("update row %d: %w", r.id, err)
		}
		updated++
	}
	return updated, nil
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

	result, err := db.Exec(query, remoteUID, remoteFolder, timeutil.Now(), userID, messageID)
	if err != nil {
		return false, fmt.Errorf("failed to update message remote_uid: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to get rows affected: %w", err)
	}

	return rows > 0, nil
}

// UpdateMessageSize persists the RFC822.SIZE of a message. The size is the
// length of the RFC822 representation this server assembles and serves as
// BODY[] — not the original raw size on the remote — because clients (iOS
// Mail in particular) cross-check RFC822.SIZE against the literal they
// actually receive and discard bodies that don't match.
func (db *DB) UpdateMessageSize(messageID int64, size int64) error {
	_, err := db.Exec(`UPDATE messages SET size = $1, updated_at = $2 WHERE id = $3`,
		size, timeutil.Now(), messageID)
	if err != nil {
		return fmt.Errorf("failed to update message size: %w", err)
	}
	return nil
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
	now := timeutil.Now()
	query := `
		UPDATE messages
		SET soft_deleted = true, soft_deleted_at = $1, original_folder_id = folder_id, updated_at = $1
		WHERE id = $2
	`

	_, err := db.Exec(query, now, id)
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

	now := timeutil.Now()
	query := `
		UPDATE messages
		SET soft_deleted = true, soft_deleted_at = $1, original_folder_id = folder_id, updated_at = $1
		WHERE folder_id = $2 AND uid = ANY($3) AND deleted = true
	`

	result, err := db.Exec(query, now, folderID, pq.Array(uids))
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

	result, err := db.Exec(query, timeutil.Now(), id)
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
	cutoffMs := timeutil.Now() - olderThan.Milliseconds()
	query := `DELETE FROM messages WHERE soft_deleted = true AND soft_deleted_at < $1`

	result, err := db.Exec(query, cutoffMs)
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

// MessageExistsInFolder checks if a message with given Message-ID exists in a folder (for dedup)
func (db *DB) MessageExistsInFolder(folderID int64, messageID string) (bool, error) {
	if messageID == "" {
		return false, nil
	}
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE folder_id = $1 AND message_id = $2 AND (soft_deleted = false OR soft_deleted IS NULL)`,
		folderID, messageID,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// GetMessageUIDByMessageID returns the UID of a message in a folder by its Message-ID header.
func (db *DB) GetMessageUIDByMessageID(folderID int64, messageID string) (uint32, error) {
	var uid uint32
	err := db.QueryRow(
		`SELECT uid FROM messages WHERE folder_id = $1 AND message_id = $2 AND (soft_deleted = false OR soft_deleted IS NULL) ORDER BY uid LIMIT 1`,
		folderID, messageID,
	).Scan(&uid)
	return uid, err
}

// CopyMessageToFolder copies a message to another folder with a new UID.
// UID assignment and the row insert run in one transaction with an atomic
// uid_next claim — concurrent COPY/MOVE/APPEND into the same folder must
// never hand out the same UID (RFC 3501 §2.3.1.1).
func (db *DB) CopyMessageToFolder(msgID, destFolderID int64) (uint32, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Claim the next UID atomically (same pattern as GetNextUIDForFolder).
	var newUID uint32
	err = tx.QueryRow(
		`UPDATE folders SET uid_next = uid_next + 1 WHERE id = $1 RETURNING uid_next - 1`,
		destFolderID,
	).Scan(&newUID)
	if err != nil {
		return 0, fmt.Errorf("failed to claim UID in destination folder: %w", err)
	}

	// Create copy in destination folder. Carry raw_email along.
	now := timeutil.Now()
	query := `
		INSERT INTO messages (
			account_id, user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
			date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
			in_reply_to, message_references, raw_email, created_at, updated_at
		)
		SELECT
			account_id, user_id, $1, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
			date, body, body_html, attachments, size, $2, seen, flagged, answered, draft, false,
			in_reply_to, message_references, raw_email, $3, $3
		FROM messages WHERE id = $4
		RETURNING id
	`

	var newMsgID int64
	err = tx.QueryRow(query, destFolderID, newUID, now, msgID).Scan(&newMsgID)
	if err != nil {
		// sql.ErrNoRows here means the source message vanished — surface it
		// as a missing-source error rather than a generic copy failure.
		return 0, fmt.Errorf("failed to copy message %d: %w", msgID, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit message copy: %w", err)
	}
	return newUID, nil
}

// selectColumns is the standard column list for message queries.
// All columns are table-qualified so queries that JOIN folders (or any
// other table that happens to share a column name like `account_id`)
// don't trip on ambiguous references.
const selectColumns = `m.id, COALESCE(m.account_id, 0), m.user_id, m.folder_id, m.message_id, m.subject, m.from_addr, m.to_addr, m.cc, m.bcc, m.reply_to,
       m.date, m.body, m.body_html, m.attachments, m.size, m.uid, m.seen, m.flagged, m.answered, m.draft, m.deleted,
       m.in_reply_to, m.message_references, COALESCE(m.spam_score, 0), COALESCE(m.spam_status, 'clean'), COALESCE(m.spam_reasons, ''),
       COALESCE(m.remote_uid, 0), COALESCE(m.remote_folder, 'INBOX'), m.created_at, m.updated_at`

// notDeletedCondition filters out deleted, soft-deleted, and spam messages.
const notDeletedCondition = `deleted = false AND (soft_deleted = false OR soft_deleted IS NULL) AND (is_spam = false OR is_spam IS NULL)`

// GetUnreadCountByUser returns the number of unread messages for a user.
func (db *DB) GetUnreadCountByUser(userID int64) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id = $1 AND seen = false AND `+notDeletedCondition, userID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to get unread count: %w", err)
	}
	return count, nil
}

// GetMessageCountByUser returns the total number of messages for a user (excl. deleted/spam).
func (db *DB) GetMessageCountByUser(userID int64) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id = $1 AND `+notDeletedCondition, userID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to get message count: %w", err)
	}
	return count, nil
}

// GetMessagesByUserFiltered retrieves messages with optional folder type, account, and search
// filtering. Returns messages and total count for pagination.
func (db *DB) GetMessagesByUserFiltered(userID int64, folderType string, accountID int64, query string, limit, offset int) ([]*models.Message, int, error) {
	baseWhere := `m.user_id = $1 AND m.deleted = false AND (m.soft_deleted = false OR m.soft_deleted IS NULL) AND (m.is_spam = false OR m.is_spam IS NULL)`
	args := []interface{}{userID}
	argN := 2

	join := ""

	// Folder type filter
	if folderType != "" && folderType != "all" {
		if folderType == "drafts" {
			baseWhere += fmt.Sprintf(` AND m.draft = true`)
		} else if folderType == "trash" {
			// For trash, show soft-deleted instead
			baseWhere = fmt.Sprintf(`m.user_id = $1 AND m.soft_deleted = true AND (m.is_spam = false OR m.is_spam IS NULL)`)
		} else {
			join = ` JOIN folders f ON m.folder_id = f.id`
			baseWhere += fmt.Sprintf(` AND f.type = $%d`, argN)
			args = append(args, folderType)
			argN++
		}
	}

	// Account filter
	if accountID > 0 {
		baseWhere += fmt.Sprintf(` AND m.account_id = $%d`, argN)
		args = append(args, accountID)
		argN++
	}

	// Search filter
	if query != "" {
		pattern := "%" + query + "%"
		baseWhere += fmt.Sprintf(` AND (m.subject ILIKE $%d OR m.from_addr ILIKE $%d OR m.to_addr ILIKE $%d OR m.body ILIKE $%d)`, argN, argN, argN, argN)
		args = append(args, pattern)
		argN++
	}

	// Count total
	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM messages m%s WHERE %s`, join, baseWhere)
	var total int
	if err := db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("failed to count messages: %w", err)
	}

	// Fetch page
	orderBy := `m.date DESC`
	if folderType == "drafts" {
		orderBy = `m.updated_at DESC`
	}

	dataQuery := fmt.Sprintf(`SELECT %s FROM messages m%s WHERE %s ORDER BY %s LIMIT $%d OFFSET $%d`,
		selectColumns, join, baseWhere, orderBy, argN, argN+1)
	args = append(args, limit, offset)

	rows, err := db.Query(dataQuery, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get filtered messages: %w", err)
	}
	defer rows.Close()

	msgs, err := scanMessages(rows)
	if err != nil {
		return nil, 0, err
	}
	return msgs, total, nil
}

// GetMessageRawEmail returns the original RFC-822 bytes of a message, or nil if
// not stored (legacy rows pre-dating migration 032). Kept as a separate fetch
// so the standard SELECTs don't drag the BYTEA payload through every list call.
func (db *DB) GetMessageRawEmail(messageID int64) ([]byte, error) {
	var raw []byte
	err := db.QueryRow(
		`SELECT raw_email FROM messages WHERE id = $1`,
		messageID,
	).Scan(&raw)
	if err != nil {
		return nil, fmt.Errorf("get raw email: %w", err)
	}
	return raw, nil
}
