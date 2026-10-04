package db

import (
	"database/sql"
	"fmt"
)

// AttendeeBackfillCandidate is a calendar event whose structured attendee
// table is still empty while its raw ICS is present — the input of the
// startup attendee/organizer backfill (caldav/importer.BackfillAttendees).
type AttendeeBackfillCandidate struct {
	EventID        int64
	ICalData       string
	OrganizerEmail string
	OrganizerName  string
}

// GetAttendeeBackfillCandidates returns every event that has ical_data but no
// calendar_attendees rows. The caller re-parses the ICS and fills the gaps.
func (db *DB) GetAttendeeBackfillCandidates() ([]AttendeeBackfillCandidate, error) {
	rows, err := db.Query(`
		SELECT e.id, e.ical_data, e.organizer_email, e.organizer_name
		FROM calendar_events e
		WHERE e.ical_data IS NOT NULL AND e.ical_data != ''
		  AND NOT EXISTS (SELECT 1 FROM calendar_attendees a WHERE a.event_id = e.id)
	`)
	if err != nil {
		return nil, fmt.Errorf("query events for attendee backfill: %w", err)
	}
	defer rows.Close()

	var out []AttendeeBackfillCandidate
	for rows.Next() {
		var c AttendeeBackfillCandidate
		var orgEmail, orgName sql.NullString
		if err := rows.Scan(&c.EventID, &c.ICalData, &orgEmail, &orgName); err != nil {
			return nil, fmt.Errorf("scan attendee backfill row: %w", err)
		}
		c.OrganizerEmail = orgEmail.String
		c.OrganizerName = orgName.String
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attendee backfill rows: %w", err)
	}
	return out, nil
}

// SetEventOrganizer stores the ORGANIZER parsed out of an event's ICS.
func (db *DB) SetEventOrganizer(eventID int64, email, name string) error {
	if _, err := db.Exec(
		`UPDATE calendar_events SET organizer_email = $1, organizer_name = $2 WHERE id = $3`,
		email, name, eventID,
	); err != nil {
		return fmt.Errorf("set organizer of event %d: %w", eventID, err)
	}
	return nil
}
