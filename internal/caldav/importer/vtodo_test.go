package importer

import (
	"strings"
	"testing"

	"github.com/yourusername/mailserver/internal/models"
)

// iosReminder is the shape iOS Reminders actually PUT at us — the payload that
// sat in the reverse-sync queue failing against iCloud six times over. Note
// what it does NOT have: DTEND, LOCATION, ORGANIZER, and any status beyond
// NEEDS-ACTION.
const iosReminder = `BEGIN:VCALENDAR
CALSCALE:GREGORIAN
PRODID:-//Apple Inc.//iOS 26.6//EN
VERSION:2.0
BEGIN:VTODO
CREATED:20260825T143426Z
DTSTAMP:20260825T143443Z
DTSTART;VALUE=DATE:20260919
DUE;VALUE=DATE:20260919
LAST-MODIFIED:20260825T143442Z
RRULE:FREQ=MONTHLY
STATUS:NEEDS-ACTION
SUMMARY:Оплата офиса
UID:FCC55C22-6C52-432C-8684-D82A49F77590
END:VTODO
END:VCALENDAR
`

func TestParseIOSReminder(t *testing.T) {
	events, err := ParseICS(iosReminder)
	if err != nil {
		t.Fatalf("ParseICS: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d components, want 1", len(events))
	}

	todo := events[0]
	if !todo.IsTodo() {
		t.Errorf("component = %q, want VTODO", todo.Component)
	}
	// Before tasks were understood this landed with an empty summary, which is
	// why the stuck-operations report showed "(без названия)".
	if todo.Summary != "Оплата офиса" {
		t.Errorf("summary = %q", todo.Summary)
	}
	if todo.UID != "FCC55C22-6C52-432C-8684-D82A49F77590" {
		t.Errorf("uid = %q", todo.UID)
	}
	if todo.Due == nil {
		t.Fatal("DUE was not parsed")
	}
	if !todo.AllDay {
		t.Error("a DATE-valued DUE should mark the task all-day")
	}
	if todo.Status != "NEEDS-ACTION" {
		t.Errorf("status = %q, want NEEDS-ACTION", todo.Status)
	}
	if todo.IsCompleted() {
		t.Error("task should not read as completed")
	}
	if todo.RRule != "FREQ=MONTHLY" {
		t.Errorf("rrule = %q", todo.RRule)
	}
}

// TestParseTodoCompletionFields covers the properties that only appear once a
// task has been worked on.
func TestParseTodoCompletionFields(t *testing.T) {
	data := `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Test//EN
BEGIN:VTODO
UID:done-1
DTSTAMP:20260101T120000Z
SUMMARY:Сдать отчёт
STATUS:COMPLETED
COMPLETED:20260110T153000Z
PERCENT-COMPLETE:100
PRIORITY:1
END:VTODO
END:VCALENDAR
`

	events, err := ParseICS(data)
	if err != nil {
		t.Fatalf("ParseICS: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d components, want 1", len(events))
	}

	todo := events[0]
	if todo.CompletedAt == nil {
		t.Fatal("COMPLETED was not parsed")
	}
	if !todo.IsCompleted() {
		t.Error("task should read as completed")
	}
	if todo.PercentComplete == nil || *todo.PercentComplete != 100 {
		t.Errorf("percent complete = %v", todo.PercentComplete)
	}
	if todo.Priority == nil || *todo.Priority != 1 {
		t.Errorf("priority = %v", todo.Priority)
	}
	if todo.Due != nil {
		t.Error("no DUE was given, so none should be set")
	}
}

// TestParseTodoWithoutDTSTART: a task need not be scheduled at all. An event
// without DTSTART is suspect; a task without one is the common case, and
// rejecting it would drop most of a reminders list on the floor.
func TestParseTodoWithoutDTSTART(t *testing.T) {
	data := `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Test//EN
BEGIN:VTODO
UID:floating-1
DTSTAMP:20260101T120000Z
SUMMARY:Купить молоко
END:VTODO
END:VCALENDAR
`

	events, err := ParseICS(data)
	if err != nil {
		t.Fatalf("ParseICS: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d components, want 1 — a task with no DTSTART must survive", len(events))
	}
	if events[0].Summary != "Купить молоко" {
		t.Errorf("summary = %q", events[0].Summary)
	}
	// Absent STATUS on a task means NEEDS-ACTION, not the CONFIRMED that
	// events default to.
	if events[0].Status != "NEEDS-ACTION" {
		t.Errorf("status = %q, want NEEDS-ACTION", events[0].Status)
	}
}

// TestParseMixedComponents: one calendar carrying both kinds must yield both,
// each labelled correctly.
func TestParseMixedComponents(t *testing.T) {
	data := `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Test//EN
BEGIN:VEVENT
UID:evt-1
DTSTAMP:20260101T120000Z
SUMMARY:Совещание
DTSTART:20260110T090000Z
DTEND:20260110T100000Z
END:VEVENT
BEGIN:VTODO
UID:todo-1
DTSTAMP:20260101T120000Z
SUMMARY:Подготовить слайды
DUE:20260109T180000Z
END:VTODO
END:VCALENDAR
`

	events, err := ParseICS(data)
	if err != nil {
		t.Fatalf("ParseICS: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d components, want 2", len(events))
	}

	byUID := map[string]*models.CalendarEvent{}
	for _, e := range events {
		byUID[e.UID] = e
	}

	evt := byUID["evt-1"]
	if evt == nil || evt.IsTodo() {
		t.Errorf("evt-1 should be an event, got %+v", evt)
	}
	if evt != nil && evt.DTEnd == nil {
		t.Error("event lost its DTEND")
	}

	todo := byUID["todo-1"]
	if todo == nil || !todo.IsTodo() {
		t.Errorf("todo-1 should be a task, got %+v", todo)
	}
	if todo != nil && todo.Due == nil {
		t.Error("task lost its DUE")
	}
}

// TestParsedTodoRoundTripsBody: the stored ical_data must still be a VTODO.
// It is what gets pushed back upstream, and re-encoding it as a VEVENT would
// turn a task into a bogus event on the remote.
func TestParsedTodoRoundTripsBody(t *testing.T) {
	events, err := ParseICS(iosReminder)
	if err != nil {
		t.Fatalf("ParseICS: %v", err)
	}

	body := events[0].ICalData
	if !strings.Contains(body, "BEGIN:VTODO") {
		t.Errorf("stored body is not a VTODO:\n%s", body)
	}
	if strings.Contains(body, "BEGIN:VEVENT") {
		t.Errorf("stored body was re-encoded as an event:\n%s", body)
	}
}
