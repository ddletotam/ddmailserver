package db

import (
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// QueueFlagSync adds or updates a flag sync entry for a message
// Uses upsert to handle rapid successive changes (latest wins)
func (db *DB) QueueFlagSync(messageID, accountID int64, remoteFolder string, remoteUID uint32, seen, flagged, answered, deleted bool) error {
	query := `
		INSERT INTO flag_sync_queue (message_id, account_id, remote_folder, remote_uid, seen, flagged, answered, deleted, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (message_id) DO UPDATE SET
			seen = EXCLUDED.seen,
			flagged = EXCLUDED.flagged,
			answered = EXCLUDED.answered,
			deleted = EXCLUDED.deleted,
			created_at = EXCLUDED.created_at
	`

	_, err := db.Exec(query, messageID, accountID, remoteFolder, remoteUID, seen, flagged, answered, deleted, time.Now())
	if err != nil {
		return fmt.Errorf("failed to queue flag sync: %w", err)
	}

	return nil
}

// GetPendingFlagSync retrieves pending flag sync entries for an account
func (db *DB) GetPendingFlagSync(accountID int64, limit int) ([]*models.FlagSyncEntry, error) {
	query := `
		SELECT id, message_id, account_id, remote_folder, remote_uid, seen, flagged, answered, deleted, created_at
		FROM flag_sync_queue
		WHERE account_id = $1
		ORDER BY created_at ASC
		LIMIT $2
	`

	rows, err := db.Query(query, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get pending flag sync: %w", err)
	}
	defer rows.Close()

	var entries []*models.FlagSyncEntry
	for rows.Next() {
		e := &models.FlagSyncEntry{}
		err := rows.Scan(
			&e.ID, &e.MessageID, &e.AccountID, &e.RemoteFolder, &e.RemoteUID,
			&e.Seen, &e.Flagged, &e.Answered, &e.Deleted, &e.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan flag sync entry: %w", err)
		}
		entries = append(entries, e)
	}

	return entries, nil
}

// DeleteFlagSyncEntry removes a completed flag sync entry
func (db *DB) DeleteFlagSyncEntry(id int64) error {
	query := `DELETE FROM flag_sync_queue WHERE id = $1`
	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete flag sync entry: %w", err)
	}
	return nil
}

// GetAccountsWithPendingFlagSync returns account IDs that have pending flag sync entries
func (db *DB) GetAccountsWithPendingFlagSync() ([]int64, error) {
	query := `SELECT DISTINCT account_id FROM flag_sync_queue`

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to get accounts with pending flag sync: %w", err)
	}
	defer rows.Close()

	var accountIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan account ID: %w", err)
		}
		accountIDs = append(accountIDs, id)
	}

	return accountIDs, nil
}

// CleanupOldFlagSync removes flag sync entries older than the given duration
func (db *DB) CleanupOldFlagSync(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	query := `DELETE FROM flag_sync_queue WHERE created_at < $1`

	result, err := db.Exec(query, cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup old flag sync: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get affected rows: %w", err)
	}

	return count, nil
}
