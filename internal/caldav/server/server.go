package server

import (
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/emersion/go-ical"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"golang.org/x/crypto/bcrypt"
)

// Server is a CalDAV server
type Server struct {
	database *db.DB
	prefix   string
}

// New creates a new CalDAV server
func New(database *db.DB, prefix string) *Server {
	return &Server{
		database: database,
		prefix:   prefix,
	}
}

// ServeHTTP handles CalDAV requests
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Authenticate user
	user, err := s.authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="CalDAV"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Log request
	log.Printf("CalDAV %s %s (user: %s)", r.Method, r.URL.Path, user.Username)

	// Route based on method
	switch r.Method {
	case "OPTIONS":
		s.handleOptions(w, r)
	case "PROPFIND":
		s.handlePropfind(w, r, user)
	case "REPORT":
		s.handleReport(w, r, user)
	case "GET":
		s.handleGet(w, r, user)
	case "PUT":
		s.handlePut(w, r, user)
	case "DELETE":
		s.handleDelete(w, r, user)
	case "MKCALENDAR":
		s.handleMkcalendar(w, r, user)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// authenticate authenticates the user from Basic Auth
func (s *Server) authenticate(r *http.Request) (*models.User, error) {
	username, password, ok := r.BasicAuth()
	if !ok {
		return nil, fmt.Errorf("no credentials")
	}

	user, err := s.database.GetUserByUsername(username)
	if err != nil {
		return nil, fmt.Errorf("user not found")
	}

	// Check password
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, fmt.Errorf("invalid password")
	}

	return user, nil
}

func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "OPTIONS, GET, PUT, DELETE, PROPFIND, REPORT, MKCALENDAR")
	w.Header().Set("DAV", "1, 2, calendar-access")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handlePropfind(w http.ResponseWriter, r *http.Request, user *models.User) {
	path := strings.TrimPrefix(r.URL.Path, s.prefix)
	path = strings.TrimSuffix(path, "/")

	// Parse depth header
	depth := r.Header.Get("Depth")
	if depth == "" {
		depth = "1"
	}

	// Determine what we're querying
	parts := strings.Split(path, "/")

	var response string
	if path == "" || path == fmt.Sprintf("%d", user.ID) {
		// Principal URL
		response = s.propfindPrincipal(user, depth)
	} else if len(parts) == 2 && parts[1] == "calendars" {
		// Calendar home set
		response = s.propfindCalendarHome(user, depth)
	} else if len(parts) == 3 && parts[1] == "calendars" {
		// Specific calendar
		calID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			http.Error(w, "Invalid calendar ID", http.StatusBadRequest)
			return
		}
		response = s.propfindCalendar(user, calID, depth)
	} else {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	w.Write([]byte(response))
}

func (s *Server) propfindPrincipal(user *models.User, depth string) string {
	principalURL := fmt.Sprintf("%s%d/", s.prefix, user.ID)
	calendarHomeURL := fmt.Sprintf("%s%d/calendars/", s.prefix, user.ID)

	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype><D:principal/></D:resourcetype>
        <D:displayname>%s</D:displayname>
        <C:calendar-home-set><D:href>%s</D:href></C:calendar-home-set>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`, principalURL, user.Username, calendarHomeURL)
}

func (s *Server) propfindCalendarHome(user *models.User, depth string) string {
	calendarHomeURL := fmt.Sprintf("%s%d/calendars/", s.prefix, user.ID)

	calendars, err := s.database.GetCalendarsByUserID(user.ID)
	if err != nil {
		log.Printf("Failed to get calendars: %v", err)
		calendars = []*models.Calendar{}
	}

	var calendarResponses strings.Builder
	for _, cal := range calendars {
		calURL := fmt.Sprintf("%s%d/calendars/%d/", s.prefix, user.ID, cal.ID)
		calendarResponses.WriteString(fmt.Sprintf(`
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype><D:collection/><C:calendar/></D:resourcetype>
        <D:displayname>%s</D:displayname>
        <C:supported-calendar-component-set>
          <C:comp name="VEVENT"/>
        </C:supported-calendar-component-set>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>`, calURL, xmlEscape(cal.Name)))
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype><D:collection/></D:resourcetype>
        <D:displayname>Calendars</D:displayname>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>%s
</D:multistatus>`, calendarHomeURL, calendarResponses.String())
}

func (s *Server) propfindCalendar(user *models.User, calID int64, depth string) string {
	cal, err := s.database.GetCalendarByID(calID)
	if err != nil || cal.UserID != user.ID {
		return `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:"><D:response><D:status>HTTP/1.1 404 Not Found</D:status></D:response></D:multistatus>`
	}

	calURL := fmt.Sprintf("%s%d/calendars/%d/", s.prefix, user.ID, cal.ID)

	var eventResponses strings.Builder
	if depth != "0" {
		events, _ := s.database.GetEventsByCalendarID(calID)
		for _, event := range events {
			eventURL := fmt.Sprintf("%s%d/calendars/%d/%s.ics", s.prefix, user.ID, cal.ID, event.UID)
			eventResponses.WriteString(fmt.Sprintf(`
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:getetag>%s</D:getetag>
        <D:getcontenttype>text/calendar; charset=utf-8</D:getcontenttype>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>`, eventURL, event.ETag))
		}
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype><D:collection/><C:calendar/></D:resourcetype>
        <D:displayname>%s</D:displayname>
        <C:supported-calendar-component-set>
          <C:comp name="VEVENT"/>
        </C:supported-calendar-component-set>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>%s
</D:multistatus>`, calURL, xmlEscape(cal.Name), eventResponses.String())
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request, user *models.User) {
	path := strings.TrimPrefix(r.URL.Path, s.prefix)
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")

	if len(parts) < 3 || parts[1] != "calendars" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	calID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.Error(w, "Invalid calendar ID", http.StatusBadRequest)
		return
	}

	cal, err := s.database.GetCalendarByID(calID)
	if err != nil || cal.UserID != user.ID {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	// Get all events for the calendar
	events, err := s.database.GetEventsByCalendarID(calID)
	if err != nil {
		http.Error(w, "Failed to get events", http.StatusInternalServerError)
		return
	}

	var responses strings.Builder
	for _, event := range events {
		eventURL := fmt.Sprintf("%s%d/calendars/%d/%s.ics", s.prefix, user.ID, cal.ID, event.UID)
		responses.WriteString(fmt.Sprintf(`
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:getetag>%s</D:getetag>
        <C:calendar-data>%s</C:calendar-data>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>`, eventURL, event.ETag, xmlEscape(event.ICalData)))
	}

	response := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">%s
</D:multistatus>`, responses.String())

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	w.Write([]byte(response))
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, user *models.User) {
	path := strings.TrimPrefix(r.URL.Path, s.prefix)
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")

	if len(parts) < 4 || parts[1] != "calendars" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	calID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.Error(w, "Invalid calendar ID", http.StatusBadRequest)
		return
	}

	cal, err := s.database.GetCalendarByID(calID)
	if err != nil || cal.UserID != user.ID {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	// Extract UID from filename
	filename := parts[3]
	if !strings.HasSuffix(filename, ".ics") {
		http.Error(w, "Invalid event path", http.StatusBadRequest)
		return
	}
	uid := strings.TrimSuffix(filename, ".ics")

	event, err := s.database.GetEventByUID(calID, uid)
	if err != nil || event == nil {
		http.Error(w, "Event not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("ETag", event.ETag)
	w.Write([]byte(event.ICalData))
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, user *models.User) {
	path := strings.TrimPrefix(r.URL.Path, s.prefix)
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")

	if len(parts) < 4 || parts[1] != "calendars" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	calID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.Error(w, "Invalid calendar ID", http.StatusBadRequest)
		return
	}

	cal, err := s.database.GetCalendarByID(calID)
	if err != nil || cal.UserID != user.ID {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	if !cal.CanWrite {
		http.Error(w, "Calendar is read-only", http.StatusForbidden)
		return
	}

	// Extract UID from filename
	filename := parts[3]
	if !strings.HasSuffix(filename, ".ics") {
		http.Error(w, "Invalid event path", http.StatusBadRequest)
		return
	}
	uid := strings.TrimSuffix(filename, ".ics")

	// Read body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	// Parse iCal data
	decoder := ical.NewDecoder(strings.NewReader(string(body)))
	icalCal, err := decoder.Decode()
	if err != nil {
		http.Error(w, "Invalid iCal data", http.StatusBadRequest)
		return
	}

	// Extract event data
	event := &models.CalendarEvent{
		CalendarID:    calID,
		UID:           uid,
		ICalData:      string(body),
		LocalModified: true,
	}

	for _, vevent := range icalCal.Events() {
		if prop := vevent.Props.Get(ical.PropUID); prop != nil && event.UID == "" {
			event.UID = prop.Value
		}
		if prop := vevent.Props.Get(ical.PropSummary); prop != nil {
			event.Summary = prop.Value
		}
		if prop := vevent.Props.Get(ical.PropDescription); prop != nil {
			event.Description = prop.Value
		}
		if prop := vevent.Props.Get(ical.PropLocation); prop != nil {
			event.Location = prop.Value
		}
		if prop := vevent.Props.Get(ical.PropRecurrenceRule); prop != nil {
			event.RRule = prop.Value
		}
		if prop := vevent.Props.Get(ical.PropDateTimeStart); prop != nil {
			if t, err := prop.DateTime(nil); err == nil {
				event.DTStart = t
			}
			if prop.Params.Get(ical.ParamValue) == "DATE" {
				event.AllDay = true
			}
		}
		if prop := vevent.Props.Get(ical.PropDateTimeEnd); prop != nil {
			if t, err := prop.DateTime(nil); err == nil {
				event.DTEnd.Time = t
				event.DTEnd.Valid = true
			}
		}
		break
	}

	// Generate ETag
	event.ETag = generateETag(event.ICalData)

	// Check If-Match header for optimistic locking
	ifMatch := r.Header.Get("If-Match")

	// Check if event exists
	existing, err := s.database.GetEventByUID(calID, uid)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	if existing != nil {
		// Check ETag
		if ifMatch != "" && ifMatch != existing.ETag {
			http.Error(w, "Precondition failed", http.StatusPreconditionFailed)
			return
		}

		// Update existing event
		existing.ICalData = event.ICalData
		existing.Summary = event.Summary
		existing.Description = event.Description
		existing.Location = event.Location
		existing.DTStart = event.DTStart
		existing.DTEnd = event.DTEnd
		existing.AllDay = event.AllDay
		existing.RRule = event.RRule
		existing.ETag = event.ETag
		existing.LocalModified = true

		if err := s.database.UpdateCalendarEvent(existing); err != nil {
			http.Error(w, "Failed to update event", http.StatusInternalServerError)
			return
		}

		w.Header().Set("ETag", event.ETag)
		w.WriteHeader(http.StatusNoContent)
	} else {
		// Check If-None-Match for new resources
		if r.Header.Get("If-None-Match") == "*" {
			// Client expects the resource to not exist - OK to create
		}

		// Create new event
		if err := s.database.CreateCalendarEvent(event); err != nil {
			http.Error(w, "Failed to create event", http.StatusInternalServerError)
			return
		}

		w.Header().Set("ETag", event.ETag)
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, user *models.User) {
	path := strings.TrimPrefix(r.URL.Path, s.prefix)
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")

	if len(parts) < 4 || parts[1] != "calendars" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	calID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.Error(w, "Invalid calendar ID", http.StatusBadRequest)
		return
	}

	cal, err := s.database.GetCalendarByID(calID)
	if err != nil || cal.UserID != user.ID {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	if !cal.CanWrite {
		http.Error(w, "Calendar is read-only", http.StatusForbidden)
		return
	}

	// Extract UID from filename
	filename := parts[3]
	if !strings.HasSuffix(filename, ".ics") {
		http.Error(w, "Invalid event path", http.StatusBadRequest)
		return
	}
	uid := strings.TrimSuffix(filename, ".ics")

	if err := s.database.DeleteCalendarEventByUID(calID, uid); err != nil {
		http.Error(w, "Failed to delete event", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMkcalendar(w http.ResponseWriter, r *http.Request, user *models.User) {
	path := strings.TrimPrefix(r.URL.Path, s.prefix)
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")

	if len(parts) < 3 || parts[1] != "calendars" {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	calendarName := parts[2]

	// Create a local source for this calendar
	source := &models.CalendarSource{
		UserID:       user.ID,
		Name:         calendarName,
		SourceType:   "local",
		SyncEnabled:  false,
		SyncInterval: 0,
		Color:        "#3788d8",
	}

	if err := s.database.CreateCalendarSource(source); err != nil {
		http.Error(w, "Failed to create calendar source", http.StatusInternalServerError)
		return
	}

	cal := &models.Calendar{
		SourceID:    source.ID,
		UserID:      user.ID,
		Name:        calendarName,
		Description: "",
		Timezone:    "UTC",
		CanWrite:    true,
		Color:       source.Color,
	}

	if err := s.database.CreateCalendar(cal); err != nil {
		http.Error(w, "Failed to create calendar", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func generateETag(content string) string {
	hash := sha256.Sum256([]byte(content))
	return fmt.Sprintf("\"%x\"", hash[:8])
}

func xmlEscape(s string) string {
	var buf strings.Builder
	xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
