package web

import (
	"context"
	"crypto/tls"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	caldavclient "github.com/yourusername/mailserver/internal/caldav/client"
	"github.com/yourusername/mailserver/internal/caldav/importer"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/oauth"
)

// CalendarsData holds data for the calendars page
type CalendarsData struct {
	PageData
	Host               string
	GoogleOAuthEnabled bool
}

// CalendarSourcesListData holds data for the calendar sources list
type CalendarSourcesListData struct {
	PageData
	Sources []*models.CalendarSource
}

// CalendarWithMeta is a calendar with additional display info
type CalendarWithMeta struct {
	*models.Calendar
	EventCount int
	SourceName string
}

// CalendarsListData holds data for the calendars list
type CalendarsListData struct {
	PageData
	Calendars []CalendarWithMeta
}

// HandleCalendarsPage renders the calendars management page
func (s *Server) HandleCalendarsPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// Get host for CalDAV URL display
	host := r.Host
	if fwdHost := r.Header.Get("X-Forwarded-Host"); fwdHost != "" {
		host = fwdHost
	}

	data := CalendarsData{
		PageData: PageData{
			Title: "Calendars",
			User:  user,
		},
		Host:               host,
		GoogleOAuthEnabled: s.googleOAuth != nil,
	}

	s.renderTemplate(w, "calendars.html", data)
}

// HandleCalendarSourcesList returns the list of calendar sources (HTMX partial)
func (s *Server) HandleCalendarSourcesList(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	sources, err := s.database.GetCalendarSourcesByUserID(user.ID)
	if err != nil {
		log.Printf("Failed to get calendar sources: %v", err)
		http.Error(w, "Failed to load sources", http.StatusInternalServerError)
		return
	}

	data := CalendarSourcesListData{
		PageData: PageData{User: user},
		Sources:  sources,
	}

	s.renderTemplatePartial(w, "calendars.html", "calendar-sources-list", data)
}

// HandleCalendarsList returns the list of calendars (HTMX partial)
func (s *Server) HandleCalendarsList(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	calendars, err := s.database.GetCalendarsByUserID(user.ID)
	if err != nil {
		log.Printf("Failed to get calendars: %v", err)
		http.Error(w, "Failed to load calendars", http.StatusInternalServerError)
		return
	}

	// Add event counts and source names
	var calendarsWithMeta []CalendarWithMeta
	for _, cal := range calendars {
		meta := CalendarWithMeta{Calendar: cal}

		// Get event count
		count, err := s.database.GetEventCountForCalendar(cal.ID)
		if err == nil {
			meta.EventCount = count
		}

		// Get source name
		if cal.SourceID > 0 {
			source, err := s.database.GetCalendarSourceByID(cal.SourceID)
			if err == nil && source != nil {
				meta.SourceName = source.Name
			}
		}

		calendarsWithMeta = append(calendarsWithMeta, meta)
	}

	data := CalendarsListData{
		PageData:  PageData{User: user},
		Calendars: calendarsWithMeta,
	}

	s.renderTemplatePartial(w, "calendars.html", "calendars-list", data)
}

// HandleCreateCalendarSourceWeb creates a new CalDAV source (web form handler)
func (s *Server) HandleCreateCalendarSourceWeb(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	source := &models.CalendarSource{
		UserID:         user.ID,
		Name:           r.FormValue("name"),
		SourceType:     "caldav",
		CalDAVURL:      r.FormValue("caldav_url"),
		CalDAVUsername: r.FormValue("caldav_username"),
		CalDAVPassword: r.FormValue("caldav_password"),
		AuthType:       "password",
		Color:          r.FormValue("color"),
		SyncEnabled:    true,
		SyncInterval:   60, // 1 minute - sync as often as possible
	}

	if source.Name == "" || source.CalDAVURL == "" {
		http.Error(w, "Name and URL are required", http.StatusBadRequest)
		return
	}

	if err := s.database.CreateCalendarSource(source); err != nil {
		log.Printf("Failed to create calendar source: %v", err)
		http.Error(w, "Failed to create source", http.StatusInternalServerError)
		return
	}

	log.Printf("Created calendar source %s for user %d", source.Name, user.ID)

	// Return updated list
	s.HandleCalendarSourcesList(w, r)
}

// HandleCreateICSURLSource creates a new ICS URL source
func (s *Server) HandleCreateICSURLSource(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	name := r.FormValue("name")
	icsURL := r.FormValue("ics_url")
	color := r.FormValue("color")
	syncInterval := 300 // 5 minutes for ICS URL

	if intervalStr := r.FormValue("sync_interval"); intervalStr != "" {
		if v, err := strconv.Atoi(intervalStr); err == nil && v >= 15 {
			syncInterval = v
		}
	}

	if name == "" || icsURL == "" {
		http.Error(w, "Name and ICS URL are required", http.StatusBadRequest)
		return
	}

	source := &models.CalendarSource{
		UserID:       user.ID,
		Name:         name,
		SourceType:   "ics_url",
		IcsURL:       icsURL,
		Color:        color,
		SyncEnabled:  true,
		SyncInterval: syncInterval,
	}

	if err := s.database.CreateCalendarSource(source); err != nil {
		log.Printf("Failed to create ICS URL source: %v", err)
		http.Error(w, "Failed to create source", http.StatusInternalServerError)
		return
	}

	log.Printf("Created ICS URL source %s for user %d", source.Name, user.ID)

	// Return updated list
	s.HandleCalendarSourcesList(w, r)
}

// HandleCreateLocalCalendar creates a new local calendar
func (s *Server) HandleCreateLocalCalendar(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	name := r.FormValue("name")
	color := r.FormValue("color")

	if name == "" {
		http.Error(w, "Name is required", http.StatusBadRequest)
		return
	}

	// Create a local source for this calendar
	source := &models.CalendarSource{
		UserID:      user.ID,
		Name:        name,
		SourceType:  "local",
		Color:       color,
		SyncEnabled: false,
	}

	if err := s.database.CreateCalendarSource(source); err != nil {
		log.Printf("Failed to create local source: %v", err)
		http.Error(w, "Failed to create calendar", http.StatusInternalServerError)
		return
	}

	// Create the calendar
	calendar := &models.Calendar{
		SourceID: source.ID,
		UserID:   user.ID,
		Name:     name,
		Color:    color,
		Timezone: "UTC",
		CanWrite: true,
	}

	if err := s.database.CreateCalendar(calendar); err != nil {
		log.Printf("Failed to create calendar: %v", err)
		http.Error(w, "Failed to create calendar", http.StatusInternalServerError)
		return
	}

	log.Printf("Created local calendar %s for user %d", name, user.ID)

	// Return updated list
	s.HandleCalendarsList(w, r)
}

// HandleSyncCalendarSource triggers sync for a calendar source
func (s *Server) HandleSyncCalendarSource(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	source, err := s.database.GetCalendarSourceByID(id)
	if err != nil || source == nil || source.UserID != user.ID {
		http.Error(w, "Source not found", http.StatusNotFound)
		return
	}

	// Check source type
	if source.SourceType == "local" {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<span class="badge bg-secondary">Local calendar</span>`)
		return
	}

	// Execute sync synchronously for immediate feedback
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	syncErr := s.syncCalendarSource(ctx, source)

	w.Header().Set("Content-Type", "text/html")
	if syncErr != nil {
		log.Printf("Calendar sync failed for %s: %v", source.Name, syncErr)
		fmt.Fprintf(w, `<span class="badge bg-danger" title="%s">Sync failed</span>`,
			template.HTMLEscapeString(syncErr.Error()))
	} else {
		fmt.Fprintf(w, `<span class="badge bg-success">Synced</span>`)
	}
}

// refreshOAuthTokensIfNeeded refreshes OAuth tokens if they are expired or about to expire
func (s *Server) refreshOAuthTokensIfNeeded(source *models.CalendarSource) error {
	// Only refresh for OAuth sources
	if source.AuthType != "oauth2_google" && source.AuthType != "oauth2_microsoft" {
		return nil
	}

	// Check if token is expired or expires within 5 minutes
	if source.OAuthTokenExpiry.IsZero() || time.Until(source.OAuthTokenExpiry) > 5*time.Minute {
		return nil // Token still valid
	}

	// Need refresh token
	if source.OAuthRefreshToken == "" {
		return fmt.Errorf("no refresh token available, please re-authenticate")
	}

	log.Printf("Refreshing OAuth token for %s (expires: %v)", source.Name, source.OAuthTokenExpiry)

	var tokenResp *oauth.TokenResponse
	var err error

	if source.AuthType == "oauth2_google" {
		if s.googleOAuth == nil {
			return fmt.Errorf("Google OAuth not configured")
		}
		tokenResp, err = s.googleOAuth.RefreshToken(source.OAuthRefreshToken)
	} else if source.AuthType == "oauth2_microsoft" {
		if s.microsoftOAuth == nil {
			return fmt.Errorf("Microsoft OAuth not configured")
		}
		tokenResp, err = s.microsoftOAuth.RefreshToken(source.OAuthRefreshToken)
	}

	if err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}

	// Calculate new expiry
	expiry := oauth.TokenExpiry(tokenResp.ExpiresIn)

	// Update tokens in database
	newRefreshToken := tokenResp.RefreshToken
	if newRefreshToken == "" {
		newRefreshToken = source.OAuthRefreshToken // Keep existing if not returned
	}

	if err := s.database.UpdateCalendarSourceOAuthTokens(source.ID, tokenResp.AccessToken, newRefreshToken, expiry); err != nil {
		return fmt.Errorf("failed to save new tokens: %w", err)
	}

	// Update source in memory for immediate use
	source.OAuthAccessToken = tokenResp.AccessToken
	source.OAuthRefreshToken = newRefreshToken
	source.OAuthTokenExpiry = expiry

	log.Printf("OAuth token refreshed for %s, new expiry: %v", source.Name, expiry)
	return nil
}

// syncCalendarSource performs synchronization for a calendar source
func (s *Server) syncCalendarSource(ctx context.Context, source *models.CalendarSource) error {
	log.Printf("Manual sync started for %s (ID: %d, type: %s)", source.Name, source.ID, source.SourceType)

	// Handle ICS URL sources separately
	if source.SourceType == "ics_url" {
		return s.syncICSURLSource(ctx, source)
	}

	// Refresh OAuth tokens if needed (for CalDAV sources)
	if err := s.refreshOAuthTokensIfNeeded(source); err != nil {
		s.database.UpdateCalendarSourceLastError(source.ID, err.Error())
		return fmt.Errorf("token refresh failed: %w", err)
	}

	// Create CalDAV client
	client := caldavclient.New(source, s.database)

	// Connect to CalDAV server
	if err := client.Connect(); err != nil {
		s.database.UpdateCalendarSourceLastError(source.ID, err.Error())
		return fmt.Errorf("failed to connect: %w", err)
	}

	// Discover calendars
	calendars, err := client.DiscoverCalendars(ctx)
	if err != nil {
		s.database.UpdateCalendarSourceLastError(source.ID, err.Error())
		return fmt.Errorf("failed to discover calendars: %w", err)
	}

	log.Printf("Discovered %d calendars for %s", len(calendars), source.Name)

	var syncErrors []string

	// Sync each calendar
	for _, remoteCal := range calendars {
		existingCal, err := s.database.GetCalendarByRemoteID(source.ID, remoteCal.RemoteID)
		if err != nil {
			syncErrors = append(syncErrors, fmt.Sprintf("check %s: %v", remoteCal.Name, err))
			continue
		}

		var cal *models.Calendar
		if existingCal == nil {
			remoteCal.SourceID = source.ID
			remoteCal.UserID = source.UserID
			if err := s.database.CreateCalendar(remoteCal); err != nil {
				syncErrors = append(syncErrors, fmt.Sprintf("create %s: %v", remoteCal.Name, err))
				continue
			}
			cal = remoteCal
			log.Printf("Created new calendar: %s", cal.Name)
		} else {
			cal = existingCal
		}

		if err := client.SyncCalendar(ctx, cal); err != nil {
			syncErrors = append(syncErrors, fmt.Sprintf("sync %s: %v", cal.Name, err))
			continue
		}

		log.Printf("Synced calendar: %s", cal.Name)
	}

	// Update last sync time (clears error on success)
	if err := s.database.UpdateCalendarSourceLastSync(source.ID, time.Now(), ""); err != nil {
		log.Printf("Failed to update last sync time: %v", err)
	}

	if len(syncErrors) > 0 {
		errMsg := strings.Join(syncErrors, "; ")
		s.database.UpdateCalendarSourceLastError(source.ID, errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	log.Printf("Manual sync completed for %s", source.Name)
	return nil
}

// syncICSURLSource performs synchronization for an ICS URL source
func (s *Server) syncICSURLSource(ctx context.Context, source *models.CalendarSource) error {
	if source.IcsURL == "" {
		return fmt.Errorf("no ICS URL configured")
	}

	// Fetch ICS from URL
	req, err := http.NewRequestWithContext(ctx, "GET", source.IcsURL, nil)
	if err != nil {
		s.database.UpdateCalendarSourceLastError(source.ID, err.Error())
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", "DDMailServer/1.0 (Calendar Sync)")
	req.Header.Set("Accept", "text/calendar, application/calendar+json, */*")

	// Skip TLS verification for ICS URLs (some corporate servers use self-signed certs)
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		s.database.UpdateCalendarSourceLastError(source.ID, err.Error())
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errMsg := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Status)
		s.database.UpdateCalendarSourceLastError(source.ID, errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		s.database.UpdateCalendarSourceLastError(source.ID, err.Error())
		return fmt.Errorf("failed to read response: %w", err)
	}

	icsData := string(body)

	// Get or create calendar for this source
	calendars, err := s.database.GetCalendarsBySourceID(source.ID)
	if err != nil {
		s.database.UpdateCalendarSourceLastError(source.ID, err.Error())
		return fmt.Errorf("failed to get calendars: %w", err)
	}

	var calendar *models.Calendar
	if len(calendars) == 0 {
		calendar = &models.Calendar{
			SourceID: source.ID,
			UserID:   source.UserID,
			Name:     source.Name,
			Color:    source.Color,
			Timezone: "UTC",
			CanWrite: false,
		}
		if err := s.database.CreateCalendar(calendar); err != nil {
			s.database.UpdateCalendarSourceLastError(source.ID, err.Error())
			return fmt.Errorf("failed to create calendar: %w", err)
		}
		log.Printf("Created calendar for ICS source: %s", calendar.Name)
	} else {
		calendar = calendars[0]
	}

	// Parse ICS events
	events, err := importer.ParseICS(icsData)
	if err != nil {
		s.database.UpdateCalendarSourceLastError(source.ID, err.Error())
		return fmt.Errorf("failed to parse ICS: %w", err)
	}

	log.Printf("Parsed %d events from ICS URL", len(events))

	// Get existing events for comparison
	existingUIDs, err := s.database.GetAllEventUIDsForCalendar(calendar.ID)
	if err != nil {
		s.database.UpdateCalendarSourceLastError(source.ID, err.Error())
		return fmt.Errorf("failed to get existing UIDs: %w", err)
	}

	// Collect changes
	changes := &db.SyncEventChanges{
		CalendarID: calendar.ID,
		Creates:    make([]*models.CalendarEvent, 0),
		Updates:    make([]*models.CalendarEvent, 0),
		DeleteUIDs: make([]string, 0),
	}

	seenUIDs := make(map[string]bool)

	for _, event := range events {
		event.CalendarID = calendar.ID
		seenUIDs[event.UID] = true

		existing, _ := s.database.GetEventByUID(calendar.ID, event.UID)
		if existing != nil {
			if existing.ETag != event.ETag {
				existing.ICalData = event.ICalData
				existing.Summary = event.Summary
				existing.Description = event.Description
				existing.Location = event.Location
				existing.DTStart = event.DTStart
				existing.DTEnd = event.DTEnd
				existing.AllDay = event.AllDay
				existing.RRule = event.RRule
				existing.ETag = event.ETag
				changes.Updates = append(changes.Updates, existing)
			}
		} else {
			changes.Creates = append(changes.Creates, event)
		}
	}

	for uid := range existingUIDs {
		if !seenUIDs[uid] {
			changes.DeleteUIDs = append(changes.DeleteUIDs, uid)
		}
	}

	// Apply changes
	if len(changes.Creates) > 0 || len(changes.Updates) > 0 || len(changes.DeleteUIDs) > 0 {
		if err := s.database.ApplySyncChanges(changes); err != nil {
			s.database.UpdateCalendarSourceLastError(source.ID, err.Error())
			return fmt.Errorf("failed to apply sync changes: %w", err)
		}
		log.Printf("ICS sync applied: %d created, %d updated, %d deleted",
			len(changes.Creates), len(changes.Updates), len(changes.DeleteUIDs))
	}

	// Update last sync time
	if err := s.database.UpdateCalendarSourceLastSync(source.ID, time.Now(), ""); err != nil {
		log.Printf("Failed to update last sync time: %v", err)
	}

	log.Printf("Manual ICS sync completed for %s", source.Name)
	return nil
}

// HandleDeleteCalendarSourceWeb deletes a calendar source (web UI)
func (s *Server) HandleDeleteCalendarSourceWeb(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	source, err := s.database.GetCalendarSourceByID(id)
	if err != nil || source == nil || source.UserID != user.ID {
		http.Error(w, "Source not found", http.StatusNotFound)
		return
	}

	if err := s.database.DeleteCalendarSource(id); err != nil {
		log.Printf("Failed to delete calendar source: %v", err)
		http.Error(w, "Failed to delete source", http.StatusInternalServerError)
		return
	}

	log.Printf("Deleted calendar source %d for user %d", id, user.ID)
	w.WriteHeader(http.StatusOK)
}

// HandleDeleteCalendarWeb deletes a calendar (web UI)
func (s *Server) HandleDeleteCalendarWeb(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	cal, err := s.database.GetCalendarByID(id)
	if err != nil || cal == nil || cal.UserID != user.ID {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	if err := s.database.DeleteCalendar(id); err != nil {
		log.Printf("Failed to delete calendar: %v", err)
		http.Error(w, "Failed to delete calendar", http.StatusInternalServerError)
		return
	}

	log.Printf("Deleted calendar %d for user %d", id, user.ID)
	w.WriteHeader(http.StatusOK)
}

// HandleImportICSWeb handles ICS file upload and import
func (s *Server) HandleImportICSWeb(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Parse multipart form (max 10MB)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	calendarIDStr := r.FormValue("calendar_id")
	calendarID, err := strconv.ParseInt(calendarIDStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid calendar ID", http.StatusBadRequest)
		return
	}

	// Verify calendar belongs to user
	cal, err := s.database.GetCalendarByID(calendarID)
	if err != nil || cal == nil || cal.UserID != user.ID {
		http.Error(w, "Calendar not found", http.StatusNotFound)
		return
	}

	// Get uploaded file
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "No file uploaded", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Validate file extension
	if !strings.HasSuffix(strings.ToLower(header.Filename), ".ics") {
		http.Error(w, "Only .ics files are supported", http.StatusBadRequest)
		return
	}

	// Read file content
	icsData, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "Failed to read file", http.StatusInternalServerError)
		return
	}

	// Import events
	count, err := importer.ImportICS(s.database, calendarID, icsData)
	if err != nil {
		log.Printf("Failed to import ICS: %v", err)
		http.Error(w, fmt.Sprintf("Import failed: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("Imported %d events from %s to calendar %d", count, header.Filename, calendarID)

	// Return updated list
	s.HandleCalendarsList(w, r)
}

// renderTemplatePartial renders a specific template block (for HTMX partials)
func (s *Server) renderTemplatePartial(w http.ResponseWriter, templateName, blockName string, data interface{}) {
	// Get user's language preference
	userLang := s.getUserLanguage(data)
	i18n := s.i18nManager.Get(userLang)

	// Add template functions
	funcMap := template.FuncMap{
		"t": i18n.T,
		"substr": func(str string, start, end int) string {
			if len(str) < end {
				return str
			}
			return str[start:end]
		},
	}

	tmpl, err := template.New("").Funcs(funcMap).ParseFS(templatesFS, "templates/"+templateName)
	if err != nil {
		log.Printf("Error parsing template %s: %v", templateName, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if err := tmpl.ExecuteTemplate(w, blockName, data); err != nil {
		log.Printf("Error executing template block %s in %s: %v", blockName, templateName, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}
