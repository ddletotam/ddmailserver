package importer

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// iCalendar date and date-time layouts (RFC 5545 §3.3.4, §3.3.5).
const (
	icalDate        = "20060102"
	icalDateTime    = "20060102T150405"
	icalDateTimeUTC = "20060102T150405Z"
)

// declaredZone is what a feed's own VTIMEZONE component said about a zone.
type declaredZone struct {
	// loc renders the block as one fixed offset. Set only when every
	// TZOFFSETTO in the block agreed — which is the common case, because
	// generators tend to emit a single STANDARD component and no rules at all.
	loc *time.Location

	// standard is the STANDARD offset on its own. Kept as a last resort for a
	// block that does declare transitions, whose TZID we cannot resolve any
	// other way.
	standard *time.Location
}

// zoneResolver turns an iCalendar TZID reference into a *time.Location.
//
// A TZID is a *name*, not an offset: `DTSTART;TZID=Asia/Almaty:20260810T120000`
// says "noon in the zone called Asia/Almaty" and carries no offset whatsoever.
// Two sources can map that name onto one, and they disagree more often than is
// comfortable:
//
//   - The host's IANA database, via time.LoadLocation. It has the full history
//     and the future rules, but it is only as fresh as the operating system
//     package. A Debian 12 box still carrying tzdata 2023c believes Asia/Almaty
//     is UTC+06 — Kazakhstan moved the entire country to UTC+05 on 2024-03-01,
//     which landed in tzdata 2024a.
//   - The VTIMEZONE component the feed ships next to its events. It is
//     authoritative about what the sender meant and immune to our host being
//     behind, but generators routinely truncate it to a single fixed offset
//     with no transition rules at all.
//
// So we consult both and compare them at the instant we actually care about.
// Agreement is the normal case and costs nothing. A disagreement means one of
// the two is wrong about this zone, and which one is recoverable: if our own
// database says the zone shifts with the seasons, then the feed's lone fixed
// offset is a truncation and the host rules win; if our database says the zone
// holds one offset all year, then the sender's declaration is the better
// authority and ours is probably stale. Either way it gets logged, because an
// hour of silent skew is the kind of defect that only surfaces when somebody
// misses a meeting.
type zoneResolver struct {
	// blocks holds the raw VTIMEZONE text per TZID, verbatim, so the
	// definitions can be written back out with the event (see withVTimezones).
	blocks map[string]string

	// declared holds what those blocks say, parsed.
	declared map[string]declaredZone

	// warned tracks what has already been reported, so one odd feed logs once
	// per sync instead of once per event.
	warned map[string]bool
}

// newZoneResolver reads the VTIMEZONE components out of an ICS payload.
//
// This works on the raw text rather than on go-ical's parsed tree so that the
// decoder path and the text fallback path in ics.go share one set of zone
// semantics. Whichever way an event reaches us, its TZIDs resolve identically.
func newZoneResolver(icsData string) *zoneResolver {
	r := &zoneResolver{
		blocks:   make(map[string]string),
		declared: make(map[string]declaredZone),
		warned:   make(map[string]bool),
	}

	var (
		cur        strings.Builder
		tzid       string
		offsets    []int
		stdOffset  int
		haveStd    bool
		inBlock    bool
		inStandard bool
	)

	for _, raw := range strings.Split(unfold(icsData), "\n") {
		line := strings.TrimRight(raw, "\r")

		if strings.HasPrefix(line, "BEGIN:VTIMEZONE") {
			inBlock, inStandard = true, false
			tzid, offsets, stdOffset, haveStd = "", nil, 0, false
			cur.Reset()
		}
		if !inBlock {
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\r\n")

		switch {
		case strings.HasPrefix(line, "TZID:"):
			tzid = strings.TrimSpace(strings.TrimPrefix(line, "TZID:"))
		case strings.HasPrefix(line, "BEGIN:STANDARD"):
			inStandard = true
		case strings.HasPrefix(line, "BEGIN:DAYLIGHT"):
			inStandard = false
		case strings.HasPrefix(line, "TZOFFSETTO:"):
			off, err := parseUTCOffset(strings.TrimPrefix(line, "TZOFFSETTO:"))
			if err != nil {
				break // malformed offset: ignore this one, keep the block
			}
			offsets = append(offsets, off)
			if inStandard && !haveStd {
				stdOffset, haveStd = off, true
			}
		case strings.HasPrefix(line, "END:VTIMEZONE"):
			r.addBlock(tzid, cur.String(), offsets, stdOffset, haveStd)
			inBlock = false
		}
	}

	return r
}

// addBlock records one finished VTIMEZONE component.
func (r *zoneResolver) addBlock(tzid, block string, offsets []int, stdOffset int, haveStd bool) {
	if tzid == "" || len(offsets) == 0 {
		return
	}
	r.blocks[tzid] = block

	uniform := true
	for _, off := range offsets[1:] {
		if off != offsets[0] {
			uniform = false
			break
		}
	}

	var dz declaredZone
	if uniform {
		dz.loc = time.FixedZone(tzid, offsets[0])
	}
	if haveStd {
		dz.standard = time.FixedZone(tzid, stdOffset)
	}
	r.declared[tzid] = dz
}

// parseProp resolves one DTSTART/DTEND-style value into an absolute instant.
//
// RFC 5545 gives three forms and all three occur in the wild: a DATE
// (`20260810`) is a floating calendar date, a value ending in Z is UTC, and
// anything else is a wall-clock reading that means whatever its TZID says — or,
// with no TZID at all, whatever zone the reader happens to be in ("floating").
// For a subscribed feed the reader's zone is a poor guess, so floating values
// take the caller-supplied fallback.
func (r *zoneResolver) parseProp(value, tzid string, isDate bool, fallback *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)

	// Not iCalendar at all, but hand-rolled feeds do emit ISO-8601 with
	// separators, and the parser this replaced accepted it.
	if strings.Contains(value, "-") {
		return parseLooseISO(value, fallback)
	}

	if isDate {
		// All-day events are stored as midnight UTC. That is the convention the
		// rest of the code already assumes; making them genuinely zone-free is
		// tracked in docs/backlog.md.
		return time.ParseInLocation(icalDate, value, time.UTC)
	}

	if strings.HasSuffix(value, "Z") {
		return time.ParseInLocation(icalDateTimeUTC, value, time.UTC)
	}

	if tzid == "" {
		return time.ParseInLocation(icalDateTime, value, fallback)
	}

	// Read the wall clock first, with no zone attached, purely to learn which
	// instant the zone question is being asked about.
	wall, err := time.ParseInLocation(icalDateTime, value, time.UTC)
	if err != nil {
		return time.Time{}, err
	}

	return time.ParseInLocation(icalDateTime, value, r.locationFor(tzid, wall, fallback))
}

// parseLooseISO accepts the ISO-8601 spellings that turn up in feeds which are
// not really iCalendar. A value carrying its own offset keeps it; one without
// falls back like any other floating time.
func parseLooseISO(value string, fallback *time.Location) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, value, fallback); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("ical: unrecognised date-time %q", value)
}

// locationFor picks the zone a TZID-qualified wall time should be read in, and
// complains whenever its two possible sources disagree.
func (r *zoneResolver) locationFor(tzid string, wall time.Time, fallback *time.Location) *time.Location {
	iana, ianaErr := time.LoadLocation(tzid)
	dz, declared := r.declared[tzid]

	if ianaErr != nil {
		// Not a name this host knows. Outlook and Exchange, for instance, emit
		// Windows zone names like "Central Asia Standard Time" — and they ship
		// a full VTIMEZONE for them, which is precisely what it is there for.
		switch {
		case declared && dz.loc != nil:
			r.warnOnce("unknown:"+tzid, "importer: TZID %q is unknown to this host; using the VTIMEZONE the feed shipped for it (%s)",
				tzid, formatOffset(offsetAtWall(dz.loc, wall)))
			return dz.loc
		case declared && dz.standard != nil:
			r.warnOnce("unknown:"+tzid, "importer: TZID %q is unknown to this host and its VTIMEZONE declares transitions we do not model; using its STANDARD offset (%s), so times may be off while DST is in effect",
				tzid, formatOffset(offsetAtWall(dz.standard, wall)))
			return dz.standard
		default:
			r.warnOnce("unknown:"+tzid, "importer: TZID %q is unknown to this host and the feed shipped no VTIMEZONE for it; falling back to %s",
				tzid, fallback)
			return fallback
		}
	}

	if !declared || dz.loc == nil {
		return iana
	}

	hostOff := offsetAtWall(iana, wall)
	feedOff := offsetAtWall(dz.loc, wall)
	if hostOff == feedOff {
		return iana
	}

	if hasSeasonalShift(iana, wall.Year()) {
		r.warnOnce("truncated:"+tzid, "importer: the VTIMEZONE for %q declares one fixed offset (%s) but this host has seasonal rules for that zone (%s at %s); treating the feed's block as truncated and keeping the host rules",
			tzid, formatOffset(feedOff), formatOffset(hostOff), wall.Format(icalDateTime))
		return iana
	}

	r.warnOnce("skew:"+tzid, "importer: zone %q — this host says %s, the feed's own VTIMEZONE says %s; trusting the feed. The host zone database is probably stale, which would otherwise shift every event in this feed by %s",
		tzid, formatOffset(hostOff), formatOffset(feedOff), time.Duration(hostOff-feedOff)*time.Second)
	return dz.loc
}

// warnOnce logs a message the first time its key is seen.
func (r *zoneResolver) warnOnce(key, format string, args ...interface{}) {
	if r.warned[key] {
		return
	}
	r.warned[key] = true
	log.Printf(format, args...)
}

// withVTimezones re-attaches the VTIMEZONE definitions an event depends on.
//
// Each event is stored as a self-contained VCALENDAR, and the encoder writes
// only the VEVENT it is handed — so a `TZID=Asia/Almaty` parameter used to
// survive into ical_data with its definition left behind in the feed. Anything
// reading our copy afterwards (our own CalDAV export, a client fetching it) then
// had to resolve the bare name against its own zone database, and could land on
// a different offset than the sender meant.
//
// tzids is expected in a stable order: ical_data is hashed into the ETag, and a
// hash that shuffles between syncs would rewrite every event every time.
func withVTimezones(icalData string, tz *zoneResolver, tzids []string) string {
	var blocks strings.Builder
	seen := make(map[string]bool)
	for _, id := range tzids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if block, ok := tz.blocks[id]; ok {
			blocks.WriteString(block)
		}
	}
	if blocks.Len() == 0 {
		return icalData
	}

	// A VTIMEZONE has to come before the component that references it.
	if i := strings.Index(icalData, "BEGIN:VEVENT"); i >= 0 {
		return icalData[:i] + blocks.String() + icalData[i:]
	}
	return icalData
}

// offsetAtWall reports the UTC offset a zone uses for a given wall-clock
// reading. Asking by wall time rather than by instant is what matters here: the
// question is always "this local reading, in that zone".
func offsetAtWall(loc *time.Location, wall time.Time) int {
	t := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), 0, loc)
	_, off := t.Zone()
	return off
}

// hasSeasonalShift reports whether a zone changes offset over the course of a
// year, which is how we tell a truncated VTIMEZONE from a stale host database.
func hasSeasonalShift(loc *time.Location, year int) bool {
	jan := time.Date(year, time.January, 15, 12, 0, 0, 0, time.UTC)
	jul := time.Date(year, time.July, 15, 12, 0, 0, 0, time.UTC)
	return offsetAtWall(loc, jan) != offsetAtWall(loc, jul)
}

// parseUTCOffset parses a TZOFFSETTO/TZOFFSETFROM value: ±hhmm or ±hhmmss.
func parseUTCOffset(v string) (int, error) {
	v = strings.TrimSpace(v)
	if len(v) != 5 && len(v) != 7 {
		return 0, fmt.Errorf("ical: malformed UTC offset %q", v)
	}

	sign := 1
	switch v[0] {
	case '+':
	case '-':
		sign = -1
	default:
		return 0, fmt.Errorf("ical: malformed UTC offset %q", v)
	}

	hours, err := strconv.Atoi(v[1:3])
	if err != nil {
		return 0, fmt.Errorf("ical: malformed UTC offset %q: %w", v, err)
	}
	minutes, err := strconv.Atoi(v[3:5])
	if err != nil {
		return 0, fmt.Errorf("ical: malformed UTC offset %q: %w", v, err)
	}
	seconds := 0
	if len(v) == 7 {
		if seconds, err = strconv.Atoi(v[5:7]); err != nil {
			return 0, fmt.Errorf("ical: malformed UTC offset %q: %w", v, err)
		}
	}

	return sign * (hours*3600 + minutes*60 + seconds), nil
}

// formatOffset renders an offset in seconds as UTC±HH:MM.
func formatOffset(sec int) string {
	sign := "+"
	if sec < 0 {
		sign, sec = "-", -sec
	}
	return fmt.Sprintf("UTC%s%02d:%02d", sign, sec/3600, (sec%3600)/60)
}

// unfold undoes RFC 5545 line folding, where a continuation line begins with a
// space or a tab.
func unfold(s string) string {
	for _, fold := range []string{"\r\n ", "\r\n\t", "\n ", "\n\t"} {
		s = strings.ReplaceAll(s, fold, "")
	}
	return s
}

// icalParam pulls one parameter out of a raw content line's parameter section:
// `DTSTART;TZID=Asia/Almaty:20260810T120000` yields "Asia/Almaty" for "TZID".
func icalParam(line, name string) string {
	head := line
	if i := strings.Index(line, ":"); i >= 0 {
		head = line[:i]
	}

	parts := strings.Split(head, ";")
	for _, part := range parts[1:] {
		k, v, ok := strings.Cut(part, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}
