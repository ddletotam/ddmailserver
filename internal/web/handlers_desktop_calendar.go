package web

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// parseExDates pulls EXDATE values out of raw iCal data and converts them
// to ms-since-epoch. Handles the most common forms:
//
//	EXDATE;TZID=Europe/Moscow:20260513T120000
//	EXDATE:20260513T120000Z
//	EXDATE;VALUE=DATE:20260513
//	EXDATE:20260513T120000,20260520T120000
//
// Returned values are sorted ascending. Anything we can't parse is silently
// skipped — a single malformed EXDATE shouldn't poison the rest, and the
// caller can still expand RRULE without that one exception.
func parseExDates(icalData string) []int64 {
	if !strings.Contains(icalData, "EXDATE") {
		return nil
	}
	var out []int64
	// Unfold RFC 5545 line folding before processing — continuation lines
	// start with a single space/tab and continue the previous logical line.
	lines := strings.Split(icalData, "\n")
	logical := make([]string, 0, len(lines))
	for _, raw := range lines {
		raw = strings.TrimRight(raw, "\r")
		if (strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")) && len(logical) > 0 {
			logical[len(logical)-1] += raw[1:]
			continue
		}
		logical = append(logical, raw)
	}
	for _, line := range logical {
		if !strings.HasPrefix(line, "EXDATE") {
			continue
		}
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		params := line[:colon]
		values := line[colon+1:]
		loc := time.UTC
		isDate := false
		for _, p := range strings.Split(params, ";") {
			if strings.HasPrefix(p, "TZID=") {
				if l, err := time.LoadLocation(strings.TrimPrefix(p, "TZID=")); err == nil {
					loc = l
				}
			}
			if p == "VALUE=DATE" {
				isDate = true
			}
		}
		for _, v := range strings.Split(values, ",") {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			var parsed time.Time
			var ok bool
			if isDate {
				if t, err := time.ParseInLocation("20060102", v, loc); err == nil {
					parsed, ok = t, true
				}
			} else if strings.HasSuffix(v, "Z") {
				if t, err := time.Parse("20060102T150405Z", v); err == nil {
					parsed, ok = t, true
				}
			} else {
				if t, err := time.ParseInLocation("20060102T150405", v, loc); err == nil {
					parsed, ok = t, true
				}
			}
			if ok {
				out = append(out, timeutil.ToMs(parsed))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// parseAlarmLeadMin pulls the first VALARM TRIGGER from an iCal blob and
// returns it as "minutes before event start" (always a positive integer).
//
// Recognised forms (RFC 5545):
//
//	TRIGGER:-PT15M             → 15
//	TRIGGER:-PT1H30M           → 90
//	TRIGGER:-P1D               → 1440
//	TRIGGER;RELATED=START:-PT5M → 5
//
// Skipped (returns 0):
//   - Positive TRIGGER (alarm AFTER event start — we don't surface those).
//   - TRIGGER;RELATED=END (alarm relative to end — meaning is fuzzy until
//     we surface event duration in the reminder; deferred).
//   - VALUE=DATE-TIME (absolute time — needs occurrence-aware handling for
//     recurring events; deferred).
//   - Anything we can't parse.
//
// Only the first VALARM is considered — clients that need multi-alarm
// support can expand this once we have UI for it.
func parseAlarmLeadMin(icalData string) int {
	if !strings.Contains(icalData, "VALARM") {
		return 0
	}
	// Unfold once so a `TRIGGER:\n -PT15M` continuation still resolves.
	lines := strings.Split(icalData, "\n")
	logical := make([]string, 0, len(lines))
	for _, raw := range lines {
		raw = strings.TrimRight(raw, "\r")
		if (strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")) && len(logical) > 0 {
			logical[len(logical)-1] += raw[1:]
			continue
		}
		logical = append(logical, raw)
	}
	inAlarm := false
	for _, line := range logical {
		switch {
		case strings.HasPrefix(line, "BEGIN:VALARM"):
			inAlarm = true
		case strings.HasPrefix(line, "END:VALARM"):
			return 0 // first VALARM had no usable TRIGGER
		case inAlarm && strings.HasPrefix(line, "TRIGGER"):
			colon := strings.Index(line, ":")
			if colon < 0 {
				continue
			}
			params := line[:colon]
			value := strings.TrimSpace(line[colon+1:])
			// Skip TRIGGER;RELATED=END or VALUE=DATE-TIME — out of scope.
			if strings.Contains(params, "RELATED=END") || strings.Contains(params, "VALUE=DATE-TIME") {
				continue
			}
			// Must start with '-' for "before"; positive means after start.
			if !strings.HasPrefix(value, "-") {
				continue
			}
			value = strings.TrimPrefix(value, "-")
			// Expect "PnDTnHnMnS" but tolerate any prefix-stripping iso-8601
			// the spec allows. We parse manually since stdlib doesn't have
			// an ISO-8601 duration parser.
			min := iso8601DurationToMinutes(value)
			if min > 0 {
				return min
			}
		}
	}
	return 0
}

// parseAlarmLeads returns the MASTER VEVENT's VALARM triggers as "minutes
// before start", in document order: element 0 is the primary reminder, the
// rest form the secondary cascade (each fires only if the previous toast
// died by timeout — client-side logic). Non-negative triggers (PT0S, "at
// the moment of start") map to 0. Events without a single usable VALARM get
// the server default — the server owns the default, the client consumes it.
//
// Critically it reads ONLY the master VEVENT (the one without a
// RECURRENCE-ID): a recurring resource bundles the master plus one VEVENT
// per modified occurrence, each with its own VALARM. Scanning the whole blob
// concatenated every override's alarms onto every occurrence — dozens of
// duplicated reminders and an endless toast cascade.
func parseAlarmLeads(icalData string) []int {
	const defaultLead = 10
	if !strings.Contains(icalData, "VALARM") {
		return []int{defaultLead}
	}
	cal, err := ical.NewDecoder(strings.NewReader(icalData)).Decode()
	if err != nil {
		return []int{defaultLead}
	}
	events := cal.Events()
	var master *ical.Event
	for i := range events {
		if events[i].Props.Get(ical.PropRecurrenceID) == nil {
			master = &events[i]
			break
		}
	}
	if master == nil {
		return []int{defaultLead}
	}

	var leads []int
	seen := map[int]bool{}
	for _, child := range master.Children {
		if child.Name != ical.CompAlarm {
			continue
		}
		trig := child.Props.Get(ical.PropTrigger)
		if trig == nil {
			continue
		}
		if trig.Params.Get("RELATED") == "END" || trig.Params.Get(ical.ParamValue) == "DATE-TIME" {
			continue
		}
		v := strings.TrimSpace(trig.Value)
		lead := 0
		if strings.HasPrefix(v, "-") {
			lead = iso8601DurationToMinutes(strings.TrimPrefix(v, "-"))
			if lead <= 0 {
				continue // unparseable "before" trigger
			}
		}
		// De-dupe identical triggers (some clients emit DISPLAY + EMAIL
		// alarms at the same lead).
		if seen[lead] {
			continue
		}
		seen[lead] = true
		leads = append(leads, lead)
	}
	if len(leads) == 0 {
		return []int{defaultLead}
	}
	return leads
}

// iso8601DurationToMinutes parses the shape "P[nW][nD][T[nH][nM][nS]]" and
// returns the total in minutes (sub-minute components are floored). Returns
// 0 on any parse error — we lose the alarm but never poison the response.
func iso8601DurationToMinutes(s string) int {
	if !strings.HasPrefix(s, "P") {
		return 0
	}
	s = s[1:]
	var total int
	inTime := false
	var num string
	flush := func(unit byte) bool {
		if num == "" {
			return false
		}
		n, err := strconv.Atoi(num)
		num = ""
		if err != nil {
			return false
		}
		switch unit {
		case 'W':
			total += n * 7 * 24 * 60
		case 'D':
			total += n * 24 * 60
		case 'H':
			total += n * 60
		case 'M':
			if inTime {
				total += n
			} else {
				// month — not meaningful for VALARM, but tolerate
				return false
			}
		case 'S':
			// floor: 30 s of lead-time isn't worth burning a unit on
		default:
			return false
		}
		return true
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == 'T':
			inTime = true
		case c >= '0' && c <= '9':
			num += string(c)
		case c == 'W', c == 'D', c == 'H', c == 'M', c == 'S':
			if !flush(c) {
				return 0
			}
		default:
			return 0
		}
	}
	return total
}

// DesktopCalendar is the trimmed shape the desktop client wants for a calendar.
// Server color is included for back-compat but the desktop ignores it and
// keeps a per-calendar color override in localStorage.
type DesktopCalendar struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"`
	SourceType  string `json:"source_type"`
	CanWrite    bool   `json:"can_write"`
	Enabled     bool   `json:"enabled"`
	Timezone    string `json:"timezone,omitempty"`
}

// DesktopCalendarAttendee is the trimmed attendee shape; matches what the
// desktop card renders (chips + RSVP pills for the matching identity).
type DesktopCalendarAttendee struct {
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	Role     string `json:"role,omitempty"`
	PartStat string `json:"partstat,omitempty"`
}

// DesktopCalendarEvent flattens models.CalendarEvent for JSON over the wire.
// Recurring events come back as the master record (client expands RRULE).
type DesktopCalendarEvent struct {
	ID             int64  `json:"id"`
	CalendarID     int64  `json:"calendar_id"`
	UID            string `json:"uid"`
	Summary        string `json:"summary"`
	Description    string `json:"description,omitempty"`
	Location       string `json:"location,omitempty"`
	DTStart        int64  `json:"dtstart"` // ms since epoch
	DTEnd          *int64 `json:"dtend"`   // ms since epoch, nullable
	AllDay         bool   `json:"all_day"`
	OrganizerEmail string `json:"organizer_email,omitempty"`
	OrganizerName  string `json:"organizer_name,omitempty"`
	Status         string `json:"status,omitempty"`
	RRule          string `json:"rrule,omitempty"`
	RecurrenceID   string `json:"recurrence_id,omitempty"`
	// ExDates lists deleted recurring-instance starts as ms-since-epoch. The
	// client must filter these out when expanding `RRule` so removed
	// occurrences don't reappear on the calendar.
	ExDates   []int64                   `json:"exdates,omitempty"`
	Attendees []DesktopCalendarAttendee `json:"attendees,omitempty"`
	// AlarmLeadMin is the VALARM trigger expressed as "minutes before
	// start" (positive int). 0 means no usable VALARM was found and the
	// desktop should fall back to its global default lead time.
	// Deprecated in favour of AlarmLeads; kept for older clients.
	AlarmLeadMin int `json:"alarm_lead_min,omitempty"`
	// AlarmLeads lists EVERY VALARM as "minutes before start" in document
	// order (0 = at start). Never empty: events without alarms carry the
	// server default. Element 0 = primary reminder, the rest cascade.
	AlarmLeads []int `json:"alarm_leads,omitempty"`
	// Extras carries every VEVENT property that is neither surfaced as a
	// first-class field above nor pure plumbing, with default values
	// (TRANSP:OPAQUE, CLASS:PUBLIC…) elided. This is how conference links
	// (CONFERENCE / X-TELEMOST-CONFERENCE), URL, CATEGORIES etc. reach the
	// desktop card — they only exist inside ical_data.
	Extras []DesktopEventExtra `json:"extras,omitempty"`
}

// DesktopEventExtra is one non-default VEVENT property, name uppercased.
type DesktopEventExtra struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// desktopExtraSkip lists VEVENT properties already surfaced as first-class
// DesktopCalendarEvent fields, or pure plumbing that means nothing to a user.
var desktopExtraSkip = map[string]bool{
	"UID": true, "SUMMARY": true, "DESCRIPTION": true, "LOCATION": true,
	"DTSTART": true, "DTEND": true, "DURATION": true, "DTSTAMP": true,
	"CREATED": true, "LAST-MODIFIED": true, "SEQUENCE": true,
	"ORGANIZER": true, "ATTENDEE": true, "RRULE": true, "RDATE": true,
	"EXDATE": true, "RECURRENCE-ID": true, "STATUS": true,
}

// extraRank orders the extras: recognisable meeting fields first, the
// alphabetical long tail after.
func extraRank(name string) int {
	switch name {
	case "CONFERENCE":
		return 0
	case "URL":
		return 1
	case "CATEGORIES":
		return 2
	case "CLASS":
		return 3
	case "TRANSP":
		return 4
	case "PRIORITY":
		return 5
	default:
		return 10
	}
}

// extraPropsFromICal extracts the non-default properties of the master
// VEVENT. Values are deduplicated case-insensitively: Yandex, for one,
// publishes the conference URL as both CONFERENCE and X-TELEMOST-CONFERENCE.
func extraPropsFromICal(icalData string) []DesktopEventExtra {
	if icalData == "" {
		return nil
	}
	cal, err := ical.NewDecoder(strings.NewReader(icalData)).Decode()
	if err != nil {
		return nil
	}
	events := cal.Events()
	var master *ical.Event
	for i := range events {
		if events[i].Props.Get(ical.PropRecurrenceID) == nil {
			master = &events[i]
			break
		}
	}
	if master == nil {
		return nil
	}

	names := make([]string, 0, len(master.Props))
	for name := range master.Props {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		ri, rj := extraRank(names[i]), extraRank(names[j])
		if ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})

	var out []DesktopEventExtra
	seenVal := make(map[string]bool)
	for _, name := range names {
		up := strings.ToUpper(name)
		// X-MOZ-*/X-LIC-* are client bookkeeping (Thunderbird ack stamps
		// and libical annotations), never user-facing data.
		if desktopExtraSkip[up] || strings.HasPrefix(up, "X-MOZ-") || strings.HasPrefix(up, "X-LIC-") {
			continue
		}
		for _, p := range master.Props[name] {
			val := p.Value
			if t, terr := p.Text(); terr == nil && t != "" {
				val = t
			}
			val = strings.TrimSpace(val)
			if val == "" {
				continue
			}
			// Default values carry no information — elide.
			upVal := strings.ToUpper(val)
			if (up == "TRANSP" && upVal == "OPAQUE") ||
				(up == "CLASS" && upVal == "PUBLIC") ||
				(up == "PRIORITY" && (val == "0" || val == "5")) ||
				up == "METHOD" || up == "CALSCALE" {
				continue
			}
			key := strings.ToLower(val)
			if seenVal[key] {
				continue
			}
			seenVal[key] = true
			out = append(out, DesktopEventExtra{Name: up, Value: val})
		}
	}
	return out
}

// overridesFromICal pulls RECURRENCE-ID override VEVENTs out of a recurring
// master's raw iCal blob. CalDAV stores a whole series as ONE resource
// (master VEVENT + one VEVENT per modified instance), and our sync keeps one
// DB row per resource — so moved/edited instances exist ONLY inside
// ical_data. Without this, a meeting dragged to another day silently
// disappears from the desktop client.
//
// Returns the overridden original instants (the master's expansion must skip
// them — RFC 5545: an override REPLACES its RECURRENCE-ID instance) and the
// override instances as standalone one-off events.
func overridesFromICal(master *models.CalendarEvent) ([]int64, []DesktopCalendarEvent) {
	if master.ICalData == "" || !strings.Contains(master.ICalData, "RECURRENCE-ID") {
		return nil, nil
	}
	cal, err := ical.NewDecoder(strings.NewReader(master.ICalData)).Decode()
	if err != nil {
		return nil, nil
	}
	var excluded []int64
	var out []DesktopCalendarEvent
	for _, ev := range cal.Events() {
		recProp := ev.Props.Get(ical.PropRecurrenceID)
		if recProp == nil {
			continue // the master itself
		}
		recT, err := recProp.DateTime(nil)
		if err != nil {
			continue
		}
		excluded = append(excluded, timeutil.ToMs(recT))

		o := DesktopCalendarEvent{
			ID:           master.ID,
			CalendarID:   master.CalendarID,
			UID:          master.UID,
			Summary:      master.Summary,
			Location:     master.Location,
			Status:       master.Status,
			RecurrenceID: recProp.Value,
		}
		if p := ev.Props.Get(ical.PropSummary); p != nil {
			if v, err := p.Text(); err == nil && v != "" {
				o.Summary = v
			}
		}
		if p := ev.Props.Get(ical.PropLocation); p != nil {
			if v, err := p.Text(); err == nil && v != "" {
				o.Location = v
			}
		}
		if p := ev.Props.Get(ical.PropDateTimeStart); p != nil {
			if t, err := p.DateTime(nil); err == nil {
				o.DTStart = timeutil.ToMs(t)
			}
			if p.Params.Get(ical.ParamValue) == "DATE" {
				o.AllDay = true
			}
		}
		if o.DTStart == 0 {
			continue // unusable override, but its exclusion still applies
		}
		if p := ev.Props.Get(ical.PropDateTimeEnd); p != nil {
			if t, err := p.DateTime(nil); err == nil {
				ms := timeutil.ToMs(t)
				o.DTEnd = &ms
			}
		}
		out = append(out, o)
	}
	return excluded, out
}

// HandleDesktopCalendars returns all of the user's calendars (enabled + disabled).
// The client decides which to display via local visibility settings; server-side
// "enabled" is the soft-delete flag rather than a UI preference.
func (s *Server) HandleDesktopCalendars(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	cals, err := s.database.GetCalendarsByUserID(user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load calendars")
		return
	}

	out := make([]DesktopCalendar, 0, len(cals))
	for _, c := range cals {
		out = append(out, DesktopCalendar{
			ID:          c.ID,
			Name:        c.Name,
			Description: c.Description,
			Color:       c.Color,
			SourceType:  c.SourceType,
			CanWrite:    c.CanWrite,
			Enabled:     c.Enabled,
			Timezone:    c.Timezone,
		})
	}

	respondJSON(w, http.StatusOK, out)
}

// HandleDesktopCalendarEvents returns events for the selected calendars in the
// requested window. Required query params: from (ms), to (ms). Optional: ids
// (comma-separated calendar IDs). Without `ids` we serve the user's full set —
// keeps the first-load case (before settings are persisted) one round-trip.
func (s *Server) HandleDesktopCalendarEvents(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	q := r.URL.Query()
	fromMs, err := strconv.ParseInt(q.Get("from"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "missing or invalid 'from' (ms)")
		return
	}
	toMs, err := strconv.ParseInt(q.Get("to"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "missing or invalid 'to' (ms)")
		return
	}
	if toMs <= fromMs {
		respondError(w, http.StatusBadRequest, "'to' must be after 'from'")
		return
	}

	// Resolve calendar set: explicit ids → filtered to user's; missing → user's
	// full set. Filtering against user's set prevents another user's calendar
	// from being queried by ID guessing.
	allCals, err := s.database.GetCalendarsByUserID(user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load calendars")
		return
	}
	allowed := make(map[int64]bool, len(allCals))
	for _, c := range allCals {
		allowed[c.ID] = true
	}

	var ids []int64
	if raw := strings.TrimSpace(q.Get("ids")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err != nil {
				continue
			}
			if allowed[id] {
				ids = append(ids, id)
			}
		}
	} else {
		for id := range allowed {
			ids = append(ids, id)
		}
	}

	if len(ids) == 0 {
		respondJSON(w, http.StatusOK, []DesktopCalendarEvent{})
		return
	}

	events, err := s.database.GetEventsForCalendarsInRange(ids, fromMs, toMs)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load events")
		return
	}

	// UID+RECURRENCE-ID pairs already present as their own DB rows — those
	// must not be duplicated by the synthetic overrides below.
	storedOverrides := make(map[string]bool)
	for _, e := range events {
		if e.RecurrenceID != "" {
			storedOverrides[e.UID+"|"+e.RecurrenceID] = true
		}
	}

	out := make([]DesktopCalendarEvent, 0, len(events))
	for _, e := range events {
		extras := extraPropsFromICal(e.ICalData)
		atts, _ := s.database.GetAttendeesByEventID(e.ID)
		dtos := make([]DesktopCalendarAttendee, 0, len(atts))
		for _, a := range atts {
			dtos = append(dtos, DesktopCalendarAttendee{
				Email:    a.Email,
				Name:     a.Name,
				Role:     a.Role,
				PartStat: a.PartStat,
			})
		}
		var exDates []int64
		if e.RRule != "" {
			exDates = parseExDates(e.ICalData)
			// Moved/edited instances: exclude their original instants from
			// the master expansion and serve the overrides themselves as
			// one-off events when they land in the requested window.
			overriddenStarts, overrides := overridesFromICal(e)
			exDates = append(exDates, overriddenStarts...)
			for _, o := range overrides {
				if storedOverrides[o.UID+"|"+o.RecurrenceID] {
					continue
				}
				oEnd := o.DTStart + 30*60*1000
				if o.DTEnd != nil {
					oEnd = *o.DTEnd
				}
				if o.DTStart < toMs && oEnd > fromMs {
					o.Attendees = dtos
					o.AlarmLeadMin = parseAlarmLeadMin(e.ICalData)
					o.AlarmLeads = parseAlarmLeads(e.ICalData)
					o.Extras = extras
					out = append(out, o)
				}
			}
		}
		out = append(out, DesktopCalendarEvent{
			ID:             e.ID,
			CalendarID:     e.CalendarID,
			UID:            e.UID,
			Summary:        e.Summary,
			Description:    e.Description,
			Location:       e.Location,
			DTStart:        e.DTStart,
			DTEnd:          e.DTEnd,
			AllDay:         e.AllDay,
			OrganizerEmail: e.OrganizerEmail,
			OrganizerName:  e.OrganizerName,
			Status:         e.Status,
			RRule:          e.RRule,
			RecurrenceID:   e.RecurrenceID,
			ExDates:        exDates,
			Attendees:      dtos,
			AlarmLeadMin:   parseAlarmLeadMin(e.ICalData),
			AlarmLeads:     parseAlarmLeads(e.ICalData),
			Extras:         extras,
		})
	}

	respondJSON(w, http.StatusOK, out)
}
