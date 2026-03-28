package db

import (
	"fmt"
	"log"
	"time"

	"github.com/yourusername/mailserver/internal/models"
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

	result, err := db.Exec(query, eventID, calendarID, sourceID, uid, remoteID, icalData, operation, time.Now())
	if err != nil {
		return fmt.Errorf("failed to queue calendar event sync: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	log.Printf("QueueCalendarEventSync: eventID=%d sourceID=%d operation=%s rowsAffected=%d", eventID, sourceID, operation, rowsAffected)

	return nil
}

// GetPendingCalendarEventSync retrieves pending calendar event sync entries for a source
func (db *DB) GetPendingCalendarEventSync(sourceID int64, limit int) ([]*models.CalendarEventSyncEntry, error) {
	query := `
		SELECT id, event_id, calendar_id, source_id, uid, COALESCE(remote_id, ''), COALESCE(ical_data, ''), operation, created_at
		FROM calendar_event_sync_queue
		WHERE source_id = $1
		ORDER BY created_at ASC
		LIMIT $2
	`

	rows, err := db.Query(query, sourceID, limit)
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
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan calendar event sync entry: %w", err)
		}
		entries = append(entries, e)
	}

	return entries, nil
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
