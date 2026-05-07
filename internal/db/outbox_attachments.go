package db

import (
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// CreateOutboxAttachment stores a file attachment for an outbox message
func (db *DB) CreateOutboxAttachment(att *models.OutboxAttachment) error {
	att.CreatedAt = time.Now()
	query := `INSERT INTO outbox_attachments (outbox_message_id, filename, content_type, size, data, content_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`
	var contentID *string
	if att.ContentID != "" {
		contentID = &att.ContentID
	}
	err := db.QueryRow(query, att.OutboxMessageID, att.Filename, att.ContentType, att.Size, att.Data, contentID, att.CreatedAt).Scan(&att.ID)
	if err != nil {
		return fmt.Errorf("failed to create outbox attachment: %w", err)
	}
	return nil
}

// GetOutboxAttachmentsByMessageID returns all attachments for an outbox message (with data)
func (db *DB) GetOutboxAttachmentsByMessageID(outboxMsgID int64) ([]*models.OutboxAttachment, error) {
	query := `SELECT id, outbox_message_id, filename, content_type, size, data, COALESCE(content_id, ''), created_at
		FROM outbox_attachments WHERE outbox_message_id = $1 ORDER BY id`
	rows, err := db.Query(query, outboxMsgID)
	if err != nil {
		return nil, fmt.Errorf("failed to get outbox attachments: %w", err)
	}
	defer rows.Close()

	var atts []*models.OutboxAttachment
	for rows.Next() {
		a := &models.OutboxAttachment{}
		if err := rows.Scan(&a.ID, &a.OutboxMessageID, &a.Filename, &a.ContentType, &a.Size, &a.Data, &a.ContentID, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan outbox attachment: %w", err)
		}
		atts = append(atts, a)
	}
	return atts, nil
}

// DeleteOutboxAttachmentsByMessageID removes all attachments for an outbox message
func (db *DB) DeleteOutboxAttachmentsByMessageID(outboxMsgID int64) error {
	_, err := db.Exec(`DELETE FROM outbox_attachments WHERE outbox_message_id = $1`, outboxMsgID)
	if err != nil {
		return fmt.Errorf("failed to delete outbox attachments: %w", err)
	}
	return nil
}
