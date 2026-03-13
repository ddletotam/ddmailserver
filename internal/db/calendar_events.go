package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// CreateCalendarEvent creates a new calendar event
func (db *DB) CreateCalendarEvent(event *models.CalendarEvent) error {
	event.CreatedAt = time.Now()
	event.UpdatedAt = time.Now()

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

	var events []*models.CalendarEvent
	for rows.Next() {
		event := &models.CalendarEvent{}

		err := rows.Scan(
			&event.ID, &event.CalendarID, &event.UID, &event.RemoteID, &event.ICalData,
			&event.Summary, &event.Description, &event.Location,
			&event.DTStart, &event.DTEnd, &event.AllDay,
			&event.OrganizerEmail, &event.OrganizerName, &event.Sequence, &event.Status,
			&event.RRule, &event.RecurrenceID, &event.ETag, &event.LocalModified,
			&event.CreatedAt, &event.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan event: %w", err)
		}

		events = append(events, event)
	}

	return events, nil
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

	err := db.QueryRow(query, id).Scan(
		&event.ID, &event.CalendarID, &event.UID, &event.RemoteID, &event.ICalData,
		&event.Summary, &event.Description, &event.Location,
		&event.DTStart, &event.DTEnd, &event.AllDay,
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

	err := db.QueryRow(query, calendarID, uid).Scan(
		&event.ID, &event.CalendarID, &event.UID, &event.RemoteID, &event.ICalData,
		&event.Summary, &event.Description, &event.Location,
		&event.DTStart, &event.DTEnd, &event.AllDay,
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

	return event, nil
}

// GetEventsByTimeRange retrieves events within a time range
func (db *DB) GetEventsByTimeRange(calendarID int64, start, end time.Time) ([]*models.CalendarEvent, error) {
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

	rows, err := db.Query(query, calendarID, start, end)
	if err != nil {
		return nil, fmt.Errorf("failed to get events: %w", err)
	}
	defer rows.Close()

	var events []*models.CalendarEvent
	for rows.Next() {
		event := &models.CalendarEvent{}

		err := rows.Scan(
			&event.ID, &event.CalendarID, &event.UID, &event.RemoteID, &event.ICalData,
			&event.Summary, &event.Description, &event.Location,
			&event.DTStart, &event.DTEnd, &event.AllDay,
			&event.OrganizerEmail, &event.OrganizerName, &event.Sequence, &event.Status,
			&event.RRule, &event.RecurrenceID, &event.ETag, &event.LocalModified,
			&event.CreatedAt, &event.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan event: %w", err)
		}

		events = append(events, event)
	}

	return events, nil
}

// UpdateCalendarEvent updates an event
func (db *DB) UpdateCalendarEvent(event *models.CalendarEvent) error {
	event.UpdatedAt = time.Now()

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

	var events []*models.CalendarEvent
	for rows.Next() {
		event := &models.CalendarEvent{}

		err := rows.Scan(
			&event.ID, &event.CalendarID, &event.UID, &event.RemoteID, &event.ICalData,
			&event.Summary, &event.Description, &event.Location,
			&event.DTStart, &event.DTEnd, &event.AllDay,
			&event.OrganizerEmail, &event.OrganizerName, &event.Sequence, &event.Status,
			&event.RRule, &event.RecurrenceID, &event.ETag, &event.LocalModified,
			&event.CreatedAt, &event.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan event: %w", err)
		}

		events = append(events, event)
	}

	return events, nil
}

// MarkEventSynced marks an event as synchronized (not locally modified)
func (db *DB) MarkEventSynced(eventID int64, etag string) error {
	query := `UPDATE calendar_events SET local_modified = false, etag = $1, updated_at = $2 WHERE id = $3`
	_, err := db.Exec(query, etag, time.Now(), eventID)
	if err != nil {
		return fmt.Errorf("failed to mark event synced: %w", err)
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

	now := time.Now()

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
	}

	// Apply updates
	for _, event := range changes.Updates {
		event.UpdatedAt = now

		query := `
			UPDATE calendar_events
			SET ical_data = $1, summary = $2, description = $3, location = $4,
			    dtstart = $5, dtend = $6, all_day = $7,
			    organizer_email = $8, organizer_name = $9, sequence = $10, status = $11,
			    rrule = $12, recurrence_id = $13,
			    etag = $14, local_modified = $15, updated_at = $16
			WHERE id = $17
		`

		_, err := tx.Exec(
			query,
			event.ICalData, event.Summary, event.Description, event.Location,
			event.DTStart, event.DTEnd, event.AllDay,
			event.OrganizerEmail, event.OrganizerName, event.Sequence, event.Status,
			event.RRule, event.RecurrenceID,
			event.ETag, event.LocalModified, event.UpdatedAt, event.ID,
		)

		if err != nil {
			return fmt.Errorf("failed to update event %s: %w", event.UID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}
