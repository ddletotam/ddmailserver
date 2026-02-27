package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// CreateMessage creates a new message
func (db *DB) CreateMessage(msg *models.Message) error {
	msg.CreatedAt = time.Now()
	msg.UpdatedAt = time.Now()

	query := `
		INSERT INTO messages (
			account_id, user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
			date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
			in_reply_to, message_references, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25)
		RETURNING id
	`

	err := db.QueryRow(
		query,
		msg.AccountID, msg.UserID, msg.FolderID, msg.MessageID, msg.Subject,
		msg.From, msg.To, msg.Cc, msg.Bcc, msg.ReplyTo,
		msg.Date, msg.Body, msg.BodyHTML, msg.Attachments, msg.Size,
		msg.UID, msg.Seen, msg.Flagged, msg.Answered, msg.Draft, msg.Deleted,
		msg.InReplyTo, msg.MessageReferences, msg.CreatedAt, msg.UpdatedAt,
	).Scan(&msg.ID)

	if err != nil {
		return fmt.Errorf("failed to create message: %w", err)
	}

	return nil
}

// GetMessagesByFolder retrieves messages in a folder
func (db *DB) GetMessagesByFolder(folderID int64, limit, offset int) ([]*models.Message, error) {
	query := `
		SELECT id, account_id, user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, created_at, updated_at
		FROM messages
		WHERE folder_id = $1 AND deleted = false
		ORDER BY date DESC
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
		SELECT id, account_id, user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, created_at, updated_at
		FROM messages
		WHERE user_id = $1 AND deleted = false
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
		SELECT id, account_id, user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, created_at, updated_at
		FROM messages
		WHERE id = $1
	`

	err := db.QueryRow(query, id).Scan(
		&msg.ID, &msg.AccountID, &msg.UserID, &msg.FolderID, &msg.MessageID, &msg.Subject,
		&msg.From, &msg.To, &msg.Cc, &msg.Bcc, &msg.ReplyTo,
		&msg.Date, &msg.Body, &msg.BodyHTML, &msg.Attachments, &msg.Size,
		&msg.UID, &msg.Seen, &msg.Flagged, &msg.Answered, &msg.Draft, &msg.Deleted,
		&msg.InReplyTo, &msg.References, &msg.CreatedAt, &msg.UpdatedAt,
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
		SELECT id, account_id, user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, created_at, updated_at
		FROM messages
		WHERE user_id = $1 AND deleted = false
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
			&msg.InReplyTo, &msg.References, &msg.CreatedAt, &msg.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan message: %w", err)
		}
		messages = append(messages, msg)
	}

	return messages, nil
}
