package db

import (
	"database/sql"
	"fmt"

	"github.com/lib/pq"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// CreateCalendarEvent creates a new calendar event
func (db *DB) CreateCalendarEvent(event *models.CalendarEvent) error {
	event.CreatedAt = timeutil.Now()
	event.UpdatedAt = timeutil.Now()

	if event.Status == "" {
		event.Status = "CONFIRMED"
	}

	var remoteID, description, location, rrule, recurrenceID, etag sql.NullString
	var organizerEmail, organizerName sql.NullString
	if event.RemoteID != "" {
		remoteID = sql.NullString{String: event.RemoteID, Valid: true}
	}
	if event.Description != "" {
		description = sql.NullString{String: event.Description, Valid: true}
	}
	if event.Location != "" {
		location = sql.NullString{String: event.Location, Valid: true}
	}
	if event.RRule != "" {
		rrule = sql.NullString{String: event.RRule, Valid: true}
	}
	if event.RecurrenceID != "" {
		recurrenceID = sql.NullString{String: event.RecurrenceID, Valid: true}
	}
	if event.ETag != "" {
		etag = sql.NullString{String: event.ETag, Valid: true}
	}
	if event.OrganizerEmail != "" {
		organizerEmail = sql.NullString{String: event.OrganizerEmail, Valid: true}
	}
	if event.OrganizerName != "" {
		organizerName = sql.NullString{String: event.OrganizerName, Valid: true}
	}

	query := `
		INSERT INTO calendar_events (
			calendar_id, uid, remote_id, ical_data,
			summary, description, location, dtstart, dtend, all_day,
			organizer_email, organizer_name, sequence, status,
			rrule, recurrence_id, etag, local_modified,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
		RETURNING id
	`

	err := db.QueryRow(
		query,
		event.CalendarID, event.UID, remoteID, event.ICalData,
		event.Summary, description, location, event.DTStart, event.DTEnd, event.AllDay,
		organizerEmail, organizerName, event.Sequence, event.Status,
		rrule, recurrenceID, etag, event.LocalModified,
		event.CreatedAt, event.UpdatedAt,
	).Scan(&event.ID)

	if err != nil {
		return fmt.Errorf("failed to create calendar event: %w", err)
	}

	return nil
}

// GetEventsByCalendarID retrieves all events for a calendar
func (db *DB) GetEventsByCalendarID(calendarID int64) ([]*models.CalendarEvent, error) {
	query := `
		SELECT id, calendar_id, uid, COALESCE(remote_id, ''), ical_data,
		       COALESCE(summary, ''), COALESCE(description, ''), COALESCE(location, ''),
		       dtstart, dtend, all_day,
		       COALESCE(organizer_email, ''), COALESCE(organizer_name, ''), COALESCE(sequence, 0), COALESCE(status, 'CONFIRMED'),
		       COALESCE(rrule, ''), COALESCE(recurrence_id, ''), COALESCE(etag, ''), local_modified,
		       created_at, updated_at
		FROM calendar_events
		WHERE calendar_id = $1
		ORDER BY dtstart
	`

	rows, err := db.Query(query, calendarID)
	if err != nil {
		return nil, fmt.Errorf("failed to get events: %w", err)
	}
	defer rows.Close()

	return scanCalendarEvents(rows)
}

// GetEventByID retrieves an event by ID
func (db *DB) GetEventByID(id int64) (*models.CalendarEvent, error) {
	event := &models.CalendarEvent{}

	query := `
		SELECT id, calendar_id, uid, COALESCE(remote_id, ''), ical_data,
		       COALESCE(summary, ''), COALESCE(description, ''), COALESCE(location, ''),
		       dtstart, dtend, all_day,
		       COALESCE(organizer_email, ''), COALESCE(organizer_name, ''), COALESCE(sequence, 0), COALESCE(status, 'CONFIRMED'),
		       COALESCE(rrule, ''), COALESCE(recurrence_id, ''), COALESCE(etag, ''), local_modified,
		       created_at, updated_at
		FROM calendar_events
		WHERE id = $1
	`

	var dtEnd sql.NullInt64
	err := db.QueryRow(query, id).Scan(
		&event.ID, &event.CalendarID, &event.UID, &event.RemoteID, &event.ICalData,
		&event.Summary, &event.Description, &event.Location,
		&event.DTStart, &dtEnd, &event.AllDay,
		&event.OrganizerEmail, &event.OrganizerName, &event.Sequence, &event.Status,
		&event.RRule, &event.RecurrenceID, &event.ETag, &event.LocalModified,
		&event.CreatedAt, &event.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("event not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get event: %w", err)
	}

	if dtEnd.Valid {
		val := dtEnd.Int64
		event.DTEnd = &val
	}

	return event, nil
}

// GetEventByUID retrieves an event by calendar and UID
func (db *DB) GetEventByUID(calendarID int64, uid string) (*models.CalendarEvent, error) {
	event := &models.CalendarEvent{}

	query := `
		SELECT id, calendar_id, uid, COALESCE(remote_id, ''), ical_data,
		       COALESCE(summary, ''), COALESCE(description, ''), COALESCE(location, ''),
		       dtstart, dtend, all_day,
		       COALESCE(organizer_email, ''), COALESCE(organizer_name, ''), COALESCE(sequence, 0), COALESCE(status, 'CONFIRMED'),
		       COALESCE(rrule, ''), COALESCE(recurrence_id, ''), COALESCE(etag, ''), local_modified,
		       created_at, updated_at
		FROM calendar_events
		WHERE calendar_id = $1 AND uid = $2
	`

	var dtEnd sql.NullInt64
	err := db.QueryRow(query, calendarID, uid).Scan(
		&event.ID, &event.CalendarID, &event.UID, &event.RemoteID, &event.ICalData,
		&event.Summary, &event.Description, &event.Location,
		&event.DTStart, &dtEnd, &event.AllDay,
		&event.OrganizerEmail, &event.OrganizerName, &event.Sequence, &event.Status,
		&event.RRule, &event.RecurrenceID, &event.ETag, &event.LocalModified,
		&event.CreatedAt, &event.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil // Not found, return nil without error
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get event: %w", err)
	}

	if dtEnd.Valid {
		val := dtEnd.Int64
		event.DTEnd = &val
	}

	return event, nil
}

// GetEventsForCalendarsInRange retrieves events for multiple calendars within
// a time window (ms since epoch). Used by the desktop API which requests
// "events visible in the current view" with a single round-trip.
//
// Includes any event whose interval overlaps [startMs, endMs) and any event
// with an RRule (recurring events expand to instances client-side; the row
// has to come back even if the master DTSTART is outside the window).
func (db *DB) GetEventsForCalendarsInRange(calendarIDs []int64, startMs, endMs int64) ([]*models.CalendarEvent, error) {
	if len(calendarIDs) == 0 {
		return []*models.CalendarEvent{}, nil
	}

	query := `
		SELECT id, calendar_id, uid, COALESCE(remote_id, ''), ical_data,
		       COALESCE(summary, ''), COALESCE(description, ''), COALESCE(location, ''),
		       dtstart, dtend, all_day,
		       COALESCE(organizer_email, ''), COALESCE(organizer_name, ''), COALESCE(sequence, 0), COALESCE(status, 'CONFIRMED'),
		       COALESCE(rrule, ''), COALESCE(recurrence_id, ''), COALESCE(etag, ''), local_modified,
		       created_at, updated_at
		FROM calendar_events
		WHERE calendar_id = ANY($1)
		  AND soft_deleted_at IS NULL
		  AND ((dtstart >= $2 AND dtstart < $3)
		       OR (dtend > $2 AND dtend <= $3)
		       OR (dtstart < $2 AND dtend > $3)
		       OR rrule IS NOT NULL AND rrule <> '')
		ORDER BY dtstart
	`

	rows, err := db.Query(query, pq.Array(calendarIDs), startMs, endMs)
	if err != nil {
		return nil, fmt.Errorf("failed to get events: %w", err)
	}
	defer rows.Close()

	return scanCalendarEvents(rows)
}

// GetEventsByTimeRange retrieves events within a time range (ms since epoch)
func (db *DB) GetEventsByTimeRange(calendarID int64, startMs, endMs int64) ([]*models.CalendarEvent, error) {
	query := `
		SELECT id, calendar_id, uid, COALESCE(remote_id, ''), ical_data,
		       COALESCE(summary, ''), COALESCE(description, ''), COALESCE(location, ''),
		       dtstart, dtend, all_day,
		       COALESCE(organizer_email, ''), COALESCE(organizer_name, ''), COALESCE(sequence, 0), COALESCE(status, 'CONFIRMED'),
		       COALESCE(rrule, ''), COALESCE(recurrence_id, ''), COALESCE(etag, ''), local_modified,
		       created_at, updated_at
		FROM calendar_events
		WHERE calendar_id = $1
		  AND ((dtstart >= $2 AND dtstart < $3)
		       OR (dtend > $2 AND dtend <= $3)
		       OR (dtstart < $2 AND dtend > $3)
		       OR rrule IS NOT NULL)
		ORDER BY dtstart
	`

	rows, err := db.Query(query, calendarID, startMs, endMs)
	if err != nil {
		return nil, fmt.Errorf("failed to get events: %w", err)
	}
	defer rows.Close()

	return scanCalendarEvents(rows)
}

// UpdateCalendarEvent updates an event
func (db *DB) UpdateCalendarEvent(event *models.CalendarEvent) error {
	event.UpdatedAt = timeutil.Now()

	query := `
		UPDATE calendar_events
		SET ical_data = $1, summary = $2, description = $3, location = $4,
		    dtstart = $5, dtend = $6, all_day = $7,
		    organizer_email = $8, organizer_name = $9, sequence = $10, status = $11,
		    rrule = $12, recurrence_id = $13,
		    etag = $14, local_modified = $15, updated_at = $16
		WHERE id = $17
	`

	_, err := db.Exec(
		query,
		event.ICalData, event.Summary, event.Description, event.Location,
		event.DTStart, event.DTEnd, event.AllDay,
		event.OrganizerEmail, event.OrganizerName, event.Sequence, event.Status,
		event.RRule, event.RecurrenceID,
		event.ETag, event.LocalModified, event.UpdatedAt, event.ID,
	)

	if err != nil {
		return fmt.Errorf("failed to update event: %w", err)
	}

	return nil
}

// DeleteCalendarEvent deletes an event
func (db *DB) DeleteCalendarEvent(id int64) error {
	query := `DELETE FROM calendar_events WHERE id = $1`
	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete event: %w", err)
	}
	return nil
}

// DeleteCalendarEventByUID deletes an event by UID
func (db *DB) DeleteCalendarEventByUID(calendarID int64, uid string) error {
	query := `DELETE FROM calendar_events WHERE calendar_id = $1 AND uid = $2`
	_, err := db.Exec(query, calendarID, uid)
	if err != nil {
		return fmt.Errorf("failed to delete event: %w", err)
	}
	return nil
}

// GetLocallyModifiedEvents retrieves events that need to be pushed to remote
func (db *DB) GetLocallyModifiedEvents(calendarID int64) ([]*models.CalendarEvent, error) {
	query := `
		SELECT id, calendar_id, uid, COALESCE(remote_id, ''), ical_data,
		       COALESCE(summary, ''), COALESCE(description, ''), COALESCE(location, ''),
		       dtstart, dtend, all_day,
		       COALESCE(organizer_email, ''), COALESCE(organizer_name, ''), COALESCE(sequence, 0), COALESCE(status, 'CONFIRMED'),
		       COALESCE(rrule, ''), COALESCE(recurrence_id, ''), COALESCE(etag, ''), local_modified,
		       created_at, updated_at
		FROM calendar_events
		WHERE calendar_id = $1 AND local_modified = true
	`

	rows, err := db.Query(query, calendarID)
	if err != nil {
		return nil, fmt.Errorf("failed to get modified events: %w", err)
	}
	defer rows.Close()

	return scanCalendarEvents(rows)
}

// MarkEventSynced marks an event as synchronized (not locally modified)
func (db *DB) MarkEventSynced(eventID int64, etag string) error {
	query := `UPDATE calendar_events SET local_modified = false, etag = $1, updated_at = $2 WHERE id = $3`
	_, err := db.Exec(query, etag, timeutil.Now(), eventID)
	if err != nil {
		return fmt.Errorf("failed to mark event synced: %w", err)
	}
	return nil
}

// UpdateEventRemoteID updates the remote_id of an event after pushing to remote server
func (db *DB) UpdateEventRemoteID(eventID int64, remoteID string) error {
	query := `UPDATE calendar_events SET remote_id = $1, updated_at = $2 WHERE id = $3`
	_, err := db.Exec(query, remoteID, timeutil.Now(), eventID)
	if err != nil {
		return fmt.Errorf("failed to update event remote ID: %w", err)
	}
	return nil
}

// GetAllEventUIDsForCalendar returns all UIDs for a calendar (for sync comparison)
func (db *DB) GetAllEventUIDsForCalendar(calendarID int64) (map[string]string, error) {
	query := `SELECT uid, COALESCE(etag, '') FROM calendar_events WHERE calendar_id = $1`

	rows, err := db.Query(query, calendarID)
	if err != nil {
		return nil, fmt.Errorf("failed to get event UIDs: %w", err)
	}
	defer rows.Close()

	uids := make(map[string]string)
	for rows.Next() {
		var uid, etag string
		if err := rows.Scan(&uid, &etag); err != nil {
			return nil, fmt.Errorf("failed to scan UID: %w", err)
		}
		uids[uid] = etag
	}

	return uids, nil
}

// EventIdentity is the least a sync needs to recognise an event it has seen
// before: the UID the feed gave it, and the content a feed cannot regenerate.
// Some feeds mint a fresh UID on every render, so the UID alone cannot answer
// "is this the same meeting" — see matchFeedEvents in internal/worker.
type EventIdentity struct {
	ID      int64
	UID     string
	ETag    string
	Summary string
	DTStart int64
}

// GetEventIdentitiesForCalendar returns one row per event, cheap enough to load
// for a whole calendar on every sync: no ical_data, no bodies.
func (db *DB) GetEventIdentitiesForCalendar(calendarID int64) ([]EventIdentity, error) {
	query := `
		SELECT id, uid, COALESCE(etag, ''), COALESCE(summary, ''), COALESCE(dtstart, 0)
		FROM calendar_events
		WHERE calendar_id = $1
	`

	rows, err := db.Query(query, calendarID)
	if err != nil {
		return nil, fmt.Errorf("failed to get event identities: %w", err)
	}
	defer rows.Close()

	var out []EventIdentity
	for rows.Next() {
		var e EventIdentity
		if err := rows.Scan(&e.ID, &e.UID, &e.ETag, &e.Summary, &e.DTStart); err != nil {
			return nil, fmt.Errorf("failed to scan event identity: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read event identities: %w", err)
	}

	return out, nil
}

// GetEventCountForCalendar returns the number of events in a calendar
func (db *DB) GetEventCountForCalendar(calendarID int64) (int, error) {
	query := `SELECT COUNT(*) FROM calendar_events WHERE calendar_id = $1`
	var count int
	err := db.QueryRow(query, calendarID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count events: %w", err)
	}
	return count, nil
}

// SyncEventChanges represents changes to be applied in a transaction
type SyncEventChanges struct {
	CalendarID int64
	Creates    []*models.CalendarEvent
	Updates    []*models.CalendarEvent
	DeleteUIDs []string
}

// ApplySyncChanges applies sync changes within a transaction
func (db *DB) ApplySyncChanges(changes *SyncEventChanges) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := timeutil.Now()

	// Apply deletes first
	for _, uid := range changes.DeleteUIDs {
		_, err := tx.Exec(`DELETE FROM calendar_events WHERE calendar_id = $1 AND uid = $2`,
			changes.CalendarID, uid)
		if err != nil {
			return fmt.Errorf("failed to delete event %s: %w", uid, err)
		}
	}

	// Apply creates
	for _, event := range changes.Creates {
		event.CreatedAt = now
		event.UpdatedAt = now
		if event.Status == "" {
			event.Status = "CONFIRMED"
		}

		var remoteID, description, location, rrule, recurrenceID, etag sql.NullString
		var organizerEmail, organizerName sql.NullString
		if event.RemoteID != "" {
			remoteID = sql.NullString{String: event.RemoteID, Valid: true}
		}
		if event.Description != "" {
			description = sql.NullString{String: event.Description, Valid: true}
		}
		if event.Location != "" {
			location = sql.NullString{String: event.Location, Valid: true}
		}
		if event.RRule != "" {
			rrule = sql.NullString{String: event.RRule, Valid: true}
		}
		if event.RecurrenceID != "" {
			recurrenceID = sql.NullString{String: event.RecurrenceID, Valid: true}
		}
		if event.ETag != "" {
			etag = sql.NullString{String: event.ETag, Valid: true}
		}
		if event.OrganizerEmail != "" {
			organizerEmail = sql.NullString{String: event.OrganizerEmail, Valid: true}
		}
		if event.OrganizerName != "" {
			organizerName = sql.NullString{String: event.OrganizerName, Valid: true}
		}

		query := `
			INSERT INTO calendar_events (
				calendar_id, uid, remote_id, ical_data,
				summary, description, location, dtstart, dtend, all_day,
				organizer_email, organizer_name, sequence, status,
				rrule, recurrence_id, etag, local_modified,
				created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
			RETURNING id
		`

		err := tx.QueryRow(
			query,
			event.CalendarID, event.UID, remoteID, event.ICalData,
			event.Summary, description, location, event.DTStart, event.DTEnd, event.AllDay,
			organizerEmail, organizerName, event.Sequence, event.Status,
			rrule, recurrenceID, etag, event.LocalModified,
			event.CreatedAt, event.UpdatedAt,
		).Scan(&event.ID)

		if err != nil {
			return fmt.Errorf("failed to create event %s: %w", event.UID, err)
		}

		if err := ReplaceAttendeesTx(tx, event.ID, attendeesToPtrs(event.Attendees)); err != nil {
			return fmt.Errorf("failed to write attendees for %s: %w", event.UID, err)
		}
	}

	// Apply updates
	for _, event := range changes.Updates {
		event.UpdatedAt = now

		// uid is written too: a feed that regenerates UIDs still describes the
		// same events, and re-pointing the stored row at the new UID is what
		// keeps it from being deleted and recreated on every sync.
		query := `
			UPDATE calendar_events
			SET uid = $1, ical_data = $2, summary = $3, description = $4, location = $5,
			    dtstart = $6, dtend = $7, all_day = $8,
			    organizer_email = $9, organizer_name = $10, sequence = $11, status = $12,
			    rrule = $13, recurrence_id = $14,
			    etag = $15, local_modified = $16, updated_at = $17
			WHERE id = $18
		`

		_, err := tx.Exec(
			query,
			event.UID, event.ICalData, event.Summary, event.Description, event.Location,
			event.DTStart, event.DTEnd, event.AllDay,
			event.OrganizerEmail, event.OrganizerName, event.Sequence, event.Status,
			event.RRule, event.RecurrenceID,
			event.ETag, event.LocalModified, event.UpdatedAt, event.ID,
		)

		if err != nil {
			return fmt.Errorf("failed to update event %s: %w", event.UID, err)
		}

		if err := ReplaceAttendeesTx(tx, event.ID, attendeesToPtrs(event.Attendees)); err != nil {
			return fmt.Errorf("failed to update attendees for %s: %w", event.UID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

func attendeesToPtrs(in []models.CalendarAttendee) []*models.CalendarAttendee {
	out := make([]*models.CalendarAttendee, len(in))
	for i := range in {
		out[i] = &in[i]
	}
	return out
}

// scanCalendarEvents scans multiple calendar event rows
func scanCalendarEvents(rows *sql.Rows) ([]*models.CalendarEvent, error) {
	var events []*models.CalendarEvent
	for rows.Next() {
		event := &models.CalendarEvent{}
		var dtEnd sql.NullInt64

		err := rows.Scan(
			&event.ID, &event.CalendarID, &event.UID, &event.RemoteID, &event.ICalData,
			&event.Summary, &event.Description, &event.Location,
			&event.DTStart, &dtEnd, &event.AllDay,
			&event.OrganizerEmail, &event.OrganizerName, &event.Sequence, &event.Status,
			&event.RRule, &event.RecurrenceID, &event.ETag, &event.LocalModified,
			&event.CreatedAt, &event.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan event: %w", err)
		}

		if dtEnd.Valid {
			val := dtEnd.Int64
			event.DTEnd = &val
		}

		events = append(events, event)
	}

	return events, nil
}
