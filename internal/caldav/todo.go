package caldav

import (
	"strings"
	"time"
)

// SetTodoCompletion rewrites a VTODO body to mark the task done or not done.
//
// Line-level surgery rather than decode-and-re-encode, for the same reason the
// rest of this package works that way: re-encoding drops every property we do
// not model, and a task synced from iOS Reminders carries a fair number of
// those (X-APPLE-SORT-ORDER, custom alarms, geofences). The user ticking a
// checkbox should not silently strip them.
//
// Three properties move together, because clients disagree about which one to
// read: STATUS is the RFC 5545 answer, COMPLETED is the timestamp Apple shows,
// and PERCENT-COMPLETE is what some Android clients key off. Setting only one
// leaves a task that looks done in one app and outstanding in another.
func SetTodoCompletion(icalData string, completed bool, nowMs int64) string {
	if icalData == "" || !strings.Contains(icalData, "BEGIN:VTODO") {
		return icalData
	}

	stamp := time.UnixMilli(nowMs).UTC().Format("20060102T150405Z")

	lines := strings.Split(icalData, "\n")
	out := make([]string, 0, len(lines)+3)

	inTodo := false
	// Whether the line just emitted was BEGIN:VTODO, so the new properties can
	// be inserted at the top of the block rather than appended after END.
	justOpened := false

	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		terminator := ""
		if strings.HasSuffix(line, "\r") {
			terminator = "\r"
		}

		upper := strings.ToUpper(trimmed)

		switch {
		case strings.HasPrefix(upper, "BEGIN:VTODO"):
			inTodo = true
			out = append(out, line)
			justOpened = true
			continue

		case strings.HasPrefix(upper, "END:VTODO"):
			inTodo = false
			out = append(out, line)
			continue
		}

		// Insert the completion properties immediately after BEGIN:VTODO. The
		// old ones are dropped below as they come past, so there is exactly one
		// of each afterwards regardless of what the source contained.
		if justOpened {
			out = append(out, "STATUS:"+todoStatus(completed)+terminator)
			if completed {
				out = append(out, "COMPLETED:"+stamp+terminator)
				out = append(out, "PERCENT-COMPLETE:100"+terminator)
			}
			justOpened = false
		}

		if inTodo && isCompletionProp(upper) {
			continue
		}

		out = append(out, line)
	}

	return strings.Join(out, "\n")
}

// isCompletionProp reports whether a content line is one of the three the
// completion state owns. The property name is everything before the first ':'
// or ';' — a parameter list must not make STATUS look like something else.
func isCompletionProp(upperLine string) bool {
	name := upperLine
	if i := strings.IndexAny(name, ":;"); i != -1 {
		name = name[:i]
	}
	switch strings.TrimSpace(name) {
	case "STATUS", "COMPLETED", "PERCENT-COMPLETE":
		return true
	}
	return false
}

func todoStatus(completed bool) string {
	if completed {
		return "COMPLETED"
	}
	return "NEEDS-ACTION"
}
