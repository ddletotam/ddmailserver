package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/caldav/importer"
	"github.com/yourusername/mailserver/internal/calendar"
	"github.com/yourusername/mailserver/internal/models"
)

// Calendar Sources Handlers

// HandleGetCalendarSources returns all calendar sources for the current user
func (s *Server) HandleGetCalendarSources(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	sources, err := s.database.GetCalendarSourcesByUserID(userID)
	if err != nil {
		log.Printf("Failed to get calendar sources: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sources)
}

// HandleCreateCalendarSource creates a new calendar source
func (s *Server) HandleCreateCalendarSource(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var source models.CalendarSource
	if err := json.NewDecoder(r.Body).Decode(&source); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	source.UserID = userID

	// Validate source type
	if source.SourceType == "" {
		source.SourceType = "local"
	}
	validTypes := map[string]bool{"local": true, "caldav": true, "ics_import": true, "ics_url": true}
	if !validTypes[source.SourceType] {
		http.Error(w, "Invalid source type", http.StatusBadRequest)
		return
	}

	// Validate account_id if provided
	if source.AccountID != nil {
		account, err := s.database.GetAccountByID(*source.AccountID)
		if err != nil || account.UserID != userID {
			http.Error(w, "Invalid account ID", http.StatusBadRequest)
			return
		}
	}

	// Set defaults
	if source.Color == "" {
		source.Color = "#3788d8"
	}
	if source.SyncInterval == 0 {
		source.SyncInterval = 60 // 1 minute - sync as often as possible
	}
	source.SyncEnabled = true

	if err := s.database.CreateCalendarSource(&source); err != nil {
		log.Printf("Failed to create calendar source: %v", err)
		http.Error(w, "Failed to create calendar source", http.StatusInternalServerError)
		return
	}

	// For local sources, create a default calendar
	if source.SourceType == "local" {
		cal := &models.Calendar{
			SourceID:    source.ID,
			UserID:      userID,
			Name:        source.Name,
			Description: "",
			Color:       source.Color,
			Timezone:    "UTC",
			CanWrite:    true,
		}
		if err := s.database.CreateCalendar(cal); err != nil {
			log.Printf("Failed to create default calendar: %v", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(source)
}

// HandleGetCalendarSource returns a single calendar source
func (s *Server) HandleGetCalendarSource(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	sourceID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid source ID", http.StatusBadRequest)
		return
	}

	source, err := s.database.GetCalendarSourceByID(sourceID)
	if err != nil {
		http.Error(w, "Calendar source not found", http.StatusNotFound)
		return
	}

	if source.UserID != userID {
		http.Error(w, "Calendar source not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(source)
}

// HandleUpdateCalendarSource updates a calendar source
func (s *Server) HandleUpdateCalendarSource(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	sourceID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid source ID", http.StatusBadRequest)
		return
	}

	existing, err := s.database.GetCalendarSourceByID(sourceID)
	if err != nil {
		http.Error(w, "Calendar source not found", http.StatusNotFound)
		return
	}

	if existing.UserID != userID {
		http.Error(w, "Calendar source not found", http.StatusNotFound)
		return
	}

	var source models.CalendarSource
	if err := json.NewDecoder(r.Body).Decode(&source); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Validate account_id if provided
	if source.AccountID != nil {
		account, err := s.database.GetAccountByID(*source.AccountID)
		if err != nil || account.UserID != userID {
			http.Error(w, "Invalid account ID", http.StatusBadRequest)
			return
		}
	}

	// Update allowed fields
	existing.Name = source.Name
	existing.CalDAVURL = source.CalDAVURL
	existing.CalDAVUsername = source.CalDAVUsername
	if source.CalDAVPassword != "" {
		existing.CalDAVPassword = source.CalDAVPassword
	}
	existing.SyncEnabled = source.SyncEnabled
	existing.SyncInterval = source.SyncInterval
	existing.Color = source.Color
	existing.AccountID = source.AccountID

	if err := s.database.UpdateCalendarSource(existing); err != nil {
		log.Printf("Failed to update calendar source: %v", err)
		http.Error(w, "Failed to update calendar source", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(existing)
}

// HandleDeleteCalendarSource deletes a calendar source
func (s *Server) HandleDeleteCalendarSource(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	sourceID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid source ID", http.StatusBadRequest)
		return
	}

	source, err := s.database.GetCalendarSourceByID(sourceID)
	if err != nil {
		http.Error(w, "Calendar source not found", http.StatusNotFound)
		return
	}

	if source.UserID != userID {
		http.Error(w, "Calendar source not found", http.StatusNotFound)
		return
	}

	if err := s.database.DeleteCalendarSource(sourceID); err != nil {
		log.Printf("Failed to delete calendar source: %v", err)
		http.Error(w, "Failed to delete calendar source", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// Calendar Handlers

// HandleGetCalendars returns all calendars for the current user
func (s *Server) HandleGetCalendars(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	calendars, err := s.database.GetCalendarsByUserID(userID)
	if err != nil {
		log.Printf("Failed to get calendars: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(calendars)
}

// HandleGetCalendar returns a single calendar
func (s *Server) HandleGetCalendar(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	calID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid calendar ID", http.StatusBadRequest)
		return
	}

	cal, err := s.database.GetCalendarByID(calID)
	if err != nil {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	if cal.UserID != userID {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cal)
}

// HandleUpdateCalendar updates a calendar
func (s *Server) HandleUpdateCalendar(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	calID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid calendar ID", http.StatusBadRequest)
		return
	}

	existing, err := s.database.GetCalendarByID(calID)
	if err != nil {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	if existing.UserID != userID {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	var cal models.Calendar
	if err := json.NewDecoder(r.Body).Decode(&cal); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Update allowed fields
	existing.Name = cal.Name
	existing.Description = cal.Description
	existing.Color = cal.Color
	existing.Timezone = cal.Timezone

	if err := s.database.UpdateCalendar(existing); err != nil {
		log.Printf("Failed to update calendar: %v", err)
		http.Error(w, "Failed to update calendar", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(existing)
}

// HandleDeleteCalendar deletes a calendar (only local calendars)
func (s *Server) HandleDeleteCalendar(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	calID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid calendar ID", http.StatusBadRequest)
		return
	}

	cal, err := s.database.GetCalendarByID(calID)
	if err != nil {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	if cal.UserID != userID {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	// Only allow deleting local calendars
	if cal.SourceType != "local" {
		http.Error(w, "Cannot delete synced calendar. Delete the source instead.", http.StatusBadRequest)
		return
	}

	if err := s.database.DeleteCalendar(calID); err != nil {
		log.Printf("Failed to delete calendar: %v", err)
		http.Error(w, "Failed to delete calendar", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// Event Handlers

// HandleGetEvents returns events for a calendar
func (s *Server) HandleGetEvents(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	calID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid calendar ID", http.StatusBadRequest)
		return
	}

	cal, err := s.database.GetCalendarByID(calID)
	if err != nil {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	if cal.UserID != userID {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	events, err := s.database.GetEventsByCalendarID(calID)
	if err != nil {
		log.Printf("Failed to get events: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}

// HandleCreateEvent creates a new event
func (s *Server) HandleCreateEvent(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	calID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid calendar ID", http.StatusBadRequest)
		return
	}

	cal, err := s.database.GetCalendarByID(calID)
	if err != nil {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	if cal.UserID != userID {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	if !cal.CanWrite {
		http.Error(w, "Calendar is read-only", http.StatusForbidden)
		return
	}

	var event models.CalendarEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	event.CalendarID = calID

	// Generate UID if not provided
	if event.UID == "" {
		event.UID = generateUID()
	}

	// Generate ICalData if not provided
	if event.ICalData == "" {
		event.ICalData = generateICalData(&event)
	}

	event.LocalModified = true

	if err := s.database.CreateCalendarEvent(&event); err != nil {
		log.Printf("Failed to create event: %v", err)
		http.Error(w, "Failed to create event", http.StatusInternalServerError)
		return
	}

	// Create fake email for search indexing
	go func() {
		inbox, err := s.database.GetOrCreateLocalInbox(userID)
		if err != nil {
			log.Printf("Failed to get inbox for fake email: %v", err)
			return
		}
		fakeMsg, err := s.database.CreateFakeEmailForEvent(&event, userID, inbox.ID)
		if err != nil {
			log.Printf("Failed to create fake email for event: %v", err)
			return
		}
		// Index in Meilisearch if available
		if s.searchIndexer != nil {
			if err := s.searchIndexer.IndexMessage(fakeMsg); err != nil {
				log.Printf("Failed to index fake email: %v", err)
			}
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(event)
}

// HandleGetEvent returns a single event
func (s *Server) HandleGetEvent(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	eventID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid event ID", http.StatusBadRequest)
		return
	}

	event, err := s.database.GetEventByID(eventID)
	if err != nil {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	// Verify ownership through calendar
	cal, err := s.database.GetCalendarByID(event.CalendarID)
	if err != nil || cal.UserID != userID {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(event)
}

// HandleUpdateEvent updates an event
func (s *Server) HandleUpdateEvent(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	eventID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid event ID", http.StatusBadRequest)
		return
	}

	existing, err := s.database.GetEventByID(eventID)
	if err != nil {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	// Verify ownership through calendar
	cal, err := s.database.GetCalendarByID(existing.CalendarID)
	if err != nil || cal.UserID != userID {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	if !cal.CanWrite {
		http.Error(w, "Calendar is read-only", http.StatusForbidden)
		return
	}

	var event models.CalendarEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Update allowed fields
	existing.Summary = event.Summary
	existing.Description = event.Description
	existing.Location = event.Location
	existing.DTStart = event.DTStart
	existing.DTEnd = event.DTEnd
	existing.AllDay = event.AllDay
	existing.RRule = event.RRule
	existing.LocalModified = true

	// Regenerate ICalData
	existing.ICalData = generateICalData(existing)

	if err := s.database.UpdateCalendarEvent(existing); err != nil {
		log.Printf("Failed to update event: %v", err)
		http.Error(w, "Failed to update event", http.StatusInternalServerError)
		return
	}

	// Update fake email for search indexing
	go func() {
		if err := s.database.UpdateFakeEmailForEvent(existing); err != nil {
			log.Printf("Failed to update fake email for event: %v", err)
			return
		}
		// Re-index in Meilisearch if available
		if s.searchIndexer != nil {
			fakeMsg, err := s.database.GetFakeEmailForEvent(existing.ID)
			if err != nil {
				log.Printf("Failed to get fake email for re-indexing: %v", err)
				return
			}
			if err := s.searchIndexer.IndexMessage(fakeMsg); err != nil {
				log.Printf("Failed to re-index fake email: %v", err)
			}
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(existing)
}

// HandleDeleteEvent deletes an event
func (s *Server) HandleDeleteEvent(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	eventID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid event ID", http.StatusBadRequest)
		return
	}

	event, err := s.database.GetEventByID(eventID)
	if err != nil {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	// Verify ownership through calendar
	cal, err := s.database.GetCalendarByID(event.CalendarID)
	if err != nil || cal.UserID != userID {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	if !cal.CanWrite {
		http.Error(w, "Calendar is read-only", http.StatusForbidden)
		return
	}

	// Get fake email ID before deleting event (cascade will delete it)
	var fakeEmailID int64
	fakeMsg, err := s.database.GetFakeEmailForEvent(eventID)
	if err == nil && fakeMsg != nil {
		fakeEmailID = fakeMsg.ID
	}

	if err := s.database.DeleteCalendarEvent(eventID); err != nil {
		log.Printf("Failed to delete event: %v", err)
		http.Error(w, "Failed to delete event", http.StatusInternalServerError)
		return
	}

	// Remove from Meilisearch if available
	if s.searchIndexer != nil && fakeEmailID > 0 {
		go func() {
			if err := s.searchIndexer.DeleteMessage(fakeEmailID); err != nil {
				log.Printf("Failed to delete fake email from search index: %v", err)
			}
		}()
	}

	w.WriteHeader(http.StatusNoContent)
}

// Import Handlers

// HandleImportICS imports events from an ICS file
func (s *Server) HandleImportICS(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	calID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid calendar ID", http.StatusBadRequest)
		return
	}

	cal, err := s.database.GetCalendarByID(calID)
	if err != nil {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	if cal.UserID != userID {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	if !cal.CanWrite {
		http.Error(w, "Calendar is read-only", http.StatusForbidden)
		return
	}

	// Read ICS data from request body
	icsData, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Import events
	imported, err := importer.ImportICSSimple(s.database, calID, icsData)
	if err != nil {
		log.Printf("Failed to import ICS: %v", err)
		http.Error(w, "Failed to import ICS", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"imported": imported})
}

// Helper functions

func (s *Server) getUserIDFromContext(r *http.Request) (int64, error) {
	claims := r.Context().Value("claims")
	if claims == nil {
		return 0, http.ErrNoCookie
	}

	c, ok := claims.(*Claims)
	if !ok {
		return 0, http.ErrNoCookie
	}

	return c.UserID, nil
}

func generateUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b) + "@ddmailserver"
}

func generateICalData(event *models.CalendarEvent) string {
	dtstart := event.DTStart.Format("20060102T150405Z")
	var dtend string
	if event.DTEnd.Valid {
		dtend = event.DTEnd.Time.Format("20060102T150405Z")
	} else {
		dtend = event.DTStart.Add(time.Hour).Format("20060102T150405Z")
	}

	ical := "BEGIN:VCALENDAR\r\n"
	ical += "VERSION:2.0\r\n"
	ical += "PRODID:-//DDMailServer//Calendar//EN\r\n"
	ical += "BEGIN:VEVENT\r\n"
	ical += "UID:" + event.UID + "\r\n"
	ical += "SUMMARY:" + event.Summary + "\r\n"
	if event.Description != "" {
		ical += "DESCRIPTION:" + event.Description + "\r\n"
	}
	if event.Location != "" {
		ical += "LOCATION:" + event.Location + "\r\n"
	}
	ical += "DTSTART:" + dtstart + "\r\n"
	ical += "DTEND:" + dtend + "\r\n"
	if event.RRule != "" {
		ical += "RRULE:" + event.RRule + "\r\n"
	}
	ical += "END:VEVENT\r\n"
	ical += "END:VCALENDAR\r\n"

	return ical
}

// Attendee Handlers

// HandleGetEventAttendees returns attendees for an event
func (s *Server) HandleGetEventAttendees(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	eventID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid event ID", http.StatusBadRequest)
		return
	}

	event, err := s.database.GetEventByID(eventID)
	if err != nil {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	// Verify ownership
	cal, err := s.database.GetCalendarByID(event.CalendarID)
	if err != nil || cal.UserID != userID {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	attendees, err := s.database.GetAttendeesByEventID(eventID)
	if err != nil {
		log.Printf("Failed to get attendees: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(attendees)
}

// EventWithAttendeesRequest represents a request to create event with attendees
type EventWithAttendeesRequest struct {
	Event       models.CalendarEvent      `json:"event"`
	Attendees   []models.CalendarAttendee `json:"attendees"`
	SendInvites bool                      `json:"send_invites"`
}

// HandleCreateEventWithAttendees creates an event and optionally sends invites
func (s *Server) HandleCreateEventWithAttendees(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	calID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid calendar ID", http.StatusBadRequest)
		return
	}

	cal, err := s.database.GetCalendarByID(calID)
	if err != nil {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	if cal.UserID != userID {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	if !cal.CanWrite {
		http.Error(w, "Calendar is read-only", http.StatusForbidden)
		return
	}

	var req EventWithAttendeesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	event := &req.Event
	event.CalendarID = calID

	// Generate UID if not provided
	if event.UID == "" {
		event.UID = generateUID()
	}

	// Get source to check for account link
	source, err := s.database.GetCalendarSourceByID(cal.SourceID)
	if err != nil {
		http.Error(w, "Calendar source not found", http.StatusInternalServerError)
		return
	}

	// Set organizer if we have a linked account
	if source.AccountID != nil {
		account, err := s.database.GetAccountByID(*source.AccountID)
		if err == nil {
			event.OrganizerEmail = account.Email
			event.OrganizerName = account.Name
		}
	}

	// Generate ICalData with attendees
	event.ICalData = generateICalData(event)
	event.LocalModified = true

	if err := s.database.CreateCalendarEvent(event); err != nil {
		log.Printf("Failed to create event: %v", err)
		http.Error(w, "Failed to create event", http.StatusInternalServerError)
		return
	}

	// Save attendees
	if len(req.Attendees) > 0 {
		for i := range req.Attendees {
			req.Attendees[i].EventID = event.ID
		}
		if err := s.database.ReplaceAttendees(event.ID, attendeesToPointers(req.Attendees)); err != nil {
			log.Printf("Failed to save attendees: %v", err)
		}
	}

	// Send invites if requested and we have an account
	if req.SendInvites && source.AccountID != nil && len(req.Attendees) > 0 {
		account, err := s.database.GetAccountByID(*source.AccountID)
		if err == nil {
			inviteService := s.getInviteService()
			if err := inviteService.SendInvites(event, req.Attendees, account); err != nil {
				log.Printf("Failed to send invites: %v", err)
			}
		}
	}

	// Load attendees for response
	event.Attendees = req.Attendees

	// Create fake email for search indexing
	go func() {
		inbox, err := s.database.GetOrCreateLocalInbox(userID)
		if err != nil {
			log.Printf("Failed to get inbox for fake email: %v", err)
			return
		}
		fakeMsg, err := s.database.CreateFakeEmailForEvent(event, userID, inbox.ID)
		if err != nil {
			log.Printf("Failed to create fake email for event: %v", err)
			return
		}
		// Index in Meilisearch if available
		if s.searchIndexer != nil {
			if err := s.searchIndexer.IndexMessage(fakeMsg); err != nil {
				log.Printf("Failed to index fake email: %v", err)
			}
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(event)
}

// HandleUpdateEventAttendees updates attendees for an event
func (s *Server) HandleUpdateEventAttendees(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	eventID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid event ID", http.StatusBadRequest)
		return
	}

	event, err := s.database.GetEventByID(eventID)
	if err != nil {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	cal, err := s.database.GetCalendarByID(event.CalendarID)
	if err != nil || cal.UserID != userID {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	if !cal.CanWrite {
		http.Error(w, "Calendar is read-only", http.StatusForbidden)
		return
	}

	var attendees []models.CalendarAttendee
	if err := json.NewDecoder(r.Body).Decode(&attendees); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if err := s.database.ReplaceAttendees(eventID, attendeesToPointers(attendees)); err != nil {
		log.Printf("Failed to update attendees: %v", err)
		http.Error(w, "Failed to update attendees", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(attendees)
}

func attendeesToPointers(attendees []models.CalendarAttendee) []*models.CalendarAttendee {
	result := make([]*models.CalendarAttendee, len(attendees))
	for i := range attendees {
		result[i] = &attendees[i]
	}
	return result
}

func (s *Server) getInviteService() *calendar.InviteService {
	return calendar.NewInviteService(s.database)
}

// Invite Response Handlers

// InviteResponseRequest represents a request to respond to an invite
type InviteResponseRequest struct {
	PartStat  string `json:"partstat"` // ACCEPTED, DECLINED, TENTATIVE
	AccountID int64  `json:"account_id"`
}

// HandleRespondToInvite handles accepting/declining/tentative responses to an invite
func (s *Server) HandleRespondToInvite(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	eventID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid event ID", http.StatusBadRequest)
		return
	}

	var req InviteResponseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Validate partstat
	validPartStats := map[string]bool{"ACCEPTED": true, "DECLINED": true, "TENTATIVE": true}
	if !validPartStats[req.PartStat] {
		http.Error(w, "Invalid partstat. Must be ACCEPTED, DECLINED, or TENTATIVE", http.StatusBadRequest)
		return
	}

	// Get the event
	event, err := s.database.GetEventByID(eventID)
	if err != nil {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	// Verify ownership
	cal, err := s.database.GetCalendarByID(event.CalendarID)
	if err != nil || cal.UserID != userID {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	// Get the account to send reply from
	account, err := s.database.GetAccountByID(req.AccountID)
	if err != nil || account.UserID != userID {
		http.Error(w, "Invalid account", http.StatusBadRequest)
		return
	}

	// Check if the event has an organizer to reply to
	if event.OrganizerEmail == "" {
		http.Error(w, "Event has no organizer to reply to", http.StatusBadRequest)
		return
	}

	// Send the reply
	inviteService := s.getInviteService()
	if err := inviteService.SendReply(event, account.Email, account.Name, req.PartStat, account); err != nil {
		log.Printf("Failed to send reply: %v", err)
		http.Error(w, "Failed to send reply", http.StatusInternalServerError)
		return
	}

	// Update our local attendee record if we're an attendee
	if err := s.database.UpdateAttendeePartStat(eventID, account.Email, req.PartStat); err != nil {
		log.Printf("Failed to update local attendee: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":   "ok",
		"partstat": req.PartStat,
	})
}

// HandleSendInvites sends invites to attendees of an existing event
func (s *Server) HandleSendInvites(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	eventID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid event ID", http.StatusBadRequest)
		return
	}

	event, err := s.database.GetEventByID(eventID)
	if err != nil {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	cal, err := s.database.GetCalendarByID(event.CalendarID)
	if err != nil || cal.UserID != userID {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	source, err := s.database.GetCalendarSourceByID(cal.SourceID)
	if err != nil || source.AccountID == nil {
		http.Error(w, "Calendar not linked to email account", http.StatusBadRequest)
		return
	}

	account, err := s.database.GetAccountByID(*source.AccountID)
	if err != nil {
		http.Error(w, "Account not found", http.StatusBadRequest)
		return
	}

	attendees, err := s.database.GetAttendeesByEventID(eventID)
	if err != nil || len(attendees) == 0 {
		http.Error(w, "No attendees to invite", http.StatusBadRequest)
		return
	}

	// Set organizer if not set
	if event.OrganizerEmail == "" {
		event.OrganizerEmail = account.Email
		event.OrganizerName = account.Name
		s.database.UpdateCalendarEvent(event)
	}

	// Convert to non-pointer slice
	attendeeSlice := make([]models.CalendarAttendee, len(attendees))
	for i, a := range attendees {
		attendeeSlice[i] = *a
	}

	inviteService := s.getInviteService()
	if err := inviteService.SendInvites(event, attendeeSlice, account); err != nil {
		log.Printf("Failed to send invites: %v", err)
		http.Error(w, "Failed to send invites", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// HandleCancelEvent cancels an event and notifies attendees
func (s *Server) HandleCancelEvent(w http.ResponseWriter, r *http.Request) {
	userID, err := s.getUserIDFromContext(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	eventID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid event ID", http.StatusBadRequest)
		return
	}

	event, err := s.database.GetEventByID(eventID)
	if err != nil {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	cal, err := s.database.GetCalendarByID(event.CalendarID)
	if err != nil || cal.UserID != userID {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	if !cal.CanWrite {
		http.Error(w, "Calendar is read-only", http.StatusForbidden)
		return
	}

	source, err := s.database.GetCalendarSourceByID(cal.SourceID)
	if err != nil {
		http.Error(w, "Calendar source not found", http.StatusInternalServerError)
		return
	}

	// Get attendees
	attendees, err := s.database.GetAttendeesByEventID(eventID)
	if err == nil && len(attendees) > 0 && source.AccountID != nil {
		account, err := s.database.GetAccountByID(*source.AccountID)
		if err == nil {
			// Convert to non-pointer slice
			attendeeSlice := make([]models.CalendarAttendee, len(attendees))
			for i, a := range attendees {
				attendeeSlice[i] = *a
			}

			// Send cancel notifications
			inviteService := s.getInviteService()
			if err := inviteService.SendCancel(event, attendeeSlice, account); err != nil {
				log.Printf("Failed to send cancel notifications: %v", err)
			}
		}
	}

	// Update event status to cancelled
	event.Status = "CANCELLED"
	if err := s.database.UpdateCalendarEvent(event); err != nil {
		log.Printf("Failed to update event status: %v", err)
		http.Error(w, "Failed to cancel event", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "cancelled"})
}
