package importer

import (
	"fmt"
	"log"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/emersion/go-ical"
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
	todo, err := database.GetAttendeeBackfillCandidates()
	if err != nil {
		return fmt.Errorf("attendee backfill: %w", err)
	}

	var backfilled, organizerFilled int
	for _, p := range todo {
		// Fast skip: if no ATTENDEE substring AND organizer already set,
		// there's nothing to do for this row.
		hasAttendee := strings.Contains(p.ICalData, "ATTENDEE")
		hasOrganizer := strings.Contains(p.ICalData, "ORGANIZER")
		needOrganizer := p.OrganizerEmail == "" && hasOrganizer
		if !hasAttendee && !needOrganizer {
			continue
		}

		// Parse via go-ical; fall back to the simple line-based parser when
		// the ICS is malformed (some sources emit unfolded lines or extra
		// VTIMEZONE preambles that confuse the strict parser).
		var attendees []models.CalendarAttendee
		var orgEmail, orgName string
		if cal, err := ical.NewDecoder(strings.NewReader(p.ICalData)).Decode(); err == nil {
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
				attendees = ParseAttendeesSimple(p.ICalData)
			}
			if needOrganizer {
				orgEmail, orgName = ParseOrganizerSimple(p.ICalData)
			}
		}

		if len(attendees) > 0 {
			if err := database.ReplaceAttendees(p.EventID, AttendeePtrs(attendees)); err != nil {
				log.Printf("backfill: ReplaceAttendees failed for event %d: %v", p.EventID, err)
				continue
			}
			backfilled++
		}

		if needOrganizer && orgEmail != "" {
			if err := database.SetEventOrganizer(p.EventID, orgEmail, orgName); err != nil {
				log.Printf("backfill: organizer update failed for event %d: %v", p.EventID, err)
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
