package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// CreateAttachment creates a new attachment
func (db *DB) CreateAttachment(att *models.Attachment) error {
	att.CreatedAt = time.Now()

	query := `
		INSERT INTO attachments (message_id, content_id, filename, content_type, size, is_inline, content, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id
	`

	var contentID sql.NullString
	if att.ContentID != "" {
		contentID = sql.NullString{String: att.ContentID, Valid: true}
	}

	err := db.QueryRow(
		query,
		att.MessageID, contentID, att.Filename, att.ContentType,
		att.Size, att.IsInline, att.Data, att.CreatedAt,
	).Scan(&att.ID)

	if err != nil {
		return fmt.Errorf("failed to create attachment: %w", err)
	}

	return nil
}

// GetAttachmentsByMessageID retrieves all attachments for a message (without content)
func (db *DB) GetAttachmentsByMessageID(messageID int64) ([]*models.Attachment, error) {
	query := `
		SELECT id, message_id, COALESCE(content_id, ''), COALESCE(filename, ''),
		       content_type, size, is_inline, created_at
		FROM attachments
		WHERE message_id = $1
		ORDER BY id
	`

	rows, err := db.Query(query, messageID)
	if err != nil {
		return nil, fmt.Errorf("failed to get attachments: %w", err)
	}
	defer rows.Close()

	var attachments []*models.Attachment
	for rows.Next() {
		att := &models.Attachment{}
		err := rows.Scan(
			&att.ID, &att.MessageID, &att.ContentID, &att.Filename,
			&att.ContentType, &att.Size, &att.IsInline, &att.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan attachment: %w", err)
		}
		attachments = append(attachments, att)
	}

	return attachments, nil
}

// GetAttachmentByID retrieves a single attachment with data
func (db *DB) GetAttachmentByID(id int64) (*models.Attachment, error) {
	query := `
		SELECT id, message_id, COALESCE(content_id, ''), COALESCE(filename, ''),
		       content_type, size, is_inline, content, created_at
		FROM attachments
		WHERE id = $1
	`

	att := &models.Attachment{}
	err := db.QueryRow(query, id).Scan(
		&att.ID, &att.MessageID, &att.ContentID, &att.Filename,
		&att.ContentType, &att.Size, &att.IsInline, &att.Data, &att.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("attachment not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get attachment: %w", err)
	}

	return att, nil
}

// GetAttachmentByContentID retrieves an attachment by Content-ID for inline images
func (db *DB) GetAttachmentByContentID(messageID int64, contentID string) (*models.Attachment, error) {
	query := `
		SELECT id, message_id, COALESCE(content_id, ''), COALESCE(filename, ''),
		       content_type, size, is_inline, content, created_at
		FROM attachments
		WHERE message_id = $1 AND content_id = $2
	`

	att := &models.Attachment{}
	err := db.QueryRow(query, messageID, contentID).Scan(
		&att.ID, &att.MessageID, &att.ContentID, &att.Filename,
		&att.ContentType, &att.Size, &att.IsInline, &att.Data, &att.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil // Not found
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get attachment by content-id: %w", err)
	}

	return att, nil
}

// DeleteAttachment deletes an attachment
func (db *DB) DeleteAttachment(id int64) error {
	query := `DELETE FROM attachments WHERE id = $1`

	result, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete attachment: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("attachment not found")
	}

	return nil
}

// DeleteAttachmentsByMessageID deletes all attachments for a message
func (db *DB) DeleteAttachmentsByMessageID(messageID int64) error {
	query := `DELETE FROM attachments WHERE message_id = $1`
	_, err := db.Exec(query, messageID)
	if err != nil {
		return fmt.Errorf("failed to delete attachments: %w", err)
	}
	return nil
}
