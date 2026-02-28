package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// CreateOutboxMessage creates a new outbox message
func (db *DB) CreateOutboxMessage(msg *models.OutboxMessage) error {
	msg.CreatedAt = time.Now()
	msg.UpdatedAt = time.Now()

	query := `
		INSERT INTO outbox_messages (
			user_id, account_id, from_addr, to_addr, cc, bcc, subject, body, body_html,
			raw_email, status, retries, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING id
	`

	err := db.QueryRow(
		query,
		msg.UserID, msg.AccountID, msg.From, msg.To, msg.Cc, msg.Bcc,
		msg.Subject, msg.Body, msg.BodyHTML, msg.RawEmail,
		msg.Status, msg.Retries, msg.CreatedAt, msg.UpdatedAt,
	).Scan(&msg.ID)

	if err != nil {
		return fmt.Errorf("failed to create outbox message: %w", err)
	}

	return nil
}

// GetPendingOutboxMessages retrieves all pending messages
func (db *DB) GetPendingOutboxMessages(limit int) ([]*models.OutboxMessage, error) {
	query := `
		SELECT id, user_id, account_id, from_addr, to_addr, cc, bcc, subject, body, body_html,
		       raw_email, status, retries, last_error, created_at, updated_at, sent_at
		FROM outbox_messages
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT $1
	`

	rows, err := db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get pending messages: %w", err)
	}
	defer rows.Close()

	return scanOutboxMessages(rows)
}

// GetOutboxMessageByID retrieves an outbox message by ID
func (db *DB) GetOutboxMessageByID(id int64) (*models.OutboxMessage, error) {
	msg := &models.OutboxMessage{}
	var sentAt sql.NullTime

	query := `
		SELECT id, user_id, account_id, from_addr, to_addr, cc, bcc, subject, body, body_html,
		       raw_email, status, retries, last_error, created_at, updated_at, sent_at
		FROM outbox_messages
		WHERE id = $1
	`

	err := db.QueryRow(query, id).Scan(
		&msg.ID, &msg.UserID, &msg.AccountID, &msg.From, &msg.To, &msg.Cc, &msg.Bcc,
		&msg.Subject, &msg.Body, &msg.BodyHTML, &msg.RawEmail,
		&msg.Status, &msg.Retries, &msg.LastError, &msg.CreatedAt, &msg.UpdatedAt, &sentAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("outbox message not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get outbox message: %w", err)
	}

	if sentAt.Valid {
		msg.SentAt = sentAt.Time
	}

	return msg, nil
}

// UpdateOutboxMessageStatus updates the status of an outbox message
func (db *DB) UpdateOutboxMessageStatus(id int64, status string, lastError string) error {
	now := time.Now()
	query := `
		UPDATE outbox_messages
		SET status = $1, last_error = $2, updated_at = $3
		WHERE id = $4
	`

	_, err := db.Exec(query, status, lastError, now, id)
	if err != nil {
		return fmt.Errorf("failed to update outbox message status: %w", err)
	}

	return nil
}

// MarkOutboxMessageSent marks a message as sent
func (db *DB) MarkOutboxMessageSent(id int64) error {
	now := time.Now()
	query := `
		UPDATE outbox_messages
		SET status = 'sent', sent_at = $1, updated_at = $2
		WHERE id = $3
	`

	_, err := db.Exec(query, now, now, id)
	if err != nil {
		return fmt.Errorf("failed to mark message as sent: %w", err)
	}

	return nil
}

// IncrementOutboxMessageRetries increments the retry counter
func (db *DB) IncrementOutboxMessageRetries(id int64, lastError string) error {
	now := time.Now()
	query := `
		UPDATE outbox_messages
		SET retries = retries + 1, last_error = $1, updated_at = $2
		WHERE id = $3
	`

	_, err := db.Exec(query, lastError, now, id)
	if err != nil {
		return fmt.Errorf("failed to increment retries: %w", err)
	}

	return nil
}

// DeleteOutboxMessage deletes an outbox message
func (db *DB) DeleteOutboxMessage(id int64) error {
	query := `DELETE FROM outbox_messages WHERE id = $1`
	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete outbox message: %w", err)
	}
	return nil
}

// Helper function to scan multiple outbox messages
func scanOutboxMessages(rows *sql.Rows) ([]*models.OutboxMessage, error) {
	var messages []*models.OutboxMessage

	for rows.Next() {
		msg := &models.OutboxMessage{}
		var sentAt sql.NullTime
		var lastError sql.NullString

		err := rows.Scan(
			&msg.ID, &msg.UserID, &msg.AccountID, &msg.From, &msg.To, &msg.Cc, &msg.Bcc,
			&msg.Subject, &msg.Body, &msg.BodyHTML, &msg.RawEmail,
			&msg.Status, &msg.Retries, &lastError, &msg.CreatedAt, &msg.UpdatedAt, &sentAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan outbox message: %w", err)
		}

		if sentAt.Valid {
			msg.SentAt = sentAt.Time
		}
		if lastError.Valid {
			msg.LastError = lastError.String
		}

		messages = append(messages, msg)
	}

	return messages, nil
}
