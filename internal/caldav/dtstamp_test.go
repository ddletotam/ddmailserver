package caldav

import (
	"strings"
	"testing"
)

// stampMs is a fixed instant so the expected DTSTAMP is spellable:
// 2026-08-11T03:05:10Z.
const stampMs = int64(1786417510000)

const wantStamp = "DTSTAMP:20260811T030510Z"

// TestEnsureDTSTAMP_RepairsTheRejectedPayload is the regression, built from the
// body that sat in the reverse-sync queue for three days. iCloud answered 403
// every time: RFC 4791 §5.3.2.1 turns an invalid calendar object into a failed
// precondition, and DTSTAMP is REQUIRED by RFC 5545.
func TestEnsureDTSTAMP_RepairsTheRejectedPayload(t *testing.T) {
	payload := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//DDMailServer//Calendar//EN",
		"BEGIN:VEVENT",
		"UID:547e634e9cddbb1b97a2b971988c08e5@ddmailserver",
		"SUMMARY:На приём",
		"DTSTART:20260814T103000Z",
		"DTEND:20260814T113000Z",
		"END:VEVENT",
		"END:VCALENDAR",
		"",
	}, "\r\n")

	got := EnsureDTSTAMP(payload, stampMs)

	if !strings.Contains(got, wantStamp) {
		t.Fatalf("DTSTAMP was not added:\n%s", got)
	}
	// It has to land inside the VEVENT, not in the VCALENDAR envelope.
	if idx, evIdx := strings.Index(got, wantStamp), strings.Index(got, "BEGIN:VEVENT"); idx < evIdx {
		t.Errorf("DTSTAMP landed outside the VEVENT:\n%s", got)
	}
	if !strings.Contains(got, "\r\n") {
		t.Errorf("CRLF line endings were lost:\n%q", got)
	}
	if strings.Contains(got, "\r\r") {
		t.Errorf("line endings were doubled:\n%q", got)
	}
}

// TestEnsureDTSTAMP_LeavesExistingStampAlone: an object that already carries a
// DTSTAMP must come back byte for byte, or we would be rewriting other people's
// events on their way out.
func TestEnsureDTSTAMP_LeavesExistingStampAlone(t *testing.T) {
	payload := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"BEGIN:VEVENT",
		"UID:already-stamped",
		"DTSTAMP:20200101T000000Z",
		"DTSTART:20260814T103000Z",
		"END:VEVENT",
		"END:VCALENDAR",
		"",
	}, "\r\n")

	if got := EnsureDTSTAMP(payload, stampMs); got != payload {
		t.Errorf("payload was modified:\nbefore: %q\nafter:  %q", payload, got)
	}
}

// TestEnsureDTSTAMP_HandlesSeveralEvents: one calendar object may hold a master
// and its overrides, and each VEVENT needs its own stamp.
func TestEnsureDTSTAMP_HandlesSeveralEvents(t *testing.T) {
	payload := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"BEGIN:VEVENT",
		"UID:one",
		"DTSTART:20260814T103000Z",
		"END:VEVENT",
		"BEGIN:VEVENT",
		"UID:two",
		"DTSTAMP:20200101T000000Z",
		"DTSTART:20260815T103000Z",
		"END:VEVENT",
		"BEGIN:VEVENT",
		"UID:three",
		"DTSTART:20260816T103000Z",
		"END:VEVENT",
		"END:VCALENDAR",
		"",
	}, "\r\n")

	got := EnsureDTSTAMP(payload, stampMs)

	if n := strings.Count(got, "DTSTAMP:"); n != 3 {
		t.Errorf("found %d DTSTAMP properties, want one per VEVENT (3):\n%s", n, got)
	}
	if !strings.Contains(got, "DTSTAMP:20200101T000000Z") {
		t.Error("the event that already had a stamp lost it")
	}
	if n := strings.Count(got, wantStamp); n != 2 {
		t.Errorf("added %d stamps, want 2 (only the events that lacked one)", n)
	}
}

// TestEnsureDTSTAMP_PassesThroughNonEvents: VTODO, VTIMEZONE and anything else
// is none of this function's business.
func TestEnsureDTSTAMP_PassesThroughNonEvents(t *testing.T) {
	payload := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"BEGIN:VTIMEZONE",
		"TZID:Asia/Almaty",
		"END:VTIMEZONE",
		"END:VCALENDAR",
		"",
	}, "\r\n")

	if got := EnsureDTSTAMP(payload, stampMs); got != payload {
		t.Errorf("a payload with no VEVENT was modified:\n%q", got)
	}

	if got := EnsureDTSTAMP("", stampMs); got != "" {
		t.Errorf("empty input produced %q", got)
	}
}

// TestEnsureDTSTAMP_KeepsLFPayloadsAsLF: the repair must not smuggle a stray CR
// into a payload that was written with bare newlines.
func TestEnsureDTSTAMP_KeepsLFPayloadsAsLF(t *testing.T) {
	payload := "BEGIN:VCALENDAR\nBEGIN:VEVENT\nUID:lf-only\nDTSTART:20260814T103000Z\nEND:VEVENT\nEND:VCALENDAR\n"

	got := EnsureDTSTAMP(payload, stampMs)

	if strings.Contains(got, "\r") {
		t.Errorf("a CR was introduced into an LF-only payload:\n%q", got)
	}
	if !strings.Contains(got, wantStamp) {
		t.Errorf("DTSTAMP was not added:\n%q", got)
	}
}
