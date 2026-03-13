package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// CreateCalendarAttendee creates a new attendee for an event
func (db *DB) CreateCalendarAttendee(attendee *models.CalendarAttendee) error {
	attendee.CreatedAt = time.Now()
	attendee.UpdatedAt = time.Now()

	if attendee.Role == "" {
		attendee.Role = "REQ-PARTICIPANT"
	}
	if attendee.PartStat == "" {
		attendee.PartStat = "NEEDS-ACTION"
	}

	query := `
		INSERT INTO calendar_attendees (
			event_id, email, name, role, partstat, rsvp, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id
	`

	err := db.QueryRow(
		query,
		attendee.EventID, attendee.Email, attendee.Name,
		attendee.Role, attendee.PartStat, attendee.RSVP,
		attendee.CreatedAt, attendee.UpdatedAt,
	).Scan(&attendee.ID)

	if err != nil {
		return fmt.Errorf("failed to create attendee: %w", err)
	}

	return nil
}

// GetAttendeesByEventID retrieves all attendees for an event
func (db *DB) GetAttendeesByEventID(eventID int64) ([]*models.CalendarAttendee, error) {
	query := `
		SELECT id, event_id, email, COALESCE(name, ''), role, partstat, rsvp, created_at, updated_at
		FROM calendar_attendees
		WHERE event_id = $1
		ORDER BY id
	`

	rows, err := db.Query(query, eventID)
	if err != nil {
		return nil, fmt.Errorf("failed to get attendees: %w", err)
	}
	defer rows.Close()

	var attendees []*models.CalendarAttendee
	for rows.Next() {
		attendee := &models.CalendarAttendee{}
		err := rows.Scan(
			&attendee.ID, &attendee.EventID, &attendee.Email, &attendee.Name,
			&attendee.Role, &attendee.PartStat, &attendee.RSVP,
			&attendee.CreatedAt, &attendee.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan attendee: %w", err)
		}
		attendees = append(attendees, attendee)
	}

	return attendees, nil
}

// GetAttendeeByEmail retrieves an attendee by event ID and email
func (db *DB) GetAttendeeByEmail(eventID int64, email string) (*models.CalendarAttendee, error) {
	attendee := &models.CalendarAttendee{}

	query := `
		SELECT id, event_id, email, COALESCE(name, ''), role, partstat, rsvp, created_at, updated_at
		FROM calendar_attendees
		WHERE event_id = $1 AND email = $2
	`

	err := db.QueryRow(query, eventID, email).Scan(
		&attendee.ID, &attendee.EventID, &attendee.Email, &attendee.Name,
		&attendee.Role, &attendee.PartStat, &attendee.RSVP,
		&attendee.CreatedAt, &attendee.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get attendee: %w", err)
	}

	return attendee, nil
}

// UpdateAttendeePartStat updates an attendee's participation status
func (db *DB) UpdateAttendeePartStat(eventID int64, email, partstat string) error {
	query := `
		UPDATE calendar_attendees
		SET partstat = $1, updated_at = $2
		WHERE event_id = $3 AND email = $4
	`

	result, err := db.Exec(query, partstat, time.Now(), eventID, email)
	if err != nil {
		return fmt.Errorf("failed to update attendee partstat: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("attendee not found")
	}

	return nil
}

// UpdateCalendarAttendee updates an attendee
func (db *DB) UpdateCalendarAttendee(attendee *models.CalendarAttendee) error {
	attendee.UpdatedAt = time.Now()

	query := `
		UPDATE calendar_attendees
		SET name = $1, role = $2, partstat = $3, rsvp = $4, updated_at = $5
		WHERE id = $6
	`

	_, err := db.Exec(
		query,
		attendee.Name, attendee.Role, attendee.PartStat, attendee.RSVP,
		attendee.UpdatedAt, attendee.ID,
	)

	if err != nil {
		return fmt.Errorf("failed to update attendee: %w", err)
	}

	return nil
}

// DeleteCalendarAttendee deletes an attendee
func (db *DB) DeleteCalendarAttendee(id int64) error {
	query := `DELETE FROM calendar_attendees WHERE id = $1`
	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete attendee: %w", err)
	}
	return nil
}

// DeleteAttendeesByEventID deletes all attendees for an event
func (db *DB) DeleteAttendeesByEventID(eventID int64) error {
	query := `DELETE FROM calendar_attendees WHERE event_id = $1`
	_, err := db.Exec(query, eventID)
	if err != nil {
		return fmt.Errorf("failed to delete attendees: %w", err)
	}
	return nil
}

// ReplaceAttendees replaces all attendees for an event
func (db *DB) ReplaceAttendees(eventID int64, attendees []*models.CalendarAttendee) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Delete existing attendees
	_, err = tx.Exec(`DELETE FROM calendar_attendees WHERE event_id = $1`, eventID)
	if err != nil {
		return fmt.Errorf("failed to delete existing attendees: %w", err)
	}

	// Insert new attendees
	now := time.Now()
	for _, attendee := range attendees {
		attendee.EventID = eventID
		attendee.CreatedAt = now
		attendee.UpdatedAt = now

		if attendee.Role == "" {
			attendee.Role = "REQ-PARTICIPANT"
		}
		if attendee.PartStat == "" {
			attendee.PartStat = "NEEDS-ACTION"
		}

		query := `
			INSERT INTO calendar_attendees (
				event_id, email, name, role, partstat, rsvp, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id
		`

		err := tx.QueryRow(
			query,
			attendee.EventID, attendee.Email, attendee.Name,
			attendee.Role, attendee.PartStat, attendee.RSVP,
			attendee.CreatedAt, attendee.UpdatedAt,
		).Scan(&attendee.ID)

		if err != nil {
			return fmt.Errorf("failed to create attendee %s: %w", attendee.Email, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// GetEventWithAttendees retrieves an event with its attendees
func (db *DB) GetEventWithAttendees(eventID int64) (*models.CalendarEvent, error) {
	event, err := db.GetEventByID(eventID)
	if err != nil {
		return nil, err
	}

	attendees, err := db.GetAttendeesByEventID(eventID)
	if err != nil {
		return nil, err
	}

	// Convert to non-pointer slice for model
	event.Attendees = make([]models.CalendarAttendee, len(attendees))
	for i, a := range attendees {
		event.Attendees[i] = *a
	}

	return event, nil
}

// GetEventsByUserEmail retrieves events where the user is an attendee
func (db *DB) GetEventsByUserEmail(email string, start, end time.Time) ([]*models.CalendarEvent, error) {
	query := `
		SELECT DISTINCT e.id, e.calendar_id, e.uid, COALESCE(e.remote_id, ''), e.ical_data,
		       COALESCE(e.summary, ''), COALESCE(e.description, ''), COALESCE(e.location, ''),
		       e.dtstart, e.dtend, e.all_day,
		       COALESCE(e.organizer_email, ''), COALESCE(e.organizer_name, ''), COALESCE(e.sequence, 0), COALESCE(e.status, 'CONFIRMED'),
		       COALESCE(e.rrule, ''), COALESCE(e.recurrence_id, ''), COALESCE(e.etag, ''), e.local_modified,
		       e.created_at, e.updated_at
		FROM calendar_events e
		JOIN calendar_attendees a ON e.id = a.event_id
		WHERE a.email = $1
		  AND e.dtstart >= $2 AND e.dtstart < $3
		ORDER BY e.dtstart
	`

	rows, err := db.Query(query, email, start, end)
	if err != nil {
		return nil, fmt.Errorf("failed to get events by attendee: %w", err)
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
