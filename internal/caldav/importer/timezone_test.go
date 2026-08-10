package importer

import (
	"strings"
	"testing"
	"time"
)

// ics assembles an ICS payload with the CRLF line endings RFC 5545 requires.
func ics(lines ...string) string {
	return strings.Join(lines, "\r\n") + "\r\n"
}

// almatyFeed reproduces the shape a real subscribed feed arrives in: events
// qualified by a TZID, plus the VTIMEZONE that defines it.
func almatyFeed(offset string, dtstart string) string {
	return ics(
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//test//test//EN",
		"BEGIN:VTIMEZONE",
		"TZID:Asia/Almaty",
		"BEGIN:STANDARD",
		"DTSTART:20251109T160000",
		"TZOFFSETFROM:"+offset,
		"TZOFFSETTO:"+offset,
		"END:STANDARD",
		"END:VTIMEZONE",
		"BEGIN:VEVENT",
		"UID:event-1",
		// DTSTAMP is not decoration: go-ical refuses to encode a VEVENT without
		// one, and parseICalEvent then falls back to regenerating ical_data from
		// our own fields — which flattens everything to UTC and drops the zone.
		"DTSTAMP:20260801T000000Z",
		"SUMMARY:Discussion",
		"DTSTART;TZID=Asia/Almaty:"+dtstart,
		"DTEND;TZID=Asia/Almaty:20260810T130000",
		"END:VEVENT",
		"END:VCALENDAR",
	)
}

// parsedEvent is the handful of fields these tests care about, in UTC.
type parsedEvent struct {
	Start  time.Time
	End    time.Time
	Raw    string
	AllDay bool
}

func mustParseOne(t *testing.T, payload string) parsedEvent {
	t.Helper()

	events, err := ParseICS(payload)
	if err != nil {
		t.Fatalf("ParseICS: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event, got %d", len(events))
	}

	ev := events[0]
	out := parsedEvent{
		Start:  time.UnixMilli(ev.DTStart).UTC(),
		Raw:    ev.ICalData,
		AllDay: ev.AllDay,
	}
	if ev.DTEnd != nil {
		out.End = time.UnixMilli(*ev.DTEnd).UTC()
	}
	return out
}

// TestAlmatyFeedIsNotShifted is the regression for the defect this file exists
// to prevent: noon in Asia/Almaty was being stored as 06:00 UTC instead of
// 07:00 UTC, one hour early, for every event in the feed.
//
// The assertion holds whichever zone database the test host carries, and that is
// the point. On a current host, IANA and the feed both say UTC+05 and agree. On
// a host still on tzdata 2023c, IANA says UTC+06 — Kazakhstan moved to UTC+05 on
// 2024-03-01 — the two disagree, Almaty has no seasonal shift to explain it, and
// the feed's own declaration wins. Both roads lead to 07:00 UTC.
func TestAlmatyFeedIsNotShifted(t *testing.T) {
	got := mustParseOne(t, almatyFeed("+0500", "20260810T120000"))

	want := time.Date(2026, 8, 10, 7, 0, 0, 0, time.UTC)
	if !got.Start.Equal(want) {
		t.Errorf("DTSTART = %s, want %s", got.Start.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	wantEnd := time.Date(2026, 8, 10, 8, 0, 0, 0, time.UTC)
	if !got.End.Equal(wantEnd) {
		t.Errorf("DTEND = %s, want %s", got.End.Format(time.RFC3339), wantEnd.Format(time.RFC3339))
	}
}

// TestStoredEventKeepsItsZoneDefinition guards the other half of the same
// problem: we used to keep the TZID parameter but drop the VTIMEZONE that gives
// it meaning, leaving every later reader to guess the offset from its own
// zone database.
func TestStoredEventKeepsItsZoneDefinition(t *testing.T) {
	got := mustParseOne(t, almatyFeed("+0500", "20260810T120000"))

	if !strings.Contains(got.Raw, "BEGIN:VTIMEZONE") {
		t.Errorf("stored ical_data lost its VTIMEZONE:\n%s", got.Raw)
	}
	if !strings.Contains(got.Raw, "TZID:Asia/Almaty") {
		t.Errorf("stored ical_data lost the TZID definition:\n%s", got.Raw)
	}
	if tzIdx, evIdx := strings.Index(got.Raw, "BEGIN:VTIMEZONE"), strings.Index(got.Raw, "BEGIN:VEVENT"); tzIdx > evIdx {
		t.Errorf("VTIMEZONE must precede the VEVENT that references it:\n%s", got.Raw)
	}
}

// TestETagIsStableAcrossParses matters because ical_data is hashed into the
// ETag and the sync treats a changed ETag as a changed event. go-ical stores
// properties in a map, so anything that walks them unsorted would rewrite every
// event on every cycle.
func TestETagIsStableAcrossParses(t *testing.T) {
	payload := almatyFeed("+0500", "20260810T120000")

	first, err := ParseICS(payload)
	if err != nil {
		t.Fatalf("ParseICS: %v", err)
	}
	for i := 0; i < 8; i++ {
		again, err := ParseICS(payload)
		if err != nil {
			t.Fatalf("ParseICS: %v", err)
		}
		if again[0].ETag != first[0].ETag {
			t.Fatalf("ETag changed between identical parses: %s then %s", first[0].ETag, again[0].ETag)
		}
	}
}

// TestUnknownTZIDUsesTheFeedsDefinition covers Outlook and Exchange, which name
// zones in Windows terms no IANA database will resolve — and ship a VTIMEZONE
// for them, which is exactly what it is for. Before, LoadLocation failed, the
// error was dropped, and the event was stored at the Unix epoch.
func TestUnknownTZIDUsesTheFeedsDefinition(t *testing.T) {
	payload := ics(
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//test//test//EN",
		"BEGIN:VTIMEZONE",
		"TZID:Central Asia Standard Time",
		"BEGIN:STANDARD",
		"DTSTART:16010101T000000",
		"TZOFFSETFROM:+0600",
		"TZOFFSETTO:+0600",
		"END:STANDARD",
		"END:VTIMEZONE",
		"BEGIN:VEVENT",
		"UID:event-1",
		"DTSTAMP:20260801T000000Z",
		"SUMMARY:Discussion",
		"DTSTART;TZID=Central Asia Standard Time:20260810T120000",
		"END:VEVENT",
		"END:VCALENDAR",
	)

	got := mustParseOne(t, payload)

	want := time.Date(2026, 8, 10, 6, 0, 0, 0, time.UTC) // noon at UTC+06
	if !got.Start.Equal(want) {
		t.Errorf("DTSTART = %s, want %s", got.Start.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestUnknownTZIDWithoutDefinitionFallsBack is the last rung of the ladder: an
// unresolvable zone and no VTIMEZONE to explain it. The event is still kept —
// dropping a meeting is worse than an approximate time — but the reading has to
// come from somewhere explicit.
func TestUnknownTZIDWithoutDefinitionFallsBack(t *testing.T) {
	tz := newZoneResolver("")
	wall := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	loc := tz.locationFor("Neverland Standard Time", wall, time.UTC)
	if loc != time.UTC {
		t.Errorf("locationFor = %v, want the supplied fallback (UTC)", loc)
	}
}

// TestTruncatedVTimezoneLosesToHostRules is the case that keeps the previous
// test from being reckless. Generators often emit one fixed offset for a zone
// that really does observe DST; trusting that blindly would be wrong for half
// the year, so when our own database has seasonal rules for the zone, they win.
func TestTruncatedVTimezoneLosesToHostRules(t *testing.T) {
	const zone = "Europe/Berlin"
	if _, err := time.LoadLocation(zone); err != nil {
		t.Skipf("host has no %s in its zone database: %v", zone, err)
	}

	payload := ics(
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//test//test//EN",
		"BEGIN:VTIMEZONE",
		"TZID:"+zone,
		"BEGIN:STANDARD",
		"DTSTART:16010101T000000",
		"TZOFFSETFROM:+0100",
		"TZOFFSETTO:+0100", // truncated: no DAYLIGHT component at all
		"END:STANDARD",
		"END:VTIMEZONE",
		"BEGIN:VEVENT",
		"UID:event-1",
		"DTSTAMP:20260801T000000Z",
		"SUMMARY:Sommertermin",
		"DTSTART;TZID="+zone+":20260715T120000",
		"END:VEVENT",
		"END:VCALENDAR",
	)

	got := mustParseOne(t, payload)

	// July in Berlin is CEST, UTC+02 — not the +01 the block claims.
	want := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	if !got.Start.Equal(want) {
		t.Errorf("DTSTART = %s, want %s (host DST rules should beat a truncated block)",
			got.Start.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestFloatingTimeUsesFallback pins the third RFC 5545 form: no TZID and no Z.
// The spec reads such a value in the viewer's own zone, which for a subscribed
// feed is a guess, so it takes the fallback the caller supplied instead.
func TestFloatingTimeUsesFallback(t *testing.T) {
	tz := newZoneResolver("")

	got, err := tz.parseProp("20260810T120000", "", false, time.FixedZone("test", 4*3600))
	if err != nil {
		t.Fatalf("parseProp: %v", err)
	}

	want := time.Date(2026, 8, 10, 8, 0, 0, 0, time.UTC)
	if !got.UTC().Equal(want) {
		t.Errorf("floating time = %s, want %s", got.UTC().Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestUTCAndAllDayValues covers the two remaining value forms.
func TestUTCAndAllDayValues(t *testing.T) {
	tz := newZoneResolver("")

	utcValue, err := tz.parseProp("20260810T120000Z", "", false, time.FixedZone("ignored", 9*3600))
	if err != nil {
		t.Fatalf("parseProp(Z): %v", err)
	}
	if want := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC); !utcValue.UTC().Equal(want) {
		t.Errorf("Z-qualified value = %s, want %s", utcValue.UTC().Format(time.RFC3339), want.Format(time.RFC3339))
	}

	date, err := tz.parseProp("20260810", "", true, time.FixedZone("ignored", 9*3600))
	if err != nil {
		t.Fatalf("parseProp(DATE): %v", err)
	}
	if want := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC); !date.UTC().Equal(want) {
		t.Errorf("all-day value = %s, want %s", date.UTC().Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestEventWithoutUsableDTStartIsDropped: an event with no start has nothing to
// show and nothing to remind about. It used to be stored with DTStart zero,
// which put it in January 1970 and left it there.
func TestEventWithoutUsableDTStartIsDropped(t *testing.T) {
	payload := ics(
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//test//test//EN",
		"BEGIN:VEVENT",
		"UID:no-start",
		"SUMMARY:Malformed",
		"DTSTART;TZID=Asia/Almaty:not-a-timestamp",
		"END:VEVENT",
		"END:VCALENDAR",
	)

	events, err := ParseICS(payload)
	if err != nil {
		t.Fatalf("ParseICS: %v", err)
	}
	for _, ev := range events {
		if ev.DTStart == 0 {
			t.Errorf("event %q was stored with a zero DTStart instead of being dropped", ev.UID)
		}
	}
}

// TestEventWithNoDTStartAtAllIsKept separates "unreadable" from "absent".
// ParseICS also handles invitations arriving by mail, where a METHOD:CANCEL
// carries a UID and nothing to schedule; rejecting those would quietly stop
// cancellations from being processed.
func TestEventWithNoDTStartAtAllIsKept(t *testing.T) {
	payload := ics(
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//test//test//EN",
		"METHOD:CANCEL",
		"BEGIN:VEVENT",
		"UID:cancelled-event",
		"DTSTAMP:20260801T000000Z",
		"SUMMARY:Cancelled",
		"SEQUENCE:2",
		"END:VEVENT",
		"END:VCALENDAR",
	)

	events, err := ParseICS(payload)
	if err != nil {
		t.Fatalf("ParseICS: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected the cancellation to survive parsing, got %d events", len(events))
	}
	if events[0].UID != "cancelled-event" {
		t.Errorf("UID = %q, want %q", events[0].UID, "cancelled-event")
	}
}

// TestTextFallbackSharesZoneSemantics checks the claim the two parsers make
// about each other: whichever one handles a payload, a TZID means the same
// thing. The text path used to read a bare local time as UTC.
func TestTextFallbackSharesZoneSemantics(t *testing.T) {
	payload := almatyFeed("+0500", "20260810T120000")
	tz := newZoneResolver(payload)

	vevents := extractVEvents(payload)
	if len(vevents) != 1 {
		t.Fatalf("expected 1 VEVENT, got %d", len(vevents))
	}

	ev := parseVEvent(vevents[0], 0, tz, time.UTC)
	if ev == nil {
		t.Fatal("parseVEvent returned nil")
	}

	want := time.Date(2026, 8, 10, 7, 0, 0, 0, time.UTC)
	if got := time.UnixMilli(ev.DTStart).UTC(); !got.Equal(want) {
		t.Errorf("text fallback DTSTART = %s, want %s (must match the decoder path)",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestParseUTCOffset(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "+0500", want: 5 * 3600},
		{in: "-0500", want: -5 * 3600},
		{in: "+0000", want: 0},
		{in: "+0530", want: 5*3600 + 30*60},
		{in: "-0330", want: -(3*3600 + 30*60)},
		{in: "+050030", want: 5*3600 + 30},
		{in: "0500", wantErr: true},
		{in: "", wantErr: true},
		{in: "+05:00", wantErr: true},
		{in: "+abcd", wantErr: true},
	}

	for _, c := range cases {
		got, err := parseUTCOffset(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseUTCOffset(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseUTCOffset(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseUTCOffset(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestICalParam(t *testing.T) {
	cases := []struct {
		line, name, want string
	}{
		{line: "DTSTART;TZID=Asia/Almaty:20260810T120000", name: "TZID", want: "Asia/Almaty"},
		{line: `DTSTART;TZID="Asia/Almaty":20260810T120000`, name: "TZID", want: "Asia/Almaty"},
		{line: "DTSTART;VALUE=DATE:20260810", name: "VALUE", want: "DATE"},
		{line: "DTSTART;VALUE=DATE-TIME;TZID=UTC:20260810T120000", name: "TZID", want: "UTC"},
		{line: "DTSTART:20260810T120000Z", name: "TZID", want: ""},
		{line: "DTSTART;TZID=Central Asia Standard Time:20260810T120000", name: "TZID", want: "Central Asia Standard Time"},
	}

	for _, c := range cases {
		if got := icalParam(c.line, c.name); got != c.want {
			t.Errorf("icalParam(%q, %q) = %q, want %q", c.line, c.name, got, c.want)
		}
	}
}
