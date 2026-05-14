package web

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

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
	ID             int64                     `json:"id"`
	CalendarID     int64                     `json:"calendar_id"`
	UID            string                    `json:"uid"`
	Summary        string                    `json:"summary"`
	Description    string                    `json:"description,omitempty"`
	Location       string                    `json:"location,omitempty"`
	DTStart        int64                     `json:"dtstart"`        // ms since epoch
	DTEnd          *int64                    `json:"dtend"`          // ms since epoch, nullable
	AllDay         bool                      `json:"all_day"`
	OrganizerEmail string                    `json:"organizer_email,omitempty"`
	OrganizerName  string                    `json:"organizer_name,omitempty"`
	Status         string                    `json:"status,omitempty"`
	RRule          string                    `json:"rrule,omitempty"`
	RecurrenceID   string                    `json:"recurrence_id,omitempty"`
	// ExDates lists deleted recurring-instance starts as ms-since-epoch. The
	// client must filter these out when expanding `RRule` so removed
	// occurrences don't reappear on the calendar.
	ExDates   []int64                   `json:"exdates,omitempty"`
	Attendees []DesktopCalendarAttendee `json:"attendees,omitempty"`
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

	out := make([]DesktopCalendarEvent, 0, len(events))
	for _, e := range events {
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
		})
	}

	respondJSON(w, http.StatusOK, out)
}
