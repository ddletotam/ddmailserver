package caldav

import "strings"

// maxOctets is the content-line limit from RFC 5545 §3.1: a line "SHOULD NOT be
// longer than 75 octets, excluding the line break".
const maxOctets = 75

// EscapeText escapes a TEXT property value per RFC 5545 §3.3.11.
//
// Only TEXT values may be escaped. Structured values must not be: RRULE spells
// its own syntax with the very characters escaped here, so running it through
// this would corrupt the rule.
//
// The importer already reverses this (see the prop.Text() call in
// importer/ics.go, and the "массаж\, Маргарита" note beside it) — the writers
// were the half that never did it. An unescaped line break is the expensive
// one: it terminates the content line early, the remainder parses as a bogus
// property, and the whole object becomes invalid. A CalDAV server may then
// refuse it under RFC 4791 §5.3.2.1, which is answered with 403.
func EscapeText(s string) string {
	// Fast path: most values carry nothing that needs escaping.
	if !strings.ContainsAny(s, "\\;,\r\n") {
		return s
	}

	var b strings.Builder
	b.Grow(len(s) + 16)

	// Byte-wise is safe: every character handled here is ASCII, and UTF-8
	// continuation bytes are all >= 0x80.
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case ';':
			b.WriteString(`\;`)
		case ',':
			b.WriteString(`\,`)
		case '\r':
			// A CRLF pair and a bare CR both mean one line break.
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
			b.WriteString(`\n`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}

	return b.String()
}

// FoldLine breaks a content line at the RFC 5545 §3.1 octet limit, continuing
// each fragment with CRLF and a single leading space.
//
// The limit is octets, not characters, which is what makes this matter for
// Cyrillic: a description that looks comfortably short is already over the
// limit at two bytes per letter. Fragments never split a UTF-8 sequence.
func FoldLine(line string) string {
	if len(line) <= maxOctets {
		return line
	}

	var b strings.Builder
	b.Grow(len(line) + 2*(len(line)/maxOctets+1))

	start := 0
	limit := maxOctets
	for {
		if len(line)-start <= limit {
			b.WriteString(line[start:])
			return b.String()
		}

		end := start + limit
		// Back off the cut until it lands on a rune boundary.
		for end > start && line[end]&0xC0 == 0x80 {
			end--
		}
		if end == start {
			// A single rune longer than the remaining budget: emit it whole
			// rather than loop forever.
			end = start + limit
			for end < len(line) && line[end]&0xC0 == 0x80 {
				end++
			}
		}

		b.WriteString(line[start:end])
		b.WriteString("\r\n ")
		start = end
		// The continuation's leading space counts against its own budget.
		limit = maxOctets - 1
	}
}

// ContentLine renders one TEXT property as a complete, escaped and folded
// content line, line break included.
func ContentLine(name, value string) string {
	return FoldLine(name+":"+EscapeText(value)) + "\r\n"
}

// RawContentLine renders a property whose value must not be escaped — RRULE and
// other structured values — folding it all the same.
func RawContentLine(name, value string) string {
	return FoldLine(name+":"+value) + "\r\n"
}
