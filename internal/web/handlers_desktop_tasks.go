package web

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	caldavutil "github.com/yourusername/mailserver/internal/caldav"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// DesktopTask is a VTODO as the desktop needs to hear it.
//
// Kept apart from DesktopCalendarEvent rather than adding a "component" field
// to it: a task has no end, often no start, and carries completion state an
// event has no place for. One struct serving both would be mostly-nil either
// way, and the week grid would have to learn to skip half its input — which is
// exactly the mess that produced untitled rows before tasks were understood.
type DesktopTask struct {
	ID         int64  `json:"id"`
	CalendarID int64  `json:"calendar_id"`
	UID        string `json:"uid"`

	Summary     string `json:"summary"`
	Description string `json:"description,omitempty"`

	// Start is optional and Due is optional, independently: a task may be
	// scheduled, deadlined, both, or neither. Milliseconds since epoch.
	Start  *int64 `json:"start,omitempty"`
	Due    *int64 `json:"due,omitempty"`
	AllDay bool   `json:"all_day"`

	// Status is the RFC 5545 spelling: NEEDS-ACTION, IN-PROCESS, COMPLETED,
	// CANCELLED.
	Status      string `json:"status"`
	Completed   bool   `json:"completed"`
	CompletedAt *int64 `json:"completed_at,omitempty"`

	PercentComplete *int16 `json:"percent_complete,omitempty"`
	// Priority: 0 undefined, 1 highest … 9 lowest (RFC 5545 §3.8.1.9).
	Priority *int16 `json:"priority,omitempty"`

	RRule string `json:"rrule,omitempty"`

	// Server-resolved, mirroring what the events endpoint provides so the
	// source-blind client can render one list: the owning calendar's colour,
	// whether this task may be edited, and the identity it belongs to.
	Color         string `json:"color,omitempty"`
	CanWrite      bool   `json:"can_write"`
	IdentityEmail string `json:"identity_email,omitempty"`
	CalendarName  string `json:"calendar_name,omitempty"`
}

// HandleDesktopTasks returns the user's tasks.
//
// No time window, unlike the events endpoint: most tasks have no start and many
// have no due date, so windowing would quietly hide the bulk of a reminders
// list. `include_completed=1` adds the finished ones.
func (s *Server) HandleDesktopTasks(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Enabled calendars only, and the gate belongs here: it covers both the
	// explicit-ids case and the no-ids case, so a disabled calendar cannot leak
	// tasks to a client that still remembers its id.
	allCals, err := s.database.GetEnabledCalendarsByUserID(user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load calendars")
		return
	}

	calByID := make(map[int64]*models.Calendar, len(allCals))
	for _, c := range allCals {
		calByID[c.ID] = c
	}

	ids := make([]int64, 0, len(allCals))
	if raw := strings.TrimSpace(r.URL.Query().Get("calendar_ids")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			id, convErr := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			// Filtering against the user's own set is what stops another
			// user's calendar from being read by guessing its id.
			if convErr == nil && calByID[id] != nil {
				ids = append(ids, id)
			}
		}
	} else {
		for id := range calByID {
			ids = append(ids, id)
		}
	}

	if len(ids) == 0 {
		respondJSON(w, http.StatusOK, []DesktopTask{})
		return
	}

	includeCompleted := r.URL.Query().Get("include_completed") == "1"

	todos, err := s.database.GetTodosForCalendars(ids, includeCompleted)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load tasks")
		return
	}

	out := make([]DesktopTask, 0, len(todos))
	for _, t := range todos {
		task := DesktopTask{
			ID:              t.ID,
			CalendarID:      t.CalendarID,
			UID:             t.UID,
			Summary:         t.Summary,
			Description:     t.Description,
			Due:             t.Due,
			AllDay:          t.AllDay,
			Status:          t.Status,
			Completed:       t.IsCompleted(),
			CompletedAt:     t.CompletedAt,
			PercentComplete: t.PercentComplete,
			Priority:        t.Priority,
			RRule:           t.RRule,
		}
		// DTSTART is stored as a plain int64 and zero means "absent" for a
		// task, where it would mean the epoch for an event.
		if t.DTStart != 0 {
			start := t.DTStart
			task.Start = &start
		}
		if cal := calByID[t.CalendarID]; cal != nil {
			task.Color = cal.Color
			task.CanWrite = cal.CanWrite
			task.IdentityEmail = cal.IdentityEmail
			task.CalendarName = cal.Name
		}
		out = append(out, task)
	}

	respondJSON(w, http.StatusOK, out)
}

// TaskCompletionRequest is the body of the completion toggle.
type TaskCompletionRequest struct {
	// Completed is a pointer so "not specified" can mean "flip it", which is
	// what a checkbox wants, while an explicit value stays authoritative for
	// callers that know the state they intend.
	Completed *bool `json:"completed"`
}

// HandleDesktopTaskCompletion marks a task done or not done.
//
// Its own endpoint rather than a field on the event PATCH: that handler is
// shaped around events — recurrence scope, DTSTART/DTEND — and completion has
// no meaning there. Mixing them would put a `completed` field on every event
// edit that no event can honour.
func (s *Server) HandleDesktopTaskCompletion(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	taskID, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad task id")
		return
	}

	var req TaskCompletionRequest
	// An empty body is legitimate: it means "toggle".
	_ = json.NewDecoder(r.Body).Decode(&req)

	task, err := s.database.GetEventByID(taskID)
	if err != nil {
		respondError(w, http.StatusNotFound, "task not found")
		return
	}
	if !task.IsTodo() {
		respondError(w, http.StatusBadRequest, "not a task")
		return
	}

	cal, err := s.database.GetCalendarByID(task.CalendarID)
	if err != nil || cal.UserID != user.ID {
		respondError(w, http.StatusNotFound, "calendar not found")
		return
	}
	if !cal.CanWrite || cal.SourceType == "ics_url" || cal.SourceType == "ics_import" {
		respondError(w, http.StatusBadRequest, "calendar is read-only")
		return
	}

	completed := !task.IsCompleted()
	if req.Completed != nil {
		completed = *req.Completed
	}

	now := timeutil.Now()
	task.ICalData = caldavutil.SetTodoCompletion(task.ICalData, completed, now)
	if completed {
		task.Status = "COMPLETED"
		task.CompletedAt = &now
		full := int16(100)
		task.PercentComplete = &full
	} else {
		task.Status = "NEEDS-ACTION"
		task.CompletedAt = nil
		task.PercentComplete = nil
	}
	// The reverse-sync worker skips rows it has already pushed; this is what
	// tells it there is something new to send.
	task.LocalModified = true

	if err := s.database.UpdateCalendarEvent(task); err != nil {
		log.Printf("task completion: failed to update task %d: %v", taskID, err)
		respondError(w, http.StatusInternalServerError, "failed to update task")
		return
	}

	s.queueEventReverseSync(cal, task, "update")

	log.Printf("task completion: task %d (%s) → completed=%t", taskID, task.UID, completed)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"id":        task.ID,
		"completed": completed,
		"status":    task.Status,
	})
}
