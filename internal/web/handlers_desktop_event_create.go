package web

import (
	"encoding/json"
	"net/http"

	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// EventCreateRequest is the body for POST /api/desktop/v1/events. The minimum
// useful payload is `calendar_id`, `summary` and `dtstart`; everything else is
// optional. Times are ms-since-epoch.
type EventCreateRequest struct {
	CalendarID  int64  `json:"calendar_id"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Location    string `json:"location"`
	DTStart     int64  `json:"dtstart"`
	DTEnd       *int64 `json:"dtend"`
	AllDay      bool   `json:"all_day"`
}

// HandleDesktopEventCreate creates an event on a writable calendar owned by
// the authenticated user, queues it for reverse sync (so CalDAV-backed
// calendars push the addition upstream), and returns the canonical row.
//
// Rejects ics_url / ics_import calendars (`can_write=false`) and any
// calendar that doesn't belong to the user; the desktop UI hides the action
// in those cases, this is the server-side guard.
func (s *Server) HandleDesktopEventCreate(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req EventCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CalendarID == 0 || req.DTStart == 0 {
		respondError(w, http.StatusBadRequest, "calendar_id and dtstart are required")
		return
	}

	cal, err := s.database.GetCalendarByID(req.CalendarID)
	if err != nil || cal == nil || cal.UserID != user.ID {
		respondError(w, http.StatusNotFound, "calendar not found")
		return
	}
	if !cal.CanWrite {
		respondError(w, http.StatusForbidden, "calendar is read-only")
		return
	}

	now := timeutil.Now()
	event := &models.CalendarEvent{
		CalendarID:    cal.ID,
		UID:           generateUID(),
		Summary:       req.Summary,
		Description:   req.Description,
		Location:      req.Location,
		DTStart:       req.DTStart,
		DTEnd:         req.DTEnd,
		AllDay:        req.AllDay,
		Status:        "CONFIRMED",
		LocalModified: true,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	event.ICalData = generateICalData(event)

	if err := s.database.CreateCalendarEvent(event); err != nil {
		respondError(w, http.StatusInternalServerError, "create failed")
		return
	}

	s.queueEventReverseSync(cal, event, "create")

	respondJSON(w, http.StatusCreated, map[string]any{
		"id":          event.ID,
		"calendar_id": event.CalendarID,
		"uid":         event.UID,
		"summary":     event.Summary,
		"dtstart":     event.DTStart,
		"dtend":       event.DTEnd,
		"all_day":     event.AllDay,
	})
}
