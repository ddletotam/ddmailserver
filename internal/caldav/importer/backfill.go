package importer

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	"github.com/emersion/go-ical"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
)

// BackfillAttendees walks every calendar_events row whose calendar_attendees
// table is empty, re-parses its ical_data and writes the ATTENDEE list back.
//
// Idempotent: events whose source ICS has no ATTENDEE line short-circuit on a
// substring test, so they're cheap to revisit on every startup. Events that
// do have attendees get processed once, after which they no longer match the
// "empty calendar_attendees" predicate.
//
// This exists because earlier versions of the CalDAV client/server stored the
// raw ical_data but never populated the structured attendees table — leaving
// the desktop's RSVP bar permanently hidden for externally-synced meetings.
func BackfillAttendees(database *db.DB) error {
	rows, err := database.Query(`
		SELECT e.id, e.ical_data, e.organizer_email, e.organizer_name
		FROM calendar_events e
		WHERE e.ical_data IS NOT NULL AND e.ical_data != ''
		  AND NOT EXISTS (SELECT 1 FROM calendar_attendees a WHERE a.event_id = e.id)
	`)
	if err != nil {
		return fmt.Errorf("query events for backfill: %w", err)
	}
	defer rows.Close()

	type pending struct {
		id             int64
		icalData       string
		organizerEmail string
		organizerName  string
	}
	var todo []pending
	for rows.Next() {
		var p pending
		var orgEmail, orgName sql.NullString
		if err := rows.Scan(&p.id, &p.icalData, &orgEmail, &orgName); err != nil {
			return fmt.Errorf("scan event row: %w", err)
		}
		if orgEmail.Valid {
			p.organizerEmail = orgEmail.String
		}
		if orgName.Valid {
			p.organizerName = orgName.String
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate event rows: %w", err)
	}

	var backfilled, organizerFilled int
	for _, p := range todo {
		// Fast skip: if no ATTENDEE substring AND organizer already set,
		// there's nothing to do for this row.
		hasAttendee := strings.Contains(p.icalData, "ATTENDEE")
		hasOrganizer := strings.Contains(p.icalData, "ORGANIZER")
		needOrganizer := p.organizerEmail == "" && hasOrganizer
		if !hasAttendee && !needOrganizer {
			continue
		}

		// Parse via go-ical; fall back to the simple line-based parser when
		// the ICS is malformed (some sources emit unfolded lines or extra
		// VTIMEZONE preambles that confuse the strict parser).
		var attendees []models.CalendarAttendee
		var orgEmail, orgName string
		if cal, err := ical.NewDecoder(strings.NewReader(p.icalData)).Decode(); err == nil {
			for _, ev := range cal.Events() {
				if hasAttendee {
					attendees = ParseAttendees(&ev)
				}
				if needOrganizer {
					orgEmail, orgName = ParseOrganizer(&ev)
				}
				break
			}
		} else {
			if hasAttendee {
				attendees = ParseAttendeesSimple(p.icalData)
			}
			if needOrganizer {
				orgEmail, orgName = ParseOrganizerSimple(p.icalData)
			}
		}

		if len(attendees) > 0 {
			if err := database.ReplaceAttendees(p.id, AttendeePtrs(attendees)); err != nil {
				log.Printf("backfill: ReplaceAttendees failed for event %d: %v", p.id, err)
				continue
			}
			backfilled++
		}

		if needOrganizer && orgEmail != "" {
			if _, err := database.Exec(
				`UPDATE calendar_events SET organizer_email = $1, organizer_name = $2 WHERE id = $3`,
				orgEmail, orgName, p.id,
			); err != nil {
				log.Printf("backfill: organizer update failed for event %d: %v", p.id, err)
				continue
			}
			organizerFilled++
		}
	}

	if backfilled > 0 || organizerFilled > 0 {
		log.Printf("Backfilled attendees on %d event(s), organizer on %d event(s)", backfilled, organizerFilled)
	}
	return nil
}
