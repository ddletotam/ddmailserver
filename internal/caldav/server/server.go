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
	"time"

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
	w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, REPORT, MKCALENDAR")
	w.Header().Set("DAV", "1, 3, access-control, calendar-access")
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

	var response string

	// iOS CalDAV discovery flow:
	// 1. PROPFIND / → return current-user-principal pointing to /principals/users/username/
	// 2. PROPFIND /principals/users/username/ → return calendar-home-set, calendar-user-address-set
	// 3. PROPFIND /calendars/username/ Depth:1 → return list of calendars

	switch {
	case path == "":
		// Root - return current-user-principal (step 1)
		response = s.propfindRoot(user)

	case path == "principals" || path == "principals/users" ||
		path == fmt.Sprintf("principals/users/%s", user.Username) ||
		path == fmt.Sprintf("calendar/dav/%s/user", user.Username):
		// Principal URL - return calendar-home-set (step 2)
		response = s.propfindPrincipal(user)

	case path == fmt.Sprintf("calendars/%s", user.Username):
		// Calendar home - return list of calendars (step 3)
		response = s.propfindCalendarHome(user, depth)

	case strings.HasPrefix(path, fmt.Sprintf("calendars/%s/", user.Username)):
		// Specific calendar
		rest := strings.TrimPrefix(path, fmt.Sprintf("calendars/%s/", user.Username))
		parts := strings.Split(rest, "/")
		calID, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			http.Error(w, "Invalid calendar ID", http.StatusBadRequest)
			return
		}
		response = s.propfindCalendar(user, calID, depth)

	default:
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	w.Write([]byte(response))
}

// propfindRoot handles PROPFIND on / - returns current-user-principal
func (s *Server) propfindRoot(user *models.User) string {
	principalURL := fmt.Sprintf("%sprincipals/users/%s/", s.prefix, user.Username)

	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:current-user-principal>
          <D:href>%s</D:href>
        </D:current-user-principal>
        <D:resourcetype>
          <D:collection/>
        </D:resourcetype>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`, s.prefix, principalURL)
}

// propfindPrincipal handles PROPFIND on principal URL - returns calendar-home-set
func (s *Server) propfindPrincipal(user *models.User) string {
	principalURL := fmt.Sprintf("%sprincipals/users/%s/", s.prefix, user.Username)
	calendarHomeURL := fmt.Sprintf("%scalendars/%s/", s.prefix, user.Username)

	// Get user email for calendar-user-address-set
	userEmail := user.Username + "@localhost"
	if user.Email != "" {
		userEmail = user.Email
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:current-user-principal>
          <D:href>%s</D:href>
        </D:current-user-principal>
        <D:resourcetype>
          <D:collection/>
          <D:principal/>
        </D:resourcetype>
        <D:displayname>%s</D:displayname>
        <C:calendar-home-set>
          <D:href>%s</D:href>
        </C:calendar-home-set>
        <C:calendar-user-address-set>
          <D:href>mailto:%s</D:href>
          <D:href>%s</D:href>
        </C:calendar-user-address-set>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`, principalURL, principalURL, user.Username, calendarHomeURL, userEmail, principalURL)
}

func (s *Server) propfindCalendarHome(user *models.User, depth string) string {
	calendarHomeURL := fmt.Sprintf("%scalendars/%s/", s.prefix, user.Username)

	calendars, err := s.database.GetCalendarsByUserID(user.ID)
	if err != nil {
		log.Printf("Failed to get calendars: %v", err)
		calendars = []*models.Calendar{}
	}

	var calendarResponses strings.Builder
	if depth != "0" {
		for _, cal := range calendars {
			calURL := fmt.Sprintf("%scalendars/%s/%d/", s.prefix, user.Username, cal.ID)
			calendarResponses.WriteString(fmt.Sprintf(`
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype>
          <D:collection/>
          <C:calendar/>
        </D:resourcetype>
        <D:displayname>%s</D:displayname>
        <C:supported-calendar-component-set>
          <C:comp name="VEVENT"/>
          <C:comp name="VTODO"/>
        </C:supported-calendar-component-set>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>`, calURL, xmlEscape(cal.Name)))
		}
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype>
          <D:collection/>
        </D:resourcetype>
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

	calURL := fmt.Sprintf("%scalendars/%s/%d/", s.prefix, user.Username, cal.ID)

	var eventResponses strings.Builder
	if depth != "0" {
		events, _ := s.database.GetEventsByCalendarID(calID)
		for _, event := range events {
			eventURL := fmt.Sprintf("%scalendars/%s/%d/%s.ics", s.prefix, user.Username, cal.ID, event.UID)
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
        <D:resourcetype>
          <D:collection/>
          <C:calendar/>
        </D:resourcetype>
        <D:displayname>%s</D:displayname>
        <C:supported-calendar-component-set>
          <C:comp name="VEVENT"/>
          <C:comp name="VTODO"/>
        </C:supported-calendar-component-set>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>%s
</D:multistatus>`, calURL, xmlEscape(cal.Name), eventResponses.String())
}

// reportRequest represents a parsed REPORT request body
type reportRequest struct {
	reportType string // "calendar-multiget", "calendar-query", or ""
	hrefs      []string
	timeRange  *timeRange
	wantData   bool // whether calendar-data was requested in <prop>
}

type timeRange struct {
	start time.Time
	end   time.Time
}

func parseReportBody(body []byte) *reportRequest {
	req := &reportRequest{wantData: true} // default: include data

	bodyStr := string(body)

	// Determine report type
	if strings.Contains(bodyStr, "calendar-multiget") {
		req.reportType = "calendar-multiget"
	} else if strings.Contains(bodyStr, "calendar-query") {
		req.reportType = "calendar-query"
	}

	// Extract hrefs for calendar-multiget
	if req.reportType == "calendar-multiget" {
		// Parse <D:href>...</D:href> or <href>...</href> entries
		type hrefDoc struct {
			Hrefs []string `xml:"href"`
		}
		// Simple extraction: find all <href> or <D:href> values
		var doc struct {
			XMLName xml.Name
			Hrefs   []string `xml:"href"`
		}
		// Try to parse, but use regex fallback for namespace issues
		if err := xml.Unmarshal(body, &doc); err == nil && len(doc.Hrefs) > 0 {
			req.hrefs = doc.Hrefs
		}
		// Fallback: extract hrefs with simple string parsing
		if len(req.hrefs) == 0 {
			remaining := bodyStr
			for {
				// Look for <href> or <D:href>
				idx := strings.Index(remaining, "<href>")
				endTag := "</href>"
				if idx == -1 {
					idx = strings.Index(remaining, "<D:href>")
					endTag = "</D:href>"
				}
				if idx == -1 {
					break
				}
				start := idx + len("<href>")
				if strings.HasPrefix(remaining[idx:], "<D:href>") {
					start = idx + len("<D:href>")
				}
				remaining = remaining[start:]
				end := strings.Index(remaining, endTag)
				if end == -1 {
					break
				}
				href := strings.TrimSpace(remaining[:end])
				if href != "" {
					req.hrefs = append(req.hrefs, href)
				}
				remaining = remaining[end+len(endTag):]
			}
		}
	}

	// Extract time-range for calendar-query
	if req.reportType == "calendar-query" {
		// Look for <C:time-range start="..." end="..."/> or <time-range .../>
		for _, prefix := range []string{"<C:time-range", "<time-range"} {
			idx := strings.Index(bodyStr, prefix)
			if idx == -1 {
				continue
			}
			tagEnd := strings.Index(bodyStr[idx:], "/>")
			if tagEnd == -1 {
				tagEnd = strings.Index(bodyStr[idx:], ">")
			}
			if tagEnd == -1 {
				continue
			}
			tag := bodyStr[idx : idx+tagEnd]

			var tr timeRange
			if s := extractAttr(tag, "start"); s != "" {
				if t, err := time.Parse("20060102T150405Z", s); err == nil {
					tr.start = t
				}
			}
			if e := extractAttr(tag, "end"); e != "" {
				if t, err := time.Parse("20060102T150405Z", e); err == nil {
					tr.end = t
				}
			}
			if !tr.start.IsZero() || !tr.end.IsZero() {
				req.timeRange = &tr
			}
			break
		}
	}

	// Check if calendar-data is requested in props
	if !strings.Contains(bodyStr, "calendar-data") {
		req.wantData = false
	}

	return req
}

func extractAttr(tag, attr string) string {
	key := attr + `="`
	idx := strings.Index(tag, key)
	if idx == -1 {
		return ""
	}
	start := idx + len(key)
	end := strings.Index(tag[start:], `"`)
	if end == -1 {
		return ""
	}
	return tag[start : start+end]
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request, user *models.User) {
	path := strings.TrimPrefix(r.URL.Path, s.prefix)
	path = strings.TrimSuffix(path, "/")

	// Expect: calendars/username/calendarID
	if !strings.HasPrefix(path, fmt.Sprintf("calendars/%s/", user.Username)) {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	rest := strings.TrimPrefix(path, fmt.Sprintf("calendars/%s/", user.Username))
	parts := strings.Split(rest, "/")
	if len(parts) < 1 {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	calID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "Invalid calendar ID", http.StatusBadRequest)
		return
	}

	cal, err := s.database.GetCalendarByID(calID)
	if err != nil || cal.UserID != user.ID {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	// Parse REPORT body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	report := parseReportBody(body)
	log.Printf("REPORT type=%s hrefs=%d timeRange=%v wantData=%v on calendar %d",
		report.reportType, len(report.hrefs), report.timeRange != nil, report.wantData, calID)

	var events []*models.CalendarEvent

	if report.reportType == "calendar-multiget" && len(report.hrefs) > 0 {
		// Only fetch requested events by UID extracted from hrefs
		for _, href := range report.hrefs {
			uid := uidFromHref(href)
			if uid == "" {
				continue
			}
			event, err := s.database.GetEventByUID(calID, uid)
			if err != nil || event == nil {
				continue
			}
			events = append(events, event)
		}
	} else {
		// calendar-query or unknown: return all events
		events, err = s.database.GetEventsByCalendarID(calID)
		if err != nil {
			http.Error(w, "Failed to get events", http.StatusInternalServerError)
			return
		}

		// Apply time-range filter if specified
		if report.timeRange != nil {
			filtered := make([]*models.CalendarEvent, 0, len(events))
			for _, event := range events {
				if eventMatchesTimeRange(event, report.timeRange) {
					filtered = append(filtered, event)
				}
			}
			events = filtered
		}
	}

	var responses strings.Builder
	for _, event := range events {
		eventURL := fmt.Sprintf("%scalendars/%s/%d/%s.ics", s.prefix, user.Username, cal.ID, event.UID)
		if report.wantData {
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
		} else {
			responses.WriteString(fmt.Sprintf(`
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:getetag>%s</D:getetag>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>`, eventURL, event.ETag))
		}
	}

	response := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">%s
</D:multistatus>`, responses.String())

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	w.Write([]byte(response))
}

// uidFromHref extracts the UID from a CalDAV href like /caldav/calendars/user/13/some-uid.ics
func uidFromHref(href string) string {
	href = strings.TrimSuffix(href, "/")
	if !strings.HasSuffix(href, ".ics") {
		return ""
	}
	idx := strings.LastIndex(href, "/")
	if idx == -1 {
		return ""
	}
	return strings.TrimSuffix(href[idx+1:], ".ics")
}

// eventMatchesTimeRange checks if an event overlaps with the given time range
func eventMatchesTimeRange(event *models.CalendarEvent, tr *timeRange) bool {
	// If no DTStart, include the event (can't filter)
	if event.DTStart.IsZero() {
		return true
	}

	eventEnd := event.DTStart // default: instant event
	if event.DTEnd.Valid && !event.DTEnd.Time.IsZero() {
		eventEnd = event.DTEnd.Time
	} else if event.AllDay {
		eventEnd = event.DTStart.AddDate(0, 0, 1)
	}

	// Recurring events: always include (proper RRULE expansion is complex)
	if event.RRule != "" {
		return true
	}

	// Check overlap: event starts before range ends AND event ends after range starts
	if !tr.end.IsZero() && event.DTStart.After(tr.end) {
		return false
	}
	if !tr.start.IsZero() && eventEnd.Before(tr.start) {
		return false
	}
	return true
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, user *models.User) {
	path := strings.TrimPrefix(r.URL.Path, s.prefix)
	path = strings.TrimSuffix(path, "/")

	// Expect: calendars/username/calendarID/event.ics
	if !strings.HasPrefix(path, fmt.Sprintf("calendars/%s/", user.Username)) {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	rest := strings.TrimPrefix(path, fmt.Sprintf("calendars/%s/", user.Username))
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	calID, err := strconv.ParseInt(parts[0], 10, 64)
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
	filename := parts[1]
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
	path = strings.TrimSuffix(path, "/")

	// Expect: calendars/username/calendarID/event.ics
	if !strings.HasPrefix(path, fmt.Sprintf("calendars/%s/", user.Username)) {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	rest := strings.TrimPrefix(path, fmt.Sprintf("calendars/%s/", user.Username))
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	calID, err := strconv.ParseInt(parts[0], 10, 64)
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
	filename := parts[1]
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
	path = strings.TrimSuffix(path, "/")

	// Expect: calendars/username/calendarID/event.ics
	if !strings.HasPrefix(path, fmt.Sprintf("calendars/%s/", user.Username)) {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	rest := strings.TrimPrefix(path, fmt.Sprintf("calendars/%s/", user.Username))
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	calID, err := strconv.ParseInt(parts[0], 10, 64)
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
	filename := parts[1]
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
	path = strings.TrimSuffix(path, "/")

	// Expect: calendars/username/newCalendarName
	if !strings.HasPrefix(path, fmt.Sprintf("calendars/%s/", user.Username)) {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	rest := strings.TrimPrefix(path, fmt.Sprintf("calendars/%s/", user.Username))
	if rest == "" {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	calendarName := rest

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
