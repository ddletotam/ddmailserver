package importer

import (
	"testing"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// Shared fixture lines; `ics` lives in timezone_test.go.
const (
	organizerLine = "ORGANIZER;CN=Boss:mailto:Boss@Example.COM"
	attendeeAnn   = "ATTENDEE;CN=Ann;ROLE=CHAIR;PARTSTAT=ACCEPTED;RSVP=TRUE:mailto:ann@example.com"
	attendeeBob   = "ATTENDEE;CN=Bob:mailto:bob@example.com"
)

// assertGuestList checks the organizer and the two attendees the fixtures below
// share, including the defaults filled in for the attendee that declares none.
func assertGuestList(t *testing.T, orgEmail, orgName string, attendees []models.CalendarAttendee) {
	t.Helper()

	if orgEmail != "boss@example.com" {
		t.Errorf("organizer email = %q, want boss@example.com", orgEmail)
	}
	if orgName != "Boss" {
		t.Errorf("organizer name = %q, want Boss", orgName)
	}

	if len(attendees) != 2 {
		t.Fatalf("got %d attendees, want 2", len(attendees))
	}

	ann := attendees[0]
	if ann.Email != "ann@example.com" || ann.Name != "Ann" || ann.Role != "CHAIR" ||
		ann.PartStat != "ACCEPTED" || !ann.RSVP {
		t.Errorf("attendee[0] = %+v, want ann@example.com/Ann/CHAIR/ACCEPTED/rsvp", ann)
	}

	// No ROLE or PARTSTAT on the wire: both must fall back to the RFC defaults
	// rather than reaching the database empty.
	bob := attendees[1]
	if bob.Email != "bob@example.com" || bob.Name != "Bob" ||
		bob.Role != "REQ-PARTICIPANT" || bob.PartStat != "NEEDS-ACTION" || bob.RSVP {
		t.Errorf("attendee[1] = %+v, want bob@example.com/Bob/REQ-PARTICIPANT/NEEDS-ACTION/no-rsvp", bob)
	}
}

// TestParseICSLiftsOrganizerAndAttendees covers the go-ical path: an ICS feed
// that ships a guest list must arrive with it, the same way the CalDAV client
// already does. Before this, ICS-sourced calendars landed with an empty
// calendar_attendees table and the desktop RSVP bar never appeared.
func TestParseICSLiftsOrganizerAndAttendees(t *testing.T) {
	data := ics(
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//Test//EN",
		"BEGIN:VEVENT",
		"UID:evt-1",
		"DTSTAMP:20260101T090000Z",
		"SUMMARY:Planning",
		"DTSTART:20260110T090000Z",
		"DTEND:20260110T100000Z",
		organizerLine,
		attendeeAnn,
		attendeeBob,
		"END:VEVENT",
		"END:VCALENDAR",
	)

	events, err := ParseICS(data)
	if err != nil {
		t.Fatalf("ParseICS: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}

	ev := events[0]
	assertGuestList(t, ev.OrganizerEmail, ev.OrganizerName, ev.Attendees)
}

// TestParseVEventLiftsOrganizerAndAttendees covers the text fallback, used when
// go-ical refuses the payload outright. It must not drop the guest list either.
func TestParseVEventLiftsOrganizerAndAttendees(t *testing.T) {
	vevent := ics(
		"BEGIN:VEVENT",
		"UID:evt-2",
		"SUMMARY:Planning",
		"DTSTART:20260110T090000Z",
		"DTEND:20260110T100000Z",
		organizerLine,
		attendeeAnn,
		attendeeBob,
		"END:VEVENT",
	)

	ev := parseVEvent(vevent, 0, newZoneResolver(vevent), time.UTC)
	if ev == nil {
		t.Fatal("parseVEvent returned nil")
	}

	assertGuestList(t, ev.OrganizerEmail, ev.OrganizerName, ev.Attendees)
}

// TestParseICSWithoutGuestListStaysEmpty pins the SmallKZ2 shape: a thin export
// with nothing but time, title and location must not synthesise an organizer or
// attendees out of thin air.
func TestParseICSWithoutGuestListStaysEmpty(t *testing.T) {
	data := ics(
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:spatie/icalendar-generator",
		"BEGIN:VEVENT",
		"UID:6a8e9bb005659",
		"DTSTAMP:20260826T075424Z",
		"SUMMARY:Team sync",
		"LOCATION:meet.example.com/room",
		"DTSTART:20260807T100000Z",
		"DTEND:20260807T110000Z",
		"END:VEVENT",
		"END:VCALENDAR",
	)

	events, err := ParseICS(data)
	if err != nil {
		t.Fatalf("ParseICS: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}

	ev := events[0]
	if ev.OrganizerEmail != "" || ev.OrganizerName != "" {
		t.Errorf("organizer = %q/%q, want empty", ev.OrganizerEmail, ev.OrganizerName)
	}
	if len(ev.Attendees) != 0 {
		t.Errorf("got %d attendees, want 0", len(ev.Attendees))
	}
}
