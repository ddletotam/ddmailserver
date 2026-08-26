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

	"github.com/yourusername/mailserver/internal/caldav/importer"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// Server is a CalDAV server
type Server struct {
	database       *db.DB
	prefix         string
	subdomainHosts map[string]bool // hosts where CalDAV is at root (nginx proxies / → /caldav/)
}

// New creates a new CalDAV server
func New(database *db.DB, prefix string) *Server {
	return &Server{
		database: database,
		prefix:   prefix,
		subdomainHosts: map[string]bool{
			"caldav.letotam.ru": true,
		},
	}
}

// effectivePrefix returns "/" for subdomain hosts (where nginx maps / → /caldav/),
// or the configured prefix for direct access (e.g. mail.letotam.ru/caldav/)
func (s *Server) effectivePrefix(r *http.Request) string {
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	// Strip port if present
	if i := strings.Index(host, ":"); i != -1 {
		host = host[:i]
	}
	if s.subdomainHosts[host] {
		return "/"
	}
	return s.prefix
}

// ServeHTTP handles CalDAV requests
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Set DAV header on all responses - required by iOS
	w.Header().Set("DAV", "1, 3, access-control, calendar-access")

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

	// Accepts the account password or an application password — a device
	// profile carries the latter, so that a leaked profile costs one revoke
	// instead of an account-wide password change.
	user, err := s.database.AuthenticateProtocol(username, password)
	if err != nil {
		return nil, fmt.Errorf("invalid credentials")
	}

	return user, nil
}

func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	log.Printf("OPTIONS %s headers: %v", r.URL.Path, r.Header)
	w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, REPORT, MKCALENDAR")
	w.Header().Set("DAV", "1, 2, 3, access-control, calendar-access")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handlePropfind(w http.ResponseWriter, r *http.Request, user *models.User) {
	path := strings.TrimPrefix(r.URL.Path, s.prefix)
	path = strings.TrimSuffix(path, "/")

	// Log request body for debugging
	body, _ := io.ReadAll(r.Body)
	if len(body) > 0 {
		log.Printf("PROPFIND %s body: %s", r.URL.Path, string(body))
	}

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

	pfx := s.effectivePrefix(r)

	switch {
	case path == "":
		// Root - return current-user-principal (step 1)
		response = s.propfindRoot(user, pfx)

	case path == "principals" || path == "principals/users" ||
		path == fmt.Sprintf("principals/users/%s", user.Username) ||
		path == fmt.Sprintf("calendar/dav/%s/user", user.Username):
		// Principal URL - return calendar-home-set (step 2)
		response = s.propfindPrincipal(user, pfx)

	case path == fmt.Sprintf("calendars/%s", user.Username):
		// Calendar home - return list of calendars (step 3)
		response = s.propfindCalendarHome(user, depth, pfx)

	case strings.HasPrefix(path, fmt.Sprintf("calendars/%s/", user.Username)):
		// Specific calendar
		rest := strings.TrimPrefix(path, fmt.Sprintf("calendars/%s/", user.Username))
		parts := strings.Split(rest, "/")
		calID, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			http.Error(w, "Invalid calendar ID", http.StatusBadRequest)
			return
		}
		response = s.propfindCalendar(user, calID, depth, pfx)

	default:
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	w.Write([]byte(response))
}

// propfindRoot handles PROPFIND on / - returns current-user-principal and calendar-home-set
func (s *Server) propfindRoot(user *models.User, prefix string) string {
	principalURL := fmt.Sprintf("%sprincipals/users/%s/", prefix, user.Username)
	calendarHomeURL := fmt.Sprintf("%scalendars/%s/", prefix, user.Username)

	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:current-user-principal>
          <D:href>%s</D:href>
        </D:current-user-principal>
        <D:principal-URL>
          <D:href>%s</D:href>
        </D:principal-URL>
        <D:resourcetype>
          <D:collection/>
        </D:resourcetype>
        <C:calendar-home-set>
          <D:href>%s</D:href>
        </C:calendar-home-set>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`, prefix, principalURL, principalURL, calendarHomeURL)
}

// propfindPrincipal handles PROPFIND on principal URL - returns calendar-home-set
func (s *Server) propfindPrincipal(user *models.User, prefix string) string {
	principalURL := fmt.Sprintf("%sprincipals/users/%s/", prefix, user.Username)
	calendarHomeURL := fmt.Sprintf("%scalendars/%s/", prefix, user.Username)

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
        <D:principal-URL>
          <D:href>%s</D:href>
        </D:principal-URL>
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
</D:multistatus>`, principalURL, principalURL, principalURL, user.Username, calendarHomeURL, userEmail, principalURL)
}

func (s *Server) propfindCalendarHome(user *models.User, depth string, prefix string) string {
	principalURL := fmt.Sprintf("%sprincipals/users/%s/", prefix, user.Username)
	calendarHomeURL := fmt.Sprintf("%scalendars/%s/", prefix, user.Username)

	calendars, err := s.database.GetEnabledCalendarsByUserID(user.ID)
	if err != nil {
		log.Printf("Failed to get calendars: %v", err)
		calendars = []*models.Calendar{}
	}

	var calendarResponses strings.Builder
	if depth != "0" {
		for _, cal := range calendars {
			calURL := fmt.Sprintf("%scalendars/%s/%d/", prefix, user.Username, cal.ID)
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
%s
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>`, calURL, xmlEscape(cal.Name), supportedComponentSetXML(cal)))
		}
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
        </D:resourcetype>
        <D:displayname>Calendars</D:displayname>
        <D:owner>
          <D:href>%s</D:href>
        </D:owner>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>%s
</D:multistatus>`, calendarHomeURL, principalURL, principalURL, calendarResponses.String())
}

func (s *Server) propfindCalendar(user *models.User, calID int64, depth string, prefix string) string {
	cal, err := s.database.GetCalendarByID(calID)
	if err != nil || cal.UserID != user.ID {
		return `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:"><D:response><D:status>HTTP/1.1 404 Not Found</D:status></D:response></D:multistatus>`
	}

	calURL := fmt.Sprintf("%scalendars/%s/%d/", prefix, user.Username, cal.ID)

	var eventResponses strings.Builder
	if depth != "0" {
		events, _ := s.database.GetEventsByCalendarID(calID)
		for _, event := range events {
			eventURL := fmt.Sprintf("%scalendars/%s/%d/%s.ics", prefix, user.Username, cal.ID, event.UID)
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
%s
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>%s
</D:multistatus>`, calURL, xmlEscape(cal.Name), supportedComponentSetXML(cal), eventResponses.String())
}

// reportRequest represents a parsed REPORT request body
type reportRequest struct {
	reportType string // "calendar-multiget", "calendar-query", or ""
	hrefs      []string
	timeRange  *timeRange
	wantData   bool // whether calendar-data was requested in <prop>

	// component is the comp-filter the client asked for: "VEVENT", "VTODO" or
	// "" for no preference. Apple's Calendar and Reminders point at the same
	// collection and tell them apart by exactly this filter — without honouring
	// it, Reminders is handed a pile of meetings and Calendar a pile of tasks.
	component string
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

	// Extract the component filter. The interesting one is nested inside
	// VCALENDAR — `<C:comp-filter name="VCALENDAR"><C:comp-filter name="VTODO">`
	// — so the outer VCALENDAR level is skipped and the first inner name wins.
	for _, prefix := range []string{"<C:comp-filter", "<comp-filter"} {
		remaining := bodyStr
		for {
			idx := strings.Index(remaining, prefix)
			if idx == -1 {
				break
			}
			tagEnd := strings.Index(remaining[idx:], ">")
			if tagEnd == -1 {
				break
			}
			name := strings.ToUpper(extractAttr(remaining[idx:idx+tagEnd], "name"))
			if name == models.ComponentEvent || name == models.ComponentTodo {
				req.component = name
				break
			}
			remaining = remaining[idx+tagEnd:]
		}
		if req.component != "" {
			break
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
	pfx := s.effectivePrefix(r)
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

	// Honour the comp-filter. Apple's Calendar and Reminders both point at the
	// same collection and distinguish themselves by nothing else, so skipping
	// this hands each of them the other's contents.
	if report.component != "" {
		filtered := make([]*models.CalendarEvent, 0, len(events))
		for _, event := range events {
			if event.ComponentOrDefault() == report.component {
				filtered = append(filtered, event)
			}
		}
		log.Printf("REPORT comp-filter=%s kept %d of %d on calendar %d",
			report.component, len(filtered), len(events), calID)
		events = filtered
	}

	var responses strings.Builder
	for _, event := range events {
		eventURL := fmt.Sprintf("%scalendars/%s/%d/%s.ics", pfx, user.Username, cal.ID, event.UID)
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
	if event.DTStart == 0 {
		return true
	}

	eventEnd := event.DTStart // default: instant event (ms)
	if event.DTEnd != nil && *event.DTEnd != 0 {
		eventEnd = *event.DTEnd
	} else if event.AllDay {
		eventEnd = event.DTStart + 24*60*60*1000 // +1 day in ms
	}

	// Recurring events: always include (proper RRULE expansion is complex)
	if event.RRule != "" {
		return true
	}

	// Check overlap: event starts before range ends AND event ends after range starts
	trStartMs := timeutil.ToMs(tr.start)
	trEndMs := timeutil.ToMs(tr.end)

	if trEndMs != 0 && event.DTStart > trEndMs {
		return false
	}
	if trStartMs != 0 && eventEnd < trStartMs {
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

	// Parse through the shared importer rather than a second hand-rolled loop.
	//
	// This handler used to walk icalCal.Events() itself, which meant a copy of
	// the property extraction that drifted from the importer's — and, more to
	// the point, was blind to VTODO: a task PUT here produced a row with no
	// summary and no due date, which is how iOS Reminders ended up stored as
	// nameless events.
	parsed, err := importer.ParseICS(string(body))
	if err != nil || len(parsed) == 0 {
		http.Error(w, "Invalid iCal data", http.StatusBadRequest)
		return
	}

	event := parsed[0]
	event.CalendarID = calID
	event.LocalModified = true
	// The client's exact bytes are kept, not the importer's re-serialisation:
	// this body is what gets pushed back upstream, and a round trip through
	// our encoder would drop properties we do not model.
	event.ICalData = string(body)
	// The path names the resource; the body's UID is advisory.
	if uid != "" {
		event.UID = uid
	}

	// A collection only accepts what it advertises. Refusing here is the whole
	// point of migrations/048: iOS was filing Reminders into event calendars
	// because we claimed to take them, and every one of those then failed
	// upstream with a 403 we could not explain.
	if !cal.Supports(event.ComponentOrDefault()) {
		log.Printf("CalDAV PUT: cal=%d rejects %s (supports %s)",
			calID, event.ComponentOrDefault(), strings.Join(cal.ComponentList(), ","))
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<D:error xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <C:supported-calendar-component/>
</D:error>`)
		return
	}

	// Generate ETag
	event.ETag = generateETag(event.ICalData)

	// Check If-Match header for optimistic locking
	ifMatch := r.Header.Get("If-Match")

	// Check if event exists
	existing, err := s.database.GetEventByUID(calID, uid)
	if err != nil {
		log.Printf("CalDAV PUT: GetEventByUID error for cal=%d uid=%s: %v", calID, uid, err)
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	log.Printf("CalDAV PUT: cal=%d uid=%s existing=%v", calID, uid, existing != nil)

	if existing != nil {
		// Check ETag
		if ifMatch != "" && ifMatch != existing.ETag {
			log.Printf("CalDAV PUT: ETag mismatch cal=%d uid=%s ifMatch=%s existing.ETag=%s", calID, uid, ifMatch, existing.ETag)
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
		existing.OrganizerEmail = event.OrganizerEmail
		existing.OrganizerName = event.OrganizerName
		existing.LocalModified = true

		if err := s.database.UpdateCalendarEvent(existing); err != nil {
			log.Printf("CalDAV PUT: UpdateCalendarEvent error for cal=%d uid=%s: %v", calID, uid, err)
			http.Error(w, "Failed to update event", http.StatusInternalServerError)
			return
		}
		if err := s.database.ReplaceAttendees(existing.ID, attendeesToPointers(event.Attendees)); err != nil {
			log.Printf("CalDAV PUT: ReplaceAttendees error for event %d: %v", existing.ID, err)
		}
		log.Printf("CalDAV PUT: updated event id=%d cal=%d uid=%s summary=%s", existing.ID, calID, uid, event.Summary)

		// Queue reverse sync for external calendars
		s.queueEventSync(cal, existing.ID, uid, existing.RemoteID, event.ICalData, "update")

		w.Header().Set("ETag", event.ETag)
		w.WriteHeader(http.StatusNoContent)
	} else {
		// Check If-None-Match for new resources
		if r.Header.Get("If-None-Match") == "*" {
			// Client expects the resource to not exist - OK to create
		}

		// Create new event
		if err := s.database.CreateCalendarEvent(event); err != nil {
			log.Printf("CalDAV PUT: CreateCalendarEvent error for cal=%d uid=%s: %v", calID, uid, err)
			http.Error(w, "Failed to create event", http.StatusInternalServerError)
			return
		}
		if err := s.database.ReplaceAttendees(event.ID, attendeesToPointers(event.Attendees)); err != nil {
			log.Printf("CalDAV PUT: ReplaceAttendees error for event %d: %v", event.ID, err)
		}
		log.Printf("CalDAV PUT: created event id=%d cal=%d uid=%s summary=%s", event.ID, calID, uid, event.Summary)

		// Queue reverse sync for external calendars
		s.queueEventSync(cal, event.ID, uid, "", event.ICalData, "create")

		w.Header().Set("ETag", event.ETag)
		w.WriteHeader(http.StatusCreated)
	}
}

func attendeesToPointers(in []models.CalendarAttendee) []*models.CalendarAttendee {
	out := make([]*models.CalendarAttendee, len(in))
	for i := range in {
		out[i] = &in[i]
	}
	return out
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

	// Get event before deletion for reverse sync
	existingEvent, _ := s.database.GetEventByUID(calID, uid)

	// Queue reverse sync BEFORE deletion (FK constraint on event_id)
	if existingEvent != nil {
		s.queueEventSync(cal, existingEvent.ID, uid, existingEvent.RemoteID, "", "delete")
	}

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

// queueEventSync queues an event change for reverse sync to external CalDAV server
func (s *Server) queueEventSync(cal *models.Calendar, eventID int64, uid, remoteID, icalData, operation string) {
	// Only queue for enabled external CalDAV sources with reverse sync enabled
	if cal.SourceType != "caldav" {
		return
	}
	if !cal.Enabled {
		return
	}
	if !cal.ReverseSync {
		return
	}
	if err := s.database.QueueCalendarEventSync(eventID, cal.ID, cal.SourceID, uid, remoteID, icalData, operation); err != nil {
		log.Printf("Failed to queue calendar event sync: %v", err)
	} else {
		log.Printf("queueEventSync: queued eventID=%d for sync to source=%d", eventID, cal.SourceID)
	}
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

// supportedComponentSetXML renders a calendar's supported-calendar-component-set
// from what the collection actually holds.
//
// This used to be hardcoded to "VEVENT and VTODO" for every calendar, and that
// single line is the origin of the whole task saga: iOS read the advertisement,
// filed its Reminders here, and the reverse sync then pushed VTODOs at an
// iCloud event collection that answered 403 to every one of them. A client is
// entitled to believe this property — so it has to be true.
func supportedComponentSetXML(cal *models.Calendar) string {
	var sb strings.Builder
	sb.WriteString("        <C:supported-calendar-component-set>\n")
	for _, comp := range cal.ComponentList() {
		fmt.Fprintf(&sb, "          <C:comp name=%q/>\n", xmlEscape(comp))
	}
	sb.WriteString("        </C:supported-calendar-component-set>")
	return sb.String()
}
