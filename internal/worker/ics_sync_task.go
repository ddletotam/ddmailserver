package worker

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	caldavutil "github.com/yourusername/mailserver/internal/caldav"
	"github.com/yourusername/mailserver/internal/caldav/importer"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/notify"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// ICSSyncTask represents an ICS URL synchronization task
type ICSSyncTask struct {
	source    *models.CalendarSource
	database  *db.DB
	notifyHub *notify.Hub
}

// NewICSSyncTask creates a new ICS URL sync task
func NewICSSyncTask(source *models.CalendarSource, database *db.DB, notifyHub *notify.Hub) *ICSSyncTask {
	return &ICSSyncTask{
		source:    source,
		database:  database,
		notifyHub: notifyHub,
	}
}

// Execute runs the ICS URL sync task
func (t *ICSSyncTask) Execute(ctx context.Context) error {
	log.Printf("Starting ICS URL sync for source %s (ID: %d)", t.source.Name, t.source.ID)

	err := t.doSync(ctx)
	if err != nil {
		// Save error to database so user can see it
		if dbErr := t.database.UpdateCalendarSourceLastError(t.source.ID, err.Error()); dbErr != nil {
			log.Printf("Failed to save sync error: %v", dbErr)
		}
		return err
	}

	log.Printf("ICS URL sync completed for %s", t.source.Name)
	return nil
}

// doSync performs the actual synchronization
func (t *ICSSyncTask) doSync(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if t.source.IcsURL == "" {
		return fmt.Errorf("no ICS URL configured")
	}

	// Fetch ICS from URL
	icsData, err := t.fetchICS(ctx, t.source.IcsURL)
	if err != nil {
		return fmt.Errorf("failed to fetch ICS: %w", err)
	}

	// A 200 response is not proof that we were handed a calendar: a captive
	// portal, an expired share link or a proxy error page all arrive with the
	// right status and the wrong body. Nothing below distinguishes "the feed is
	// now empty" from "this is not a feed", so check before parsing.
	if !strings.Contains(icsData, "BEGIN:VCALENDAR") {
		return fmt.Errorf("response is not iCalendar data (%d bytes, no BEGIN:VCALENDAR)", len(icsData))
	}

	// Get or create calendar for this source
	calendars, err := t.database.GetCalendarsBySourceID(t.source.ID)
	if err != nil {
		return fmt.Errorf("failed to get calendars: %w", err)
	}

	var calendar *models.Calendar
	if len(calendars) == 0 {
		// Create default calendar
		calendar = &models.Calendar{
			SourceID: t.source.ID,
			UserID:   t.source.UserID,
			Name:     t.source.Name,
			Color:    t.source.Color,
			Timezone: "UTC",
			CanWrite: false, // Read-only for ICS URL sources
			Enabled:  true,  // Without this, Go's bool zero-value lands as soft-deleted on insert
		}
		if err := t.database.CreateCalendar(calendar); err != nil {
			return fmt.Errorf("failed to create calendar: %w", err)
		}
		log.Printf("Created calendar for ICS source: %s", calendar.Name)
	} else {
		calendar = calendars[0]
	}

	// A disabled calendar does not sync with its source. An ICS source owns
	// exactly one calendar, so turning it off silences the whole source.
	if !calendar.Enabled {
		log.Printf("Skipping ICS sync for %s — calendar %q is disabled", t.source.Name, calendar.Name)
		return nil
	}

	// Parse ICS events
	events, err := importer.ParseICS(icsData)
	if err != nil {
		return fmt.Errorf("failed to parse ICS: %w", err)
	}

	log.Printf("Parsed %d events from ICS URL", len(events))

	// Inject default alarm if source has it enabled
	if t.source.DefaultAlarmEnabled {
		for _, event := range events {
			event.ICalData = caldavutil.InjectDefaultAlarm(event.ICalData, t.source.DefaultAlarmBefore, t.source.DefaultAlarmUnit)
		}
	}

	// Get existing events for comparison
	identities, err := t.database.GetEventIdentitiesForCalendar(calendar.ID)
	if err != nil {
		return fmt.Errorf("failed to get existing events: %w", err)
	}

	// Every UID absent from the feed is queued for deletion below, so a payload
	// that yields no events at all would empty the calendar in one transaction.
	// ParseICS reports no error for input that simply is not iCalendar, which
	// makes that the default outcome of a malformed feed rather than an edge
	// case. Treat it as a failed sync: the events stay, the error surfaces on
	// the source, and the next cycle tries again.
	if len(events) == 0 && len(identities) > 0 {
		return fmt.Errorf("feed parsed to 0 events while the calendar holds %d; refusing to read that as a full delete", len(identities))
	}

	for _, event := range events {
		event.CalendarID = calendar.ID
	}

	matches, creates, deleteUIDs := matchFeedEvents(identities, events)

	// Collect changes for transactional application
	changes := &db.SyncEventChanges{
		CalendarID: calendar.ID,
		Creates:    creates,
		Updates:    make([]*models.CalendarEvent, 0, len(matches)),
		DeleteUIDs: deleteUIDs,
	}

	readopted := 0
	for _, match := range matches {
		existing, err := t.database.GetEventByID(match.ExistingID)
		if err != nil || existing == nil {
			log.Printf("Failed to load event %d for update: %v", match.ExistingID, err)
			continue
		}

		// An unchanged event is left alone. A re-adopted one is always written:
		// its stored UID is stale by definition, and that is the whole point of
		// having matched it.
		if existing.ETag == match.Event.ETag && !match.ByContent {
			continue
		}
		if match.ByContent {
			readopted++
		}

		existing.UID = match.Event.UID
		existing.ICalData = match.Event.ICalData
		existing.Summary = match.Event.Summary
		existing.Description = match.Event.Description
		existing.Location = match.Event.Location
		existing.DTStart = match.Event.DTStart
		existing.DTEnd = match.Event.DTEnd
		existing.AllDay = match.Event.AllDay
		existing.RRule = match.Event.RRule
		existing.ETag = match.Event.ETag
		changes.Updates = append(changes.Updates, existing)
	}

	// Apply all changes in a single transaction
	if len(changes.Creates) > 0 || len(changes.Updates) > 0 || len(changes.DeleteUIDs) > 0 {
		if err := t.database.ApplySyncChanges(changes); err != nil {
			return fmt.Errorf("failed to apply sync changes: %w", err)
		}
		if readopted > 0 {
			log.Printf("ICS sync applied: %d created, %d updated, %d deleted (%d matched by content — this feed reissues its UIDs)",
				len(changes.Creates), len(changes.Updates), len(changes.DeleteUIDs), readopted)
		} else {
			log.Printf("ICS sync applied: %d created, %d updated, %d deleted",
				len(changes.Creates), len(changes.Updates), len(changes.DeleteUIDs))
		}

		if t.notifyHub != nil {
			t.notifyHub.Publish(notify.Event{
				UserID:     t.source.UserID,
				Type:       notify.EventCalendarUpdated,
				CalendarID: calendar.ID,
			})
		}
	}

	// Update last sync time (clears error on success)
	if err := t.database.UpdateCalendarSourceLastSync(t.source.ID, timeutil.Now(), ""); err != nil {
		log.Printf("Failed to update last sync time: %v", err)
	}

	return nil
}

// fetchICS fetches ICS data from a URL
func (t *ICSSyncTask) fetchICS(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", "DDMailServer/1.0 (Calendar Sync)")
	req.Header.Set("Accept", "text/calendar, application/calendar+json, */*")

	// Skip TLS verification for ICS URLs (some corporate servers use self-signed certs)
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	return string(body), nil
}

// Type returns the task type for queue routing
func (t *ICSSyncTask) Type() TaskType {
	return TaskTypeIMAP // Use IMAP queue (similar I/O bound work)
}

// Priority returns the task priority
func (t *ICSSyncTask) Priority() int {
	return 5 // Default priority
}

// String returns a human-readable description
func (t *ICSSyncTask) String() string {
	return fmt.Sprintf("ICSSync[%s]", t.source.Name)
}
