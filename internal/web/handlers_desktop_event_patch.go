package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// EventPatchRequest is the body shape for PATCH /api/desktop/v1/events/{id}.
// `scope` is ignored for non-recurring events; "single" and "future" require
// `recurrence_id` (ms — the start of the instance being edited).
type EventPatchRequest struct {
	Scope        string  `json:"scope"`         // "all" | "single" | "future"
	RecurrenceID int64   `json:"recurrence_id"` // ms, the instance being edited
	Summary      *string `json:"summary"`
	Description  *string `json:"description"`
	Location     *string `json:"location"`
	DTStart      *int64  `json:"dtstart"`
	DTEnd        *int64  `json:"dtend"`
	AllDay       *bool   `json:"all_day"`
}

// HandleDesktopEventPatch edits a calendar event. Scope handling:
//   - "all" (default): rewrite the master VEVENT in place. All instances of a
//     recurring series inherit the change.
//   - "future": modify the master's RRULE to end one millisecond before the
//     instance being edited, then create a new event with a fresh UID whose
//     DTSTART is the edited instance and whose RRULE continues from there.
//     The two halves are separate UIDs — the cleanest cross-server split.
//   - "single": not yet implemented. iCal allows per-instance overrides via
//     a sibling VEVENT with RECURRENCE-ID inside the same ical_data, but the
//     client-side RRULE expansion would also need to apply those overrides
//     during occurrence generation. Tracked for a follow-up.
//
// Read-only sources (ics_url / ics_import) reject the edit; the client hides
// the edit button for those calendars but we still validate server-side.
func (s *Server) HandleDesktopEventPatch(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	eventID, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad event id")
		return
	}

	var req EventPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid body")
		return
	}
	scope := strings.ToLower(strings.TrimSpace(req.Scope))
	if scope == "" {
		scope = "all"
	}

	event, err := s.database.GetEventByID(eventID)
	if err != nil {
		respondError(w, http.StatusNotFound, "event not found")
		return
	}
	cal, err := s.database.GetCalendarByID(event.CalendarID)
	if err != nil || cal.UserID != user.ID {
		respondError(w, http.StatusNotFound, "calendar not found")
		return
	}
	if cal.SourceType == "ics_url" || cal.SourceType == "ics_import" {
		respondError(w, http.StatusBadRequest, "calendar is read-only")
		return
	}

	// Non-recurring events: scope choice doesn't matter, treat as "all".
	isRecurring := event.RRule != ""
	if !isRecurring {
		scope = "all"
	}

	switch scope {
	case "all":
		if err := s.applyEditAll(event, &req); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.queueEventReverseSync(cal, event, "update")

	case "future":
		if req.RecurrenceID == 0 {
			respondError(w, http.StatusBadRequest, "recurrence_id required for scope=future")
			return
		}
		newEvent, err := s.applyEditFuture(event, cal, &req)
		if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.queueEventReverseSync(cal, event, "update")
		s.queueEventReverseSync(cal, newEvent, "create")

	case "single":
		respondError(w, http.StatusNotImplemented,
			"editing a single instance isn't implemented yet — choose 'this and future' or 'all events'")
		return

	default:
		respondError(w, http.StatusBadRequest, "scope must be all|single|future")
		return
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"id":    event.ID,
		"scope": scope,
	})
}

// applyEditAll rewrites the master VEVENT with the supplied fields. Unspecified
// fields are left untouched. ical_data is round-tripped through go-ical so
// VALARM blocks and X-* properties survive the edit.
func (s *Server) applyEditAll(event *models.CalendarEvent, req *EventPatchRequest) error {
	applyFieldsToModel(event, req)
	if newICal, ok := mutateEventInICal(event.ICalData, "", req); ok {
		event.ICalData = newICal
	}
	event.LocalModified = true
	return s.database.UpdateCalendarEvent(event)
}

// applyEditFuture splits the recurring series: the master's RRULE gets a
// new UNTIL clause one millisecond before the edited instance, and a new
// event row with a derived UID inherits the remaining recurrences starting
// at the edited instance with the modified fields applied.
//
// Idempotent on retry: the continuation UID is derived deterministically
// from the master UID + cutoff, so a duplicate request finds the existing
// continuation via GetEventByUID and updates fields in place rather than
// creating a second row.
func (s *Server) applyEditFuture(master *models.CalendarEvent, cal *models.Calendar, req *EventPatchRequest) (*models.CalendarEvent, error) {
	cutoff := req.RecurrenceID - 1 // one ms before the edited instance
	contUID := fmt.Sprintf("%s-cont-%d@ddmailserver", master.UID, req.RecurrenceID)

	// 1. Cap the master series — safe to repeat: addUntilToRRule replaces
	// any existing UNTIL clause, so re-running the operation produces the
	// same capped value.
	cappedRule := addUntilToRRule(master.RRule, timeutil.FromMs(cutoff))
	master.RRule = cappedRule
	if newICal, ok := capRRuleInICal(master.ICalData, cappedRule); ok {
		master.ICalData = newICal
	}
	master.LocalModified = true
	if err := s.database.UpdateCalendarEvent(master); err != nil {
		return nil, fmt.Errorf("update master: %w", err)
	}

	// 2. Resolve continuation: either an existing row from a prior call, or
	// a fresh one. Updating in place on retry avoids two duplicate halves
	// hanging around after a network glitch.
	existing, _ := s.database.GetEventByUID(master.CalendarID, contUID)
	duration := int64(0)
	if master.DTEnd != nil && *master.DTEnd > master.DTStart {
		duration = *master.DTEnd - master.DTStart
	}
	if existing != nil {
		applyFieldsToModel(existing, req)
		if req.DTStart == nil {
			existing.DTStart = req.RecurrenceID
		}
		if req.DTEnd == nil && duration > 0 {
			end := existing.DTStart + duration
			existing.DTEnd = &end
		}
		existing.RRule = stripUntilFromRRule(master.RRule)
		existing.ICalData = generateICalData(existing)
		existing.LocalModified = true
		if err := s.database.UpdateCalendarEvent(existing); err != nil {
			return nil, fmt.Errorf("update continuation: %w", err)
		}
		return existing, nil
	}

	cont := *master
	cont.ID = 0
	cont.UID = contUID
	cont.RemoteID = ""
	cont.ETag = ""
	cont.RRule = stripUntilFromRRule(master.RRule)
	cont.DTStart = req.RecurrenceID
	if duration > 0 {
		end := req.RecurrenceID + duration
		cont.DTEnd = &end
	}
	applyFieldsToModel(&cont, req)
	cont.ICalData = generateICalData(&cont)
	cont.LocalModified = true
	if err := s.database.CreateCalendarEvent(&cont); err != nil {
		return nil, fmt.Errorf("create continuation: %w", err)
	}
	return &cont, nil
}

// queueEventReverseSync wraps the reverse-sync queue with the common
// "skip when not a CalDAV calendar / reverse_sync disabled" preflight.
func (s *Server) queueEventReverseSync(cal *models.Calendar, event *models.CalendarEvent, op string) {
	if cal.SourceType != "caldav" || !cal.Enabled || !cal.ReverseSync {
		return
	}
	_ = s.database.QueueCalendarEventSync(
		event.ID, cal.ID, cal.SourceID,
		event.UID, event.RemoteID, event.ICalData, op,
	)
}

// applyFieldsToModel writes the request's non-nil pointer fields onto the
// model in place. The pointer-vs-value distinction matters because PATCH
// semantics need to differentiate "field not specified" from "field set to
// empty string / 0 / false".
func applyFieldsToModel(e *models.CalendarEvent, req *EventPatchRequest) {
	if req.Summary != nil {
		e.Summary = *req.Summary
	}
	if req.Description != nil {
		e.Description = *req.Description
	}
	if req.Location != nil {
		e.Location = *req.Location
	}
	if req.DTStart != nil {
		e.DTStart = *req.DTStart
	}
	if req.DTEnd != nil {
		// dtend=0 from the client means "clear it" (single-point event).
		if *req.DTEnd == 0 {
			e.DTEnd = nil
		} else {
			v := *req.DTEnd
			e.DTEnd = &v
		}
	}
	if req.AllDay != nil {
		e.AllDay = *req.AllDay
	}
}

// mutateEventInICal re-parses the ical_data, mutates the VEVENT matching
// `recurrenceID` (empty string ⇒ master VEVENT), and re-serializes. Returns
// ok=false on a parse failure — caller leaves ical_data alone in that case
// and the indexed fields remain the source of truth for our own UI.
func mutateEventInICal(icalData, recurrenceID string, req *EventPatchRequest) (string, bool) {
	dec := ical.NewDecoder(strings.NewReader(icalData))
	cal, err := dec.Decode()
	if err != nil {
		return "", false
	}

	for _, comp := range cal.Children {
		if comp.Name != ical.CompEvent {
			continue
		}
		// Targeting master = empty recurrence id; targeting an override = match value.
		ridProp := comp.Props.Get(ical.PropRecurrenceID)
		gotRID := ""
		if ridProp != nil {
			gotRID = ridProp.Value
		}
		if gotRID != recurrenceID {
			continue
		}

		if req.Summary != nil {
			comp.Props.SetText(ical.PropSummary, *req.Summary)
		}
		if req.Description != nil {
			comp.Props.SetText(ical.PropDescription, *req.Description)
		}
		if req.Location != nil {
			comp.Props.SetText(ical.PropLocation, *req.Location)
		}
		if req.DTStart != nil {
			p := ical.NewProp(ical.PropDateTimeStart)
			p.SetDateTime(timeutil.FromMs(*req.DTStart))
			comp.Props.Del(ical.PropDateTimeStart)
			comp.Props.Add(p)
		}
		if req.DTEnd != nil {
			comp.Props.Del(ical.PropDateTimeEnd)
			if *req.DTEnd != 0 {
				p := ical.NewProp(ical.PropDateTimeEnd)
				p.SetDateTime(timeutil.FromMs(*req.DTEnd))
				comp.Props.Add(p)
			}
		}
	}

	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return "", false
	}
	return buf.String(), true
}

// addUntilToRRule injects or replaces the UNTIL clause in an RRULE string.
// UNTIL is rendered in UTC basic ISO 8601, per RFC 5545 §3.3.10.
func addUntilToRRule(rrule string, until time.Time) string {
	untilStr := until.UTC().Format("20060102T150405Z")
	parts := strings.Split(rrule, ";")
	out := make([]string, 0, len(parts)+1)
	replaced := false
	for _, p := range parts {
		if strings.HasPrefix(strings.ToUpper(p), "UNTIL=") {
			out = append(out, "UNTIL="+untilStr)
			replaced = true
			continue
		}
		if strings.HasPrefix(strings.ToUpper(p), "COUNT=") {
			// COUNT and UNTIL are mutually exclusive per spec; drop COUNT
			// when we're capping with UNTIL.
			continue
		}
		out = append(out, p)
	}
	if !replaced {
		out = append(out, "UNTIL="+untilStr)
	}
	return strings.Join(out, ";")
}

// stripUntilFromRRule returns the rule with any UNTIL= component removed,
// used when forking the continuation series — the new series should run
// indefinitely (or to whatever the original UNTIL was, which we don't try
// to preserve for first-cut simplicity).
func stripUntilFromRRule(rrule string) string {
	parts := strings.Split(rrule, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.HasPrefix(strings.ToUpper(p), "UNTIL=") {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, ";")
}

// capRRuleInICal rewrites the RRULE property of the master VEVENT inside
// the raw ical_data. Used together with addUntilToRRule when splitting a
// series — the indexed RRule column gets the new value but the iCal blob
// also has to be updated for CalDAV reverse-sync to push the cap upstream.
func capRRuleInICal(icalData, newRule string) (string, bool) {
	dec := ical.NewDecoder(strings.NewReader(icalData))
	cal, err := dec.Decode()
	if err != nil {
		return "", false
	}
	for _, comp := range cal.Children {
		if comp.Name != ical.CompEvent {
			continue
		}
		// Master = no RECURRENCE-ID property.
		if comp.Props.Get(ical.PropRecurrenceID) != nil {
			continue
		}
		comp.Props.SetText(ical.PropRecurrenceRule, newRule)
	}
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return "", false
	}
	return buf.String(), true
}
