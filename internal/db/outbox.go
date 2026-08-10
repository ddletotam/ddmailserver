package db

import (
	"database/sql"
	"fmt"

	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// CreateOutboxMessage creates a new outbox message
func (db *DB) CreateOutboxMessage(msg *models.OutboxMessage) error {
	msg.CreatedAt = timeutil.Now()
	msg.UpdatedAt = timeutil.Now()

	query := `
		INSERT INTO outbox_messages (
			user_id, account_id, from_addr, to_addr, cc, bcc, subject, body, body_html,
			raw_email, status, retries, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING id
	`

	// Use NULL for accountID=0 (direct delivery from local domain)
	var accountID interface{} = msg.AccountID
	if msg.AccountID == 0 {
		accountID = nil
	}

	err := db.QueryRow(
		query,
		msg.UserID, accountID, msg.From, msg.To, msg.Cc, msg.Bcc,
		msg.Subject, msg.Body, msg.BodyHTML, msg.RawEmail,
		msg.Status, msg.Retries, msg.CreatedAt, msg.UpdatedAt,
	).Scan(&msg.ID)

	if err != nil {
		return fmt.Errorf("failed to create outbox message: %w", err)
	}

	return nil
}

// GetPendingOutboxMessages retrieves pending messages that are due: after a
// failed attempt next_attempt_at holds the earliest time of the next try
// (backoff schedule in IncrementOutboxMessageRetries), so retries don't
// hammer a temporarily-refusing MX every scheduler cycle.
func (db *DB) GetPendingOutboxMessages(limit int) ([]*models.OutboxMessage, error) {
	query := `
		SELECT id, user_id, COALESCE(account_id, 0), from_addr, to_addr, cc, bcc, subject, body, body_html,
		       raw_email, status, retries, last_error, created_at, updated_at, sent_at
		FROM outbox_messages
		WHERE status = 'pending' AND COALESCE(next_attempt_at, 0) <= $2
		ORDER BY created_at ASC
		LIMIT $1
	`

	rows, err := db.Query(query, limit, timeutil.Now())
	if err != nil {
		return nil, fmt.Errorf("failed to get pending messages: %w", err)
	}
	defer rows.Close()

	return scanOutboxMessages(rows)
}

// GetOutboxMessageByID retrieves an outbox message by ID
func (db *DB) GetOutboxMessageByID(id int64) (*models.OutboxMessage, error) {
	msg := &models.OutboxMessage{}
	var sentAt sql.NullInt64

	query := `
		SELECT id, user_id, COALESCE(account_id, 0), from_addr, to_addr, cc, bcc, subject, body, body_html,
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
		msg.SentAt = sentAt.Int64
	}

	return msg, nil
}

// UpdateOutboxMessageStatus updates the status of an outbox message
func (db *DB) UpdateOutboxMessageStatus(id int64, status string, lastError string) error {
	now := timeutil.Now()
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
	now := timeutil.Now()
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

// IncrementOutboxMessageRetries increments the retry counter and schedules
// the next attempt with growing backoff: 1m → 5m → 15m → 1h → 4h → 12h,
// indexed by the retries count BEFORE this failure. Past the last step the
// delay stays 12h, but the send tasks mark the message failed after
// maxRetries attempts anyway.
// Returns the new retry count, so callers decide whether to give up from the
// stored value rather than from the copy they read when the task was created.
func (db *DB) IncrementOutboxMessageRetries(id int64, lastError string) (int, error) {
	now := timeutil.Now()

	// $2 is cast explicitly. It appears both as a bigint column value and as
	// the left operand of an addition with integer literals, and Postgres will
	// not deduce one type for both — it fails the whole statement with
	// "inconsistent types deduced for parameter $2". Which it did, on every
	// attempt: no failure was ever counted, next_attempt_at stayed 0, and a
	// permanently rejected message went on being retried once per scheduler
	// cycle indefinitely instead of backing off and being marked failed.
	//
	// The CASE reads the pre-update value of retries, so the first failure
	// waits a minute.
	query := `
		UPDATE outbox_messages
		SET retries = COALESCE(retries, 0) + 1, last_error = $1, updated_at = $2::bigint,
		    next_attempt_at = $2::bigint + (CASE LEAST(COALESCE(retries, 0), 5)
		        WHEN 0 THEN 60000
		        WHEN 1 THEN 300000
		        WHEN 2 THEN 900000
		        WHEN 3 THEN 3600000
		        WHEN 4 THEN 14400000
		        ELSE 43200000
		    END)
		WHERE id = $3
		RETURNING retries
	`

	var retries int
	if err := db.QueryRow(query, lastError, now, id).Scan(&retries); err != nil {
		return 0, fmt.Errorf("failed to increment retries: %w", err)
	}

	return retries, nil
}

// RecoverStrandedOutboxMessages moves messages left in 'sending' back to
// 'pending' and reports how many were freed.
//
// Nothing else ever clears that status. It is set just before a send begins, and
// if the process stops in between — a restart, a crash — the row becomes
// untouchable, because the scheduler only ever picks up 'pending'. One such
// message sat in 'sending' for two months.
//
// Meant to be called at startup, where nothing can be genuinely in flight yet.
// It assumes a single mailserver instance per database; with two running, one
// starting up could hand the other's in-flight message back to the queue.
func (db *DB) RecoverStrandedOutboxMessages() (int64, error) {
	result, err := db.Exec(`
		UPDATE outbox_messages
		SET status = 'pending', updated_at = $1
		WHERE status = 'sending'
	`, timeutil.Now())
	if err != nil {
		return 0, fmt.Errorf("failed to recover stranded outbox messages: %w", err)
	}

	freed, err := result.RowsAffected()
	if err != nil {
		return 0, nil // the update itself succeeded; the count is cosmetic
	}

	return freed, nil
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
		var sentAt sql.NullInt64
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
			msg.SentAt = sentAt.Int64
		}
		if lastError.Valid {
			msg.LastError = lastError.String
		}

		messages = append(messages, msg)
	}

	return messages, nil
}
