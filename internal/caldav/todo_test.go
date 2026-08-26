package caldav

import (
	"strings"
	"testing"
)

const reminderBody = "BEGIN:VCALENDAR\r\n" +
	"CALSCALE:GREGORIAN\r\n" +
	"PRODID:-//Apple Inc.//iOS 26.6//EN\r\n" +
	"VERSION:2.0\r\n" +
	"BEGIN:VTODO\r\n" +
	"CREATED:20260825T143426Z\r\n" +
	"DTSTAMP:20260825T143443Z\r\n" +
	"DUE;VALUE=DATE:20260919\r\n" +
	"STATUS:NEEDS-ACTION\r\n" +
	"SUMMARY:Оплата офиса\r\n" +
	"UID:FCC55C22-6C52-432C-8684-D82A49F77590\r\n" +
	"X-APPLE-SORT-ORDER:12\r\n" +
	"END:VTODO\r\n" +
	"END:VCALENDAR\r\n"

func TestSetTodoCompletionMarksDone(t *testing.T) {
	out := SetTodoCompletion(reminderBody, true, 1767225600000) // 2026-01-01T00:00:00Z

	if !strings.Contains(out, "STATUS:COMPLETED") {
		t.Errorf("STATUS was not set:\n%s", out)
	}
	if strings.Contains(out, "STATUS:NEEDS-ACTION") {
		t.Error("the old STATUS survived — clients would see two")
	}
	if !strings.Contains(out, "COMPLETED:20260101T000000Z") {
		t.Errorf("COMPLETED timestamp missing:\n%s", out)
	}
	if !strings.Contains(out, "PERCENT-COMPLETE:100") {
		t.Error("PERCENT-COMPLETE missing — some clients read only this")
	}

	// Unmodelled properties must survive: re-encoding through a parser would
	// drop them, which is why this is line-level surgery.
	if !strings.Contains(out, "X-APPLE-SORT-ORDER:12") {
		t.Error("a vendor property was stripped")
	}
	if !strings.Contains(out, "SUMMARY:Оплата офиса") || !strings.Contains(out, "DUE;VALUE=DATE:20260919") {
		t.Error("task content was damaged")
	}
	if !strings.Contains(out, "\r\n") {
		t.Error("CRLF line endings were not preserved")
	}
}

func TestSetTodoCompletionMarksUndone(t *testing.T) {
	done := SetTodoCompletion(reminderBody, true, 1767225600000)
	out := SetTodoCompletion(done, false, 1767225600000)

	if !strings.Contains(out, "STATUS:NEEDS-ACTION") {
		t.Errorf("STATUS was not reset:\n%s", out)
	}
	if strings.Contains(out, "COMPLETED:") {
		t.Error("COMPLETED must be removed when un-ticking, not left behind")
	}
	if strings.Contains(out, "PERCENT-COMPLETE") {
		t.Error("PERCENT-COMPLETE must be removed when un-ticking")
	}
	if !strings.Contains(out, "X-APPLE-SORT-ORDER:12") {
		t.Error("a vendor property was stripped on the round trip")
	}
}

// TestSetTodoCompletionExactlyOnce guards the property count across repeated
// toggles: appending instead of replacing would accumulate a STATUS per click.
func TestSetTodoCompletionExactlyOnce(t *testing.T) {
	out := reminderBody
	for i := 0; i < 5; i++ {
		out = SetTodoCompletion(out, i%2 == 0, 1767225600000)
	}

	if n := strings.Count(out, "STATUS:"); n != 1 {
		t.Errorf("got %d STATUS lines after five toggles, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, "BEGIN:VTODO"); n != 1 {
		t.Errorf("got %d VTODO blocks, want 1", n)
	}
}

// TestSetTodoCompletionIgnoresEvents: the helper must be a no-op on a VEVENT,
// so a mis-routed call cannot quietly stamp COMPLETED onto a meeting.
func TestSetTodoCompletionIgnoresEvents(t *testing.T) {
	event := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:x\r\nSTATUS:CONFIRMED\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

	if out := SetTodoCompletion(event, true, 1767225600000); out != event {
		t.Errorf("a VEVENT was modified:\n%s", out)
	}
	if out := SetTodoCompletion("", true, 1767225600000); out != "" {
		t.Error("empty input should stay empty")
	}
}

// TestSetTodoCompletionWithoutExistingStatus: a task that never declared one
// still has to come back with exactly one.
func TestSetTodoCompletionWithoutExistingStatus(t *testing.T) {
	bare := "BEGIN:VCALENDAR\nBEGIN:VTODO\nUID:bare\nSUMMARY:Купить молоко\nEND:VTODO\nEND:VCALENDAR\n"

	out := SetTodoCompletion(bare, true, 1767225600000)
	if n := strings.Count(out, "STATUS:COMPLETED"); n != 1 {
		t.Errorf("got %d STATUS lines, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, "SUMMARY:Купить молоко") {
		t.Error("content was lost")
	}
	// LF input must not acquire CR.
	if strings.Contains(out, "\r") {
		t.Error("CR was introduced into an LF payload")
	}
}
