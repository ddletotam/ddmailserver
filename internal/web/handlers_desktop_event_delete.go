package web

import (
	"log"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
)

// HandleDesktopEventDelete removes a calendar event. For v1 only the
// "all" scope is supported — the entire event row (and, for recurring,
// the whole series) is removed. Per-instance / future-only deletes for
// recurring series are tracked alongside their PATCH counterparts and
// share the same scope semantics.
//
// External-calendar (CalDAV) deletes are mirrored upstream via the
// reverse-sync queue so the source server (Yandex, Apple, …) also drops
// the event on its next worker tick.
func (s *Server) HandleDesktopEventDelete(w http.ResponseWriter, r *http.Request) {
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

	event, err := s.database.GetEventByID(eventID)
	if err != nil {
		respondError(w, http.StatusNotFound, "event not found")
		return
	}

	cal, err := s.database.GetCalendarByID(event.CalendarID)
	if err != nil || cal.UserID != user.ID {
		respondError(w, http.StatusNotFound, "event not found")
		return
	}
	if !cal.CanWrite {
		respondError(w, http.StatusForbidden, "calendar is read-only")
		return
	}

	// Queue reverse-sync BEFORE we delete locally. The queue row needs
	// event.ICalData / RemoteID, which only the live row carries; after
	// DELETE we'd lose the upstream pointer and the source server would
	// keep its copy forever.
	s.queueEventReverseSync(cal, event, "delete")

	// Drop the fake email ID before cascade-delete wipes it — we still
	// need to invalidate the search index entry.
	var fakeEmailID int64
	if fakeMsg, err := s.database.GetFakeEmailForEvent(eventID); err == nil && fakeMsg != nil {
		fakeEmailID = fakeMsg.ID
	}

	if err := s.database.DeleteCalendarEvent(eventID); err != nil {
		log.Printf("desktop event delete: %v", err)
		respondError(w, http.StatusInternalServerError, "delete failed")
		return
	}

	if s.searchIndexer != nil && fakeEmailID > 0 {
		go func() {
			if err := s.searchIndexer.DeleteMessage(fakeEmailID); err != nil {
				log.Printf("delete fake email from search: %v", err)
			}
		}()
	}

	w.WriteHeader(http.StatusNoContent)
}
