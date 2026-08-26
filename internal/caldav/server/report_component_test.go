package server

import (
	"strings"
	"testing"

	"github.com/yourusername/mailserver/internal/models"
)

// TestParseReportComponentFilter covers the filter Apple relies on. Calendar
// and Reminders point at the same collection and are told apart by nothing but
// this comp-filter, so a miss hands each of them the other's contents.
//
// The nesting matters: the outer filter is always VCALENDAR, and naively taking
// the first `name=` would make every request look like a VCALENDAR query.
func TestParseReportComponentFilter(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "Reminders asking for tasks",
			body: `<?xml version="1.0" encoding="UTF-8"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><D:getetag/><C:calendar-data/></D:prop>
  <C:filter>
    <C:comp-filter name="VCALENDAR">
      <C:comp-filter name="VTODO"/>
    </C:comp-filter>
  </C:filter>
</C:calendar-query>`,
			want: models.ComponentTodo,
		},
		{
			name: "Calendar asking for events",
			body: `<?xml version="1.0" encoding="UTF-8"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <C:filter>
    <C:comp-filter name="VCALENDAR">
      <C:comp-filter name="VEVENT">
        <C:time-range start="20260101T000000Z" end="20260201T000000Z"/>
      </C:comp-filter>
    </C:comp-filter>
  </C:filter>
</C:calendar-query>`,
			want: models.ComponentEvent,
		},
		{
			name: "unprefixed element names",
			body: `<calendar-query xmlns="urn:ietf:params:xml:ns:caldav">
  <filter><comp-filter name="VCALENDAR"><comp-filter name="VTODO"/></comp-filter></filter>
</calendar-query>`,
			want: models.ComponentTodo,
		},
		{
			name: "no filter at all",
			body: `<C:calendar-query xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop xmlns:D="DAV:"><D:getetag/></D:prop>
</C:calendar-query>`,
			want: "",
		},
		{
			name: "multiget carries no comp-filter",
			body: `<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:href>/caldav/calendars/u/1/abc.ics</D:href>
</C:calendar-multiget>`,
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseReportBody([]byte(c.body)).component
			if got != c.want {
				t.Errorf("component = %q, want %q", got, c.want)
			}
		})
	}
}

// TestParseReportKeepsTimeRangeWithComponent: the two filters arrive nested in
// the same document, and parsing one must not consume the other.
func TestParseReportKeepsTimeRangeWithComponent(t *testing.T) {
	body := `<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <C:filter>
    <C:comp-filter name="VCALENDAR">
      <C:comp-filter name="VEVENT">
        <C:time-range start="20260101T000000Z" end="20260201T000000Z"/>
      </C:comp-filter>
    </C:comp-filter>
  </C:filter>
</C:calendar-query>`

	req := parseReportBody([]byte(body))
	if req.component != models.ComponentEvent {
		t.Errorf("component = %q", req.component)
	}
	if req.timeRange == nil {
		t.Fatal("time-range was lost")
	}
	if req.timeRange.start.IsZero() || req.timeRange.end.IsZero() {
		t.Errorf("time-range = %v..%v", req.timeRange.start, req.timeRange.end)
	}
}

// TestSupportedComponentSetXML pins the property a client is entitled to
// believe. Advertising VTODO on a collection that cannot hold one is the exact
// mistake that started this: iOS filed its Reminders here and every push
// upstream then failed with 403.
func TestSupportedComponentSetXML(t *testing.T) {
	cases := []struct {
		declared string
		wantHas  []string
		wantNot  []string
	}{
		{declared: "", wantHas: []string{`"VEVENT"`}, wantNot: []string{"VTODO"}},
		{declared: "VEVENT", wantHas: []string{`"VEVENT"`}, wantNot: []string{"VTODO"}},
		{declared: "VTODO", wantHas: []string{`"VTODO"`}, wantNot: []string{"VEVENT"}},
		{declared: "VEVENT,VTODO", wantHas: []string{`"VEVENT"`, `"VTODO"`}},
		{declared: " vevent , vtodo ", wantHas: []string{`"VEVENT"`, `"VTODO"`}},
	}

	for _, c := range cases {
		cal := &models.Calendar{SupportedComponents: c.declared}
		out := supportedComponentSetXML(cal)

		for _, want := range c.wantHas {
			if !strings.Contains(out, want) {
				t.Errorf("declared %q: missing %s in\n%s", c.declared, want, out)
			}
		}
		for _, unwanted := range c.wantNot {
			if strings.Contains(out, unwanted) {
				t.Errorf("declared %q: unexpectedly advertises %s", c.declared, unwanted)
			}
		}
		if !strings.Contains(out, "<C:supported-calendar-component-set>") ||
			!strings.Contains(out, "</C:supported-calendar-component-set>") {
			t.Errorf("declared %q: element is not well formed:\n%s", c.declared, out)
		}
	}
}

// TestCalendarSupports is the guard the PUT handler leans on.
func TestCalendarSupports(t *testing.T) {
	eventsOnly := &models.Calendar{SupportedComponents: "VEVENT"}
	if !eventsOnly.Supports(models.ComponentEvent) {
		t.Error("an event calendar should accept events")
	}
	if eventsOnly.Supports(models.ComponentTodo) {
		t.Error("an event calendar must refuse tasks — this is the 403 we were causing")
	}

	// Empty means events only: every calendar that existed before tasks did.
	legacy := &models.Calendar{}
	if !legacy.Supports(models.ComponentEvent) || legacy.Supports(models.ComponentTodo) {
		t.Error("an unset collection should read as events only")
	}

	both := &models.Calendar{SupportedComponents: "VEVENT,VTODO"}
	if !both.Supports(models.ComponentEvent) || !both.Supports(models.ComponentTodo) {
		t.Error("a mixed collection should accept both")
	}
}
