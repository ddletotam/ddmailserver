package caldav

import (
	"strings"
	"time"
)

// EnsureDTSTAMP adds a DTSTAMP to every VEVENT that does not already carry one.
//
// DTSTAMP is REQUIRED by RFC 5545 §3.6.1, and a CalDAV server is entitled to
// refuse a calendar object without it: RFC 4791 §5.3.2.1 makes an invalid
// object a failed precondition, and a failed precondition is answered with 403
// Forbidden. iCloud does exactly that, which is why an event generated on our
// side could be read back from Apple all day and never be pushed to it — the
// reverse sync retried the same rejected body for days.
//
// This runs where the object is sent rather than only where it is generated, so
// that payloads already queued are repaired too, whoever wrote them. The
// generators were fixed as well; this is the backstop.
//
// stampMs is passed in rather than read from the clock so the result is
// testable.
func EnsureDTSTAMP(icalData string, stampMs int64) string {
	if icalData == "" || !strings.Contains(icalData, "BEGIN:VEVENT") {
		return icalData
	}

	stamp := time.UnixMilli(stampMs).UTC().Format("20060102T150405Z")

	lines := strings.Split(icalData, "\n")
	out := make([]string, 0, len(lines)+2)

	var block []string
	inEvent := false
	hasStamp := false

	// flush emits the buffered VEVENT, inserting DTSTAMP right after
	// BEGIN:VEVENT when the block had none.
	flush := func() {
		if len(block) == 0 {
			return
		}
		if !hasStamp {
			// Match the line ending the block already uses, so a CRLF payload
			// stays CRLF and an LF one stays LF.
			terminator := ""
			if strings.HasSuffix(block[0], "\r") {
				terminator = "\r"
			}
			repaired := make([]string, 0, len(block)+1)
			repaired = append(repaired, block[0])
			repaired = append(repaired, "DTSTAMP:"+stamp+terminator)
			repaired = append(repaired, block[1:]...)
			block = repaired
		}
		out = append(out, block...)
		block = nil
	}

	for _, line := range lines {
		bare := strings.TrimRight(line, "\r")

		switch {
		case strings.HasPrefix(bare, "BEGIN:VEVENT"):
			inEvent, hasStamp = true, false
			block = append(block, line)
			continue
		case strings.HasPrefix(bare, "END:VEVENT"):
			block = append(block, line)
			flush()
			inEvent = false
			continue
		}

		if inEvent {
			if strings.HasPrefix(bare, "DTSTAMP") {
				hasStamp = true
			}
			block = append(block, line)
			continue
		}

		out = append(out, line)
	}

	// An unterminated VEVENT is malformed, but dropping it would be worse than
	// passing it along as-is.
	flush()

	return strings.Join(out, "\n")
}
