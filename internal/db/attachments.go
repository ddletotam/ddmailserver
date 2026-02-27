package db

import (
	"database/sql"
	"fmt"

	"github.com/yourusername/mailserver/internal/models"
)

// CreateAttachment creates a new attachment
func (db *DB) CreateAttachment(att *models.Attachment) error {
	query := `
		INSERT INTO attachments (message_id, filename, content_type, size, data)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at
	`

	err := db.QueryRow(
		query,
		att.MessageID, att.Filename, att.ContentType, att.Size, att.Data,
	).Scan(&att.ID, &att.CreatedAt)

	if err != nil {
		return fmt.Errorf("failed to create attachment: %w", err)
	}

	return nil
}

// GetAttachmentsByMessageID retrieves all attachments for a message
func (db *DB) GetAttachmentsByMessageID(messageID int64) ([]*models.Attachment, error) {
	query := `
		SELECT id, message_id, filename, content_type, size, created_at
		FROM attachments
		WHERE message_id = $1
		ORDER BY filename
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
			&att.ID, &att.MessageID, &att.Filename, &att.ContentType,
			&att.Size, &att.CreatedAt,
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
		SELECT id, message_id, filename, content_type, size, data, created_at
		FROM attachments
		WHERE id = $1
	`

	att := &models.Attachment{}
	err := db.QueryRow(query, id).Scan(
		&att.ID, &att.MessageID, &att.Filename, &att.ContentType,
		&att.Size, &att.Data, &att.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("attachment not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get attachment: %w", err)
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
