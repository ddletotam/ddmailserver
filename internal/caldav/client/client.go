package client

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"
	caldavutil "github.com/yourusername/mailserver/internal/caldav"
	"github.com/yourusername/mailserver/internal/caldav/importer"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// Client is a CalDAV client for syncing with external calendars
type Client struct {
	source     *models.CalendarSource
	database   *db.DB
	client     *caldav.Client
	httpClient webdav.HTTPClient
}

// New creates a new CalDAV client
func New(source *models.CalendarSource, database *db.DB) *Client {
	return &Client{
		source:   source,
		database: database,
	}
}

// buildFullURL constructs a full URL from the source's CalDAV URL and a remote path
func (c *Client) buildFullURL(remotePath string) string {
	// If remotePath is already a full URL, return it as-is
	if strings.HasPrefix(remotePath, "http://") || strings.HasPrefix(remotePath, "https://") {
		return remotePath
	}

	// Parse the source's CalDAV URL to get the base (scheme + host)
	parsed, err := url.Parse(c.source.CalDAVURL)
	if err != nil {
		// Fallback: just return the path (will fail, but with a clearer error)
		return remotePath
	}

	// Construct full URL: scheme://host + remotePath
	return fmt.Sprintf("%s://%s%s", parsed.Scheme, parsed.Host, remotePath)
}

// Connect establishes a connection to the CalDAV server
func (c *Client) Connect() error {
	var client *caldav.Client
	var err error

	var httpClient *http.Client
	if c.source.AuthType == "password" {
		// Create HTTP client with ETag fix transport, then add basic auth
		httpClient = &http.Client{
			Transport: &etagFixTransport{base: http.DefaultTransport},
			Timeout:   30 * time.Second,
		}
		authClient := webdav.HTTPClientWithBasicAuth(httpClient, c.source.CalDAVUsername, c.source.CalDAVPassword)
		client, err = caldav.NewClient(authClient, c.source.CalDAVURL)
		// Also store the auth client for fallback requests
		c.httpClient = authClient
	} else if c.source.AuthType == "oauth2_google" || c.source.AuthType == "oauth2_microsoft" {
		// Use OAuth2 bearer token, also wrapped with ETag fix
		transport := &etagFixTransport{
			base: &oauthTransport{
				base:  http.DefaultTransport,
				token: c.source.OAuthAccessToken,
			},
		}
		httpClient = &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		}
		client, err = caldav.NewClient(httpClient, c.source.CalDAVURL)
		c.httpClient = httpClient
	} else {
		return fmt.Errorf("unsupported auth type: %s", c.source.AuthType)
	}

	if err != nil {
		return fmt.Errorf("failed to create CalDAV client: %w", err)
	}

	c.client = client
	return nil
}

// DiscoverCalendars discovers available calendars from the server
func (c *Client) DiscoverCalendars(ctx context.Context) ([]*models.Calendar, error) {
	if c.client == nil {
		return nil, fmt.Errorf("client not connected")
	}

	// Google Calendar doesn't support standard CalDAV discovery
	// Create a calendar entry directly using the known URL structure
	if c.source.AuthType == "oauth2_google" {
		return c.discoverGoogleCalendars(ctx)
	}

	// Try standard CalDAV discovery first
	// If it fails, fall back to using the URL as a direct calendar
	principal, err := c.client.FindCurrentUserPrincipal(ctx)
	if err != nil {
		// Discovery failed - server doesn't support it or URL points directly to a calendar
		log.Printf("CalDAV discovery failed for %s: %v - using URL as direct calendar", c.source.Name, err)
		return c.useDirectCalendarURL(ctx)
	}

	// Find calendar home set
	homeSet, err := c.client.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("failed to find calendar home set: %w", err)
	}

	// Find all calendars
	calendars, err := c.client.FindCalendars(ctx, homeSet)
	if err != nil {
		return nil, fmt.Errorf("failed to find calendars: %w", err)
	}

	var result []*models.Calendar
	for _, cal := range calendars {
		// Task lists used to be dropped here — "skip if not a calendar" — which
		// is half of why iOS Reminders had nowhere to go: Apple keeps them in
		// their own VTODO-only collections, we discovered those and threw them
		// away, and the reminders ended up filed against an event calendar
		// instead. They are collections like any other now; what differs is
		// what each one accepts.
		components := remoteComponentSet(cal)

		calendar := &models.Calendar{
			SourceID:            c.source.ID,
			UserID:              c.source.UserID,
			RemoteID:            cal.Path,
			Name:                cal.Name,
			Description:         cal.Description,
			Timezone:            "UTC",
			CanWrite:            true,
			Color:               c.source.Color, // Use source color
			SupportedComponents: components,
		}

		log.Printf("CalDAV discovery: calendar %q (%s) accepts %s", cal.Name, cal.Path, components)
		result = append(result, calendar)
	}

	return result, nil
}

// remoteComponentSet renders what a discovered collection accepts, as the
// comma-separated form stored on calendars.supported_components.
//
// Only VEVENT and VTODO are kept: those are the two this server can represent,
// and recording a VJOURNAL we cannot store would put us right back to
// advertising capabilities that do not exist. A collection that declares
// nothing is read as events only — the conservative answer, and what every
// calendar predating tasks effectively was.
func remoteComponentSet(cal caldav.Calendar) string {
	var out []string
	for _, comp := range cal.SupportedComponentSet {
		switch strings.ToUpper(strings.TrimSpace(comp)) {
		case models.ComponentEvent:
			out = append(out, models.ComponentEvent)
		case models.ComponentTodo:
			out = append(out, models.ComponentTodo)
		}
	}
	if len(out) == 0 {
		return models.ComponentEvent
	}
	return strings.Join(out, ",")
}

// useDirectCalendarURL creates a calendar entry from a direct calendar URL (skip discovery)
// Used for Yandex and other providers where user provides a specific calendar URL
func (c *Client) useDirectCalendarURL(ctx context.Context) ([]*models.Calendar, error) {
	log.Printf("Using direct calendar URL (skipping discovery): %s", c.source.CalDAVURL)

	calendar := &models.Calendar{
		SourceID:    c.source.ID,
		UserID:      c.source.UserID,
		RemoteID:    c.source.CalDAVURL, // Use the full URL as remote ID
		Name:        c.source.Name,
		Description: "Direct CalDAV Calendar",
		Timezone:    "UTC",
		CanWrite:    true,
		Color:       c.source.Color,
	}

	return []*models.Calendar{calendar}, nil
}

// discoverGoogleCalendars handles Google Calendar's non-standard CalDAV
func (c *Client) discoverGoogleCalendars(ctx context.Context) ([]*models.Calendar, error) {
	// Google CalDAV URL format: https://apidata.googleusercontent.com/caldav/v2/{email}/events
	// The URL stored in source is already the calendar URL

	// For Google, we create a single calendar entry for the primary calendar
	// The actual calendar URL is the one stored in CalDAVURL
	calendar := &models.Calendar{
		SourceID:    c.source.ID,
		UserID:      c.source.UserID,
		RemoteID:    c.source.CalDAVURL, // Use the full URL as remote ID
		Name:        c.source.Name,
		Description: "Google Calendar",
		Timezone:    "UTC",
		CanWrite:    true,
		Color:       c.source.Color,
	}

	return []*models.Calendar{calendar}, nil
}

// SyncCalendar synchronizes a calendar from the server using transactions
func (c *Client) SyncCalendar(ctx context.Context, cal *models.Calendar) error {
	if c.client == nil {
		return fmt.Errorf("client not connected")
	}

	// A disabled calendar does not talk to its source in either direction. The
	// guard sits here rather than in the callers so no sync path can miss it.
	if !cal.Enabled {
		log.Printf("Skipping calendar %s — disabled", cal.Name)
		return nil
	}

	log.Printf("Syncing calendar %s (%s)", cal.Name, cal.RemoteID)

	// Get all current events from the server
	// For direct calendar URLs, the client base URL is already the full calendar URL
	calendarPath := cal.RemoteID
	if strings.HasPrefix(cal.RemoteID, "https://") || strings.HasPrefix(cal.RemoteID, "http://") {
		calendarPath = ""
	}

	// Time range for events (6 months back, 1 year forward)
	now := time.Now()
	startTime := now.AddDate(0, -6, 0)
	endTime := now.AddDate(1, 0, 0)

	// Ask for what this collection actually holds. Asking every collection for
	// VEVENT is why Apple's reminder lists came back empty once discovery
	// finally stopped throwing them away: a VTODO-only collection answers a
	// VEVENT query with nothing, perfectly correctly.
	component := models.ComponentEvent
	if cal.Supports(models.ComponentTodo) && !cal.Supports(models.ComponentEvent) {
		component = models.ComponentTodo
	}

	// A time-range on VTODO is a trap. RFC 4791 §9.9 defines it against DTSTART,
	// DUE, DURATION and COMPLETED, and a task carrying none of those — the
	// ordinary "buy milk" — matches no window at all. Tasks are fetched whole.
	var filterStart, filterEnd time.Time
	if component == models.ComponentEvent {
		filterStart, filterEnd = startTime, endTime
	}

	// Try with time-range filter first (required by iCloud and many servers)
	// Request minimal data in query to get list of paths/ETags
	query := &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{
			Name: "VCALENDAR",
			Comps: []caldav.CalendarCompRequest{
				{Name: component, Props: []string{"UID"}},
			},
		},
		CompFilter: caldav.CompFilter{
			Name: "VCALENDAR",
			Comps: []caldav.CompFilter{
				{
					Name:  component,
					Start: filterStart,
					End:   filterEnd,
				},
			},
		},
	}

	objects, err := c.client.QueryCalendar(ctx, calendarPath, query)
	if err != nil {
		// Fallback: try without time-range (for servers that don't support it)
		log.Printf("CalDAV QueryCalendar with time-range failed, trying without: %v", err)
		query = &caldav.CalendarQuery{
			CompRequest: caldav.CalendarCompRequest{
				Name: "VCALENDAR",
				Comps: []caldav.CalendarCompRequest{
					{Name: component, Props: []string{"UID"}},
				},
			},
			CompFilter: caldav.CompFilter{
				Name: "VCALENDAR",
				Comps: []caldav.CompFilter{
					{Name: component},
				},
			},
		}
		objects, err = c.client.QueryCalendar(ctx, calendarPath, query)
		if err != nil {
			log.Printf("CalDAV QueryCalendar failed for path '%s' (base URL: %s): %v", calendarPath, c.source.CalDAVURL, err)
			return fmt.Errorf("failed to query calendar: %w", err)
		}
	}

	// For each object, fetch full data using GET
	log.Printf("CalDAV query returned %d object paths for calendar %s, fetching full data...", len(objects), cal.Name)
	fullObjects := make([]caldav.CalendarObject, 0, len(objects))
	for _, obj := range objects {
		if obj.Path == "" {
			continue
		}
		fullObj, err := c.client.GetCalendarObject(ctx, obj.Path)
		if err != nil {
			// Try fallback direct HTTP fetch for servers with non-standard iCalendar
			if strings.Contains(err.Error(), "invalid syntax") || strings.Contains(err.Error(), "parse") {
				fullObj, err = c.fetchICSDirectly(ctx, obj.Path)
				if err != nil {
					log.Printf("Failed to fetch calendar object %s (fallback): %v", obj.Path, err)
					continue
				}
			} else {
				log.Printf("Failed to GET calendar object %s: %v", obj.Path, err)
				continue
			}
		}
		if fullObj == nil {
			continue
		}
		fullObjects = append(fullObjects, *fullObj)
	}
	objects = fullObjects

	log.Printf("CalDAV returned %d objects for calendar %s", len(objects), cal.Name)

	// Get existing events for comparison
	existingUIDs, err := c.database.GetAllEventUIDsForCalendar(cal.ID)
	if err != nil {
		return fmt.Errorf("failed to get existing UIDs: %w", err)
	}

	// Collect changes for transactional application
	changes := &db.SyncEventChanges{
		CalendarID: cal.ID,
		Creates:    make([]*models.CalendarEvent, 0),
		Updates:    make([]*models.CalendarEvent, 0),
		DeleteUIDs: make([]string, 0),
	}

	// Track which UIDs we've seen
	seenUIDs := make(map[string]bool)

	// Process each event from the server
	parsedCount := 0
	for i, obj := range objects {
		log.Printf("Processing object %d/%d, path=%s, hasData=%v", i+1, len(objects), obj.Path, obj.Data != nil)
		event, err := c.parseCalendarObject(&obj, cal.ID)
		if err != nil {
			log.Printf("Failed to parse event %d: %v", i+1, err)
			continue
		}

		if event.UID == "" {
			log.Printf("Skipping event with empty UID: %s", event.Summary)
			continue
		}

		parsedCount++
		seenUIDs[event.UID] = true

		// Check if event exists
		existing, err := c.database.GetEventByUID(cal.ID, event.UID)
		if err != nil {
			log.Printf("Failed to check existing event: %v", err)
			continue
		}

		if existing != nil {
			// Skip locally modified events — they will be pushed to remote by reverse sync
			if existing.LocalModified {
				continue
			}
			// Check if ETag changed
			if existing.ETag != event.ETag {
				// Queue update
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
				existing.Attendees = event.Attendees
				changes.Updates = append(changes.Updates, existing)
			}
		} else {
			// Skip create when the user has already asked us to DELETE
			// this event upstream and the outbound worker hasn't yet
			// drained the queue. Without this guard, the next inbound
			// sync would resurrect the row (deleted_locally → not_found
			// → "queue create" branch), the frontend would show the
			// event again for a couple minutes, and then the eventually-
			// successful outbound DELETE would finally remove it on the
			// upstream side and the row would vanish for good. From the
			// user's perspective: "I deleted it, it came back, then it
			// disappeared." Confusing.
			pendingDelete, pdErr := c.database.IsEventPendingDelete(cal.SourceID, event.UID)
			if pdErr != nil {
				log.Printf("Failed to check pending delete for UID %s: %v", event.UID, pdErr)
			}
			if pendingDelete {
				log.Printf("Skipping inbound create for UID %s on %s — delete pending in reverse-sync queue", event.UID, cal.Name)
				continue
			}
			// Queue create
			changes.Creates = append(changes.Creates, event)
		}
	}

	log.Printf("Parsed %d events, queued %d creates, %d updates", parsedCount, len(changes.Creates), len(changes.Updates))

	// Queue deletes for events that no longer exist on the server
	for uid := range existingUIDs {
		if !seenUIDs[uid] {
			changes.DeleteUIDs = append(changes.DeleteUIDs, uid)
		}
	}

	// Apply all changes in a single transaction
	if len(changes.Creates) > 0 || len(changes.Updates) > 0 || len(changes.DeleteUIDs) > 0 {
		if err := c.database.ApplySyncChanges(changes); err != nil {
			return fmt.Errorf("failed to apply sync changes: %w", err)
		}
		log.Printf("Sync applied: %d created, %d updated, %d deleted",
			len(changes.Creates), len(changes.Updates), len(changes.DeleteUIDs))
	}

	return nil
}

// PushChanges pushes local changes to the server
func (c *Client) PushChanges(ctx context.Context, cal *models.Calendar) error {
	if c.client == nil {
		return fmt.Errorf("client not connected")
	}

	// Get locally modified events
	events, err := c.database.GetLocallyModifiedEvents(cal.ID)
	if err != nil {
		return fmt.Errorf("failed to get modified events: %w", err)
	}

	for _, event := range events {
		// Build the path for this event
		eventPath := fmt.Sprintf("%s%s.ics", cal.RemoteID, event.UID)

		// Create iCal calendar
		icalCal := c.createICalFromEvent(event)

		// PUT the event to the server
		_, err := c.client.PutCalendarObject(ctx, eventPath, icalCal)
		if err != nil {
			log.Printf("Failed to push event %s: %v", event.UID, err)
			continue
		}

		// Mark as synced
		if err := c.database.MarkEventSynced(event.ID, event.ETag); err != nil {
			log.Printf("Failed to mark event synced: %v", err)
		}
	}

	return nil
}

// parseCalendarObject parses a caldav.CalendarObject into our model
func (c *Client) parseCalendarObject(obj *caldav.CalendarObject, calendarID int64) (*models.CalendarEvent, error) {
	event := &models.CalendarEvent{
		CalendarID: calendarID,
		RemoteID:   obj.Path,
		ETag:       obj.ETag,
	}

	if obj.Data == nil {
		return nil, fmt.Errorf("no calendar data")
	}

	// Ensure PRODID exists (required by iCal spec, but some servers omit it)
	if obj.Data.Props.Get(ical.PropProductID) == nil {
		obj.Data.Props.SetText(ical.PropProductID, "-//DDMailServer//CalDAV Client//EN")
	}
	// Ensure VERSION exists
	if obj.Data.Props.Get(ical.PropVersion) == nil {
		obj.Data.Props.SetText(ical.PropVersion, "2.0")
	}

	// Ensure required properties exist (RFC 5545 demands DTSTAMP and UID on
	// both VEVENT and VTODO; the repair used to skip tasks, which would have
	// left them unencodable the moment we tried to push one back).
	for _, child := range obj.Data.Children {
		if child.Name == ical.CompEvent || child.Name == ical.CompToDo {
			// Add DTSTAMP if missing
			if child.Props.Get(ical.PropDateTimeStamp) == nil {
				dtstamp := ical.NewProp(ical.PropDateTimeStamp)
				dtstamp.SetDateTime(time.Now().UTC())
				child.Props.Add(dtstamp)
			}
			// Add UID if missing (generate from path or content hash)
			if child.Props.Get(ical.PropUID) == nil {
				uid := generateUIDFromPath(obj.Path)
				child.Props.SetText(ical.PropUID, uid)
			}
		}
	}

	// Store raw iCal data
	var icalData strings.Builder
	encoder := ical.NewEncoder(&icalData)
	if err := encoder.Encode(obj.Data); err != nil {
		return nil, fmt.Errorf("failed to serialize iCal: %w", err)
	}
	event.ICalData = icalData.String()

	// Inject default alarm if source has it enabled and event has no VALARM
	if c.source.DefaultAlarmEnabled {
		event.ICalData = caldavutil.InjectDefaultAlarm(event.ICalData, c.source.DefaultAlarmBefore, c.source.DefaultAlarmUnit)
	}

	// If no ETag, generate one
	if event.ETag == "" {
		event.ETag = generateETag(event.ICalData)
	}

	// Extract the properties through the shared importer.
	//
	// This was a third hand-rolled copy of the same extraction — after the one
	// in the CalDAV server's PUT handler — and it had drifted in two ways that
	// mattered: it was blind to VTODO, so a reminder list synced as rows with
	// no summary and no due date; and it resolved DTSTART with prop.DateTime,
	// which ignores whatever VTIMEZONE the server sent and silently zeroes a
	// start whose zone this host does not know. The importer's zone resolver
	// handles both.
	parsed, parseErr := importer.ParseICS(event.ICalData)
	if parseErr != nil || len(parsed) == 0 {
		return nil, fmt.Errorf("failed to parse calendar object: %w", parseErr)
	}

	p := parsed[0]
	event.Component = p.Component
	event.UID = p.UID
	event.Summary = p.Summary
	event.Description = p.Description
	event.Location = p.Location
	event.RRule = p.RRule
	event.RecurrenceID = p.RecurrenceID
	event.DTStart = p.DTStart
	event.DTEnd = p.DTEnd
	event.AllDay = p.AllDay
	event.Due = p.Due
	event.CompletedAt = p.CompletedAt
	event.PercentComplete = p.PercentComplete
	event.Priority = p.Priority
	if p.Status != "" {
		event.Status = p.Status
	}
	// Organizer + attendees. Without these the RSVP pills in the desktop card
	// never resolve `myAttendee` and the bar stays hidden — the user can't see
	// they were invited to an externally-synced meeting.
	event.OrganizerEmail = p.OrganizerEmail
	event.OrganizerName = p.OrganizerName
	event.Attendees = p.Attendees

	if event.UID == "" {
		return nil, fmt.Errorf("event has no UID")
	}

	return event, nil
}

// createICalFromEvent creates an ical.Calendar from a CalendarEvent
func (c *Client) createICalFromEvent(event *models.CalendarEvent) *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//DDMailServer//Calendar//EN")

	vevent := ical.NewEvent()
	vevent.Props.SetText(ical.PropUID, event.UID)
	vevent.Props.SetText(ical.PropSummary, event.Summary)

	if event.Description != "" {
		vevent.Props.SetText(ical.PropDescription, event.Description)
	}
	if event.Location != "" {
		vevent.Props.SetText(ical.PropLocation, event.Location)
	}
	if event.RRule != "" {
		vevent.Props.SetText(ical.PropRecurrenceRule, event.RRule)
	}

	// Set DTSTART
	dtstart := ical.NewProp(ical.PropDateTimeStart)
	dtstart.SetDateTime(timeutil.FromMs(event.DTStart))
	vevent.Props.Add(dtstart)

	// Set DTEND
	if event.DTEnd != nil && *event.DTEnd != 0 {
		dtend := ical.NewProp(ical.PropDateTimeEnd)
		dtend.SetDateTime(timeutil.FromMs(*event.DTEnd))
		vevent.Props.Add(dtend)
	}

	cal.Children = append(cal.Children, vevent.Component)

	return cal
}

// PutEventRaw uploads raw iCal data to a remote CalDAV path
func (c *Client) PutEventRaw(ctx context.Context, remotePath string, icalData string) error {
	if c.httpClient == nil {
		return fmt.Errorf("client not connected")
	}

	// Build full URL from source's CalDAV URL and remote path
	fullURL := c.buildFullURL(remotePath)

	// A calendar object missing DTSTAMP is invalid per RFC 5545 and a CalDAV
	// server may — and iCloud does — reject it with 403. Repairing here covers
	// bodies that were generated before the writers were fixed and are already
	// sitting in the sync queue.
	icalData = caldavutil.EnsureDTSTAMP(icalData, timeutil.Now())

	req, err := http.NewRequestWithContext(ctx, "PUT", fullURL, strings.NewReader(icalData))
	if err != nil {
		return fmt.Errorf("failed to create PUT request: %w", err)
	}
	req.Header.Set("Content-Type", "text/calendar; charset=utf-8")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("PUT request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		// Headers go to the log rather than into the error: the error is stored
		// per queue entry and must stay short, but when the body comes back
		// empty — as iCloud's 403 did — the headers are the only thing left that
		// might name the precondition that failed.
		logResponseHeaders("PUT", fullURL, resp)
		// The URL belongs in the message: this error is all the sync queue
		// keeps, and "PUT failed with status 403:" on its own says nothing about
		// where it was sent.
		return fmt.Errorf("PUT %s failed with status %d: %s", fullURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

// logResponseHeaders dumps a failed CalDAV response's headers, sorted so two
// failures can be diffed against each other.
func logResponseHeaders(method, url string, resp *http.Response) {
	names := make([]string, 0, len(resp.Header))
	for name := range resp.Header {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(strings.Join(resp.Header[name], ", "))
	}

	log.Printf("CalDAV %s %s → %d; response headers: %s", method, url, resp.StatusCode, b.String())
}

// DeleteEvent deletes an event from a remote CalDAV server
func (c *Client) DeleteEvent(ctx context.Context, remotePath string) error {
	if c.httpClient == nil {
		return fmt.Errorf("client not connected")
	}

	// Build full URL from source's CalDAV URL and remote path
	fullURL := c.buildFullURL(remotePath)

	req, err := http.NewRequestWithContext(ctx, "DELETE", fullURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create DELETE request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 && resp.StatusCode != 404 {
		body, _ := io.ReadAll(resp.Body)
		logResponseHeaders("DELETE", fullURL, resp)
		return fmt.Errorf("DELETE %s failed with status %d: %s", fullURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

// generateETag generates an ETag from content
func generateETag(content string) string {
	hash := sha256.Sum256([]byte(content))
	return fmt.Sprintf("\"%x\"", hash[:8])
}

// generateUIDFromPath generates a stable UID from a calendar object path
func generateUIDFromPath(path string) string {
	hash := sha256.Sum256([]byte(path))
	return fmt.Sprintf("%x@ddmailserver.local", hash[:16])
}

// oauthTransport adds OAuth2 bearer token to requests
type oauthTransport struct {
	base  http.RoundTripper
	token string
}

func (t *oauthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(req)
}

// etagFixTransport wraps a transport to fix malformed ETags from Yandex CalDAV
type etagFixTransport struct {
	base http.RoundTripper
}

func (t *etagFixTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	// Read the body, fix ETags, and replace the body
	// Check for XML content type (may include charset parameter)
	contentType := resp.Header.Get("Content-Type")
	isXML := strings.Contains(contentType, "xml") || strings.Contains(contentType, "text/xml")

	if resp.Body != nil && isXML {
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return resp, readErr
		}

		// Fix unquoted ETags (Yandex returns <D:getetag>value</D:getetag> without quotes)
		// Convert to proper format: <D:getetag>"value"</D:getetag>
		bodyStr := string(body)

		// Match <D:getetag> or <getetag> or <d:getetag> followed by content without quotes
		// Support various namespace prefixes and handle whitespace
		re := regexp.MustCompile(`(<(?:[dD]:)?getetag[^>]*>)\s*([^<"]+?)\s*(</(?:[dD]:)?getetag>)`)
		fixedBody := re.ReplaceAllStringFunc(bodyStr, func(match string) string {
			parts := re.FindStringSubmatch(match)
			if len(parts) == 4 {
				etag := strings.TrimSpace(parts[2])
				// Only fix if not already quoted
				if etag != "" && !strings.HasPrefix(etag, "\"") {
					return parts[1] + "\"" + etag + "\"" + parts[3]
				}
			}
			return match
		})

		resp.Body = io.NopCloser(strings.NewReader(fixedBody))
		resp.ContentLength = int64(len(fixedBody))
	}

	return resp, nil
}

// fetchICSDirectly fetches an ICS file directly via HTTP when go-webdav fails
// This is a fallback for servers like Yandex that return non-standard iCalendar data
func (c *Client) fetchICSDirectly(ctx context.Context, path string) (*caldav.CalendarObject, error) {
	if c.httpClient == nil {
		return nil, fmt.Errorf("no HTTP client available")
	}

	// Construct full URL from base URL and path
	baseURL := c.source.CalDAVURL
	// Remove trailing slash from base URL if present
	baseURL = strings.TrimSuffix(baseURL, "/")
	// Make sure path starts with /
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	// Extract scheme and host from base URL
	var fullURL string
	if strings.HasPrefix(baseURL, "https://") {
		parts := strings.SplitN(baseURL[8:], "/", 2)
		fullURL = "https://" + parts[0] + path
	} else if strings.HasPrefix(baseURL, "http://") {
		parts := strings.SplitN(baseURL[7:], "/", 2)
		fullURL = "http://" + parts[0] + path
	} else {
		return nil, fmt.Errorf("invalid base URL: %s", baseURL)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Pre-process ICS content to fix common issues
	icsContent := string(body)

	// Fix common issues with Yandex iCalendar:
	// 1. Remove BOM if present
	icsContent = strings.TrimPrefix(icsContent, "\xef\xbb\xbf")

	// 2. Normalize line endings to CRLF (iCalendar spec requires CRLF)
	icsContent = strings.ReplaceAll(icsContent, "\r\n", "\n")
	icsContent = strings.ReplaceAll(icsContent, "\r", "\n")
	icsContent = strings.ReplaceAll(icsContent, "\n", "\r\n")

	// 3. Fix unfolded long lines - some servers don't properly fold lines
	// This is a simple fix that might help with some parsers

	// Try to parse the cleaned content
	dec := ical.NewDecoder(strings.NewReader(icsContent))
	cal, err := dec.Decode()
	if err != nil {
		// Try one more time with just the raw body (minimal processing)
		dec = ical.NewDecoder(strings.NewReader(string(body)))
		cal, err = dec.Decode()
		if err != nil {
			return nil, fmt.Errorf("iCalendar parse error: %w", err)
		}
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		// Generate a pseudo-etag from content hash
		hash := sha256.Sum256(body)
		etag = fmt.Sprintf("\"%x\"", hash[:8])
	}

	return &caldav.CalendarObject{
		Path: path,
		ETag: etag,
		Data: cal,
	}, nil
}
