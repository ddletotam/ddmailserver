package db

import (
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// QueueFlagSync adds or updates a flag sync entry for a message.
// Uses upsert to handle rapid successive changes (latest wins), with two
// exceptions to plain "latest wins":
//   - `deleted` is sticky (OR): once a delete is queued the message must
//     disappear from the source server even if a later flag change races in —
//     otherwise the delete is silently lost and the two sides diverge.
//   - retry/backoff state resets, so a fresh local change is attempted
//     immediately instead of inheriting the previous failure's backoff.
func (db *DB) QueueFlagSync(messageID, accountID int64, remoteFolder string, remoteUID uint32, seen, flagged, answered, deleted bool) error {
	query := `
		INSERT INTO flag_sync_queue (message_id, account_id, remote_folder, remote_uid, seen, flagged, answered, deleted, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (message_id) DO UPDATE SET
			seen = EXCLUDED.seen,
			flagged = EXCLUDED.flagged,
			answered = EXCLUDED.answered,
			deleted = flag_sync_queue.deleted OR EXCLUDED.deleted,
			created_at = EXCLUDED.created_at,
			retry_count = 0,
			last_error = '',
			next_attempt_at = 0
	`

	_, err := db.Exec(query, messageID, accountID, remoteFolder, remoteUID, seen, flagged, answered, deleted, timeutil.Now())
	if err != nil {
		return fmt.Errorf("failed to queue flag sync: %w", err)
	}

	return nil
}

// GetPendingFlagSync retrieves pending flag sync entries for an account.
// Entries that previously failed sit in the queue with `next_attempt_at` in
// the future (exponential backoff capped at 24h); only rows whose backoff
// has elapsed are returned, so a dead source server doesn't get hammered
// with a fresh connect+LOGIN on every scheduler tick.
func (db *DB) GetPendingFlagSync(accountID int64, limit int) ([]*models.FlagSyncEntry, error) {
	query := `
		SELECT id, message_id, account_id, remote_folder, remote_uid, seen, flagged, answered, deleted,
		       created_at, COALESCE(retry_count, 0), COALESCE(last_error, '')
		FROM flag_sync_queue
		WHERE account_id = $1 AND COALESCE(next_attempt_at, 0) <= $2
		ORDER BY created_at ASC
		LIMIT $3
	`

	rows, err := db.Query(query, accountID, timeutil.Now(), limit)
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
			&e.RetryCount, &e.LastError,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan flag sync entry: %w", err)
		}
		entries = append(entries, e)
	}

	return entries, nil
}

// MarkFlagSyncFailed bumps retry_count, stores the error (truncated), and
// pushes next_attempt_at forward. Same atomic SQL backoff as
// MarkCalendarEventSyncFailed: 1m → 5m → 30m → 3h → 12h → 24h cap.
func (db *DB) MarkFlagSyncFailed(id int64, errMsg string) error {
	if len(errMsg) > 1024 {
		errMsg = errMsg[:1024] + "…"
	}
	now := timeutil.Now()
	query := `
		UPDATE flag_sync_queue
		SET retry_count = retry_count + 1,
		    last_error = $1,
		    last_attempt_at = $2::bigint,
		    next_attempt_at = $2::bigint + CASE
		        WHEN retry_count + 1 = 1 THEN 60000
		        WHEN retry_count + 1 = 2 THEN 300000
		        WHEN retry_count + 1 = 3 THEN 1800000
		        WHEN retry_count + 1 = 4 THEN 10800000
		        WHEN retry_count + 1 = 5 THEN 43200000
		        ELSE 86400000
		    END
		WHERE id = $3
	`
	if _, err := db.Exec(query, errMsg, now, id); err != nil {
		return fmt.Errorf("mark flag sync failed: %w", err)
	}
	return nil
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

// GetAccountsWithPendingFlagSync returns account IDs that have flag sync
// entries whose backoff has elapsed — accounts where every entry is still
// backing off are skipped so the scheduler doesn't spawn no-op tasks.
func (db *DB) GetAccountsWithPendingFlagSync() ([]int64, error) {
	query := `SELECT DISTINCT account_id FROM flag_sync_queue WHERE COALESCE(next_attempt_at, 0) <= $1`

	rows, err := db.Query(query, timeutil.Now())
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
	cutoffMs := timeutil.Now() - olderThan.Milliseconds()
	query := `DELETE FROM flag_sync_queue WHERE created_at < $1`

	result, err := db.Exec(query, cutoffMs)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup old flag sync: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get affected rows: %w", err)
	}

	return count, nil
}
