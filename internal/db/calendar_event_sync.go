package db

import (
	"fmt"
	"log"

	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// QueueCalendarEventSync adds or updates a calendar event sync entry
// Uses upsert to handle rapid successive changes (latest wins)
func (db *DB) QueueCalendarEventSync(eventID, calendarID, sourceID int64, uid, remoteID, icalData, operation string) error {
	query := `
		INSERT INTO calendar_event_sync_queue (event_id, calendar_id, source_id, uid, remote_id, ical_data, operation, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (event_id) DO UPDATE SET
			ical_data = EXCLUDED.ical_data,
			operation = EXCLUDED.operation,
			created_at = EXCLUDED.created_at
	`

	result, err := db.Exec(query, eventID, calendarID, sourceID, uid, remoteID, icalData, operation, timeutil.Now())
	if err != nil {
		return fmt.Errorf("failed to queue calendar event sync: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	log.Printf("QueueCalendarEventSync: eventID=%d sourceID=%d operation=%s rowsAffected=%d", eventID, sourceID, operation, rowsAffected)

	return nil
}

// GetPendingCalendarEventSync retrieves entries ready for another attempt.
// Entries that previously failed sit in the queue with `next_attempt_at` in
// the future (exponential backoff capped at 24h); we only pull rows whose
// backoff has elapsed so a chronically-failing source doesn't burn CPU.
func (db *DB) GetPendingCalendarEventSync(sourceID int64, limit int) ([]*models.CalendarEventSyncEntry, error) {
	query := `
		SELECT id, event_id, calendar_id, source_id, uid, COALESCE(remote_id, ''), COALESCE(ical_data, ''),
		       operation, created_at, COALESCE(retry_count, 0), COALESCE(last_error, '')
		FROM calendar_event_sync_queue
		WHERE source_id = $1 AND COALESCE(next_attempt_at, 0) <= $2
		ORDER BY created_at ASC
		LIMIT $3
	`

	rows, err := db.Query(query, sourceID, timeutil.Now(), limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get pending calendar event sync: %w", err)
	}
	defer rows.Close()

	var entries []*models.CalendarEventSyncEntry
	for rows.Next() {
		e := &models.CalendarEventSyncEntry{}
		err := rows.Scan(
			&e.ID, &e.EventID, &e.CalendarID, &e.SourceID,
			&e.UID, &e.RemoteID, &e.ICalData, &e.Operation, &e.CreatedAt,
			&e.RetryCount, &e.LastError,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan calendar event sync entry: %w", err)
		}
		entries = append(entries, e)
	}

	return entries, nil
}

// MarkCalendarEventSyncFailed bumps retry_count, stores the error truncated
// to a sane length, and pushes `next_attempt_at` forward via the backoff
// schedule. Errors longer than 1024 chars are clipped because some upstream
// servers dump 50KB XML on failure and we don't need that in the queue.
//
// Backoff is computed in SQL based on the post-increment retry_count to keep
// the bump+schedule atomic:
//
//	new retry → next delay
//	1 → 1m, 2 → 5m, 3 → 30m, 4 → 3h, 5 → 12h, 6+ → 24h cap.
func (db *DB) MarkCalendarEventSyncFailed(id int64, errMsg string) error {
	if len(errMsg) > 1024 {
		errMsg = errMsg[:1024] + "…"
	}
	now := timeutil.Now()
	query := `
		UPDATE calendar_event_sync_queue
		SET retry_count = retry_count + 1,
		    last_error = $1,
		    last_attempt_at = $2,
		    next_attempt_at = $2 + CASE
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
		return fmt.Errorf("mark sync failed: %w", err)
	}
	return nil
}

// CountPendingCalendarSyncFailures returns the number of entries for a source
// that have retry_count >= minRetries — i.e. failed at least that many
// times. Used by the warning-email task to decide whether the situation
// warrants telling the user.
func (db *DB) CountPendingCalendarSyncFailures(sourceID int64, minRetries int) (int, error) {
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM calendar_event_sync_queue WHERE source_id = $1 AND retry_count >= $2`,
		sourceID, minRetries,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count failures: %w", err)
	}
	return n, nil
}

// SampleFailingSyncEntries returns up to `limit` failing entries for a
// source. Used in the warning-email body to show concrete examples.
func (db *DB) SampleFailingSyncEntries(sourceID int64, minRetries, limit int) ([]*models.CalendarEventSyncEntry, error) {
	rows, err := db.Query(
		`SELECT id, event_id, calendar_id, source_id, uid, COALESCE(remote_id, ''),
		        COALESCE(ical_data, ''), operation, created_at,
		        COALESCE(retry_count, 0), COALESCE(last_error, '')
		 FROM calendar_event_sync_queue
		 WHERE source_id = $1 AND retry_count >= $2
		 ORDER BY retry_count DESC, created_at ASC
		 LIMIT $3`,
		sourceID, minRetries, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("sample failing entries: %w", err)
	}
	defer rows.Close()
	var out []*models.CalendarEventSyncEntry
	for rows.Next() {
		e := &models.CalendarEventSyncEntry{}
		if err := rows.Scan(
			&e.ID, &e.EventID, &e.CalendarID, &e.SourceID,
			&e.UID, &e.RemoteID, &e.ICalData, &e.Operation, &e.CreatedAt,
			&e.RetryCount, &e.LastError,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, e)
	}
	return out, nil
}

// GetCalendarSourceWarning loads warning state for a source. Missing row →
// last_warning_sent_at=0 and consecutive_failures=0, which is what we want
// (first failure: send immediately when threshold met; then daily).
func (db *DB) GetCalendarSourceWarning(sourceID int64) (int64, int, error) {
	var lastSent int64
	var consecutive int
	err := db.QueryRow(
		`SELECT last_warning_sent_at, consecutive_failures FROM calendar_source_warnings WHERE source_id = $1`,
		sourceID,
	).Scan(&lastSent, &consecutive)
	if err != nil && err.Error() == "sql: no rows in result set" {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("get source warning: %w", err)
	}
	return lastSent, consecutive, nil
}

// SetCalendarSourceWarning upserts the warning bookkeeping row.
func (db *DB) SetCalendarSourceWarning(sourceID, lastSent int64, consecutive int) error {
	_, err := db.Exec(
		`INSERT INTO calendar_source_warnings (source_id, last_warning_sent_at, consecutive_failures)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (source_id) DO UPDATE SET
		   last_warning_sent_at = EXCLUDED.last_warning_sent_at,
		   consecutive_failures = EXCLUDED.consecutive_failures`,
		sourceID, lastSent, consecutive,
	)
	if err != nil {
		return fmt.Errorf("set source warning: %w", err)
	}
	return nil
}

// DeleteCalendarEventSyncEntry removes a completed calendar event sync entry
func (db *DB) DeleteCalendarEventSyncEntry(id int64) error {
	query := `DELETE FROM calendar_event_sync_queue WHERE id = $1`
	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete calendar event sync entry: %w", err)
	}
	return nil
}

// GetSourcesWithPendingCalendarEventSync returns source IDs that have pending event sync entries
func (db *DB) GetSourcesWithPendingCalendarEventSync() ([]int64, error) {
	query := `SELECT DISTINCT source_id FROM calendar_event_sync_queue`

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to get sources with pending calendar event sync: %w", err)
	}
	defer rows.Close()

	var sourceIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan source ID: %w", err)
		}
		sourceIDs = append(sourceIDs, id)
	}

	return sourceIDs, nil
}
