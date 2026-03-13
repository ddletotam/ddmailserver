package worker

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/yourusername/mailserver/internal/caldav/importer"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
)

// ICSSyncTask represents an ICS URL synchronization task
type ICSSyncTask struct {
	source   *models.CalendarSource
	database *db.DB
}

// NewICSSyncTask creates a new ICS URL sync task
func NewICSSyncTask(source *models.CalendarSource, database *db.DB) *ICSSyncTask {
	return &ICSSyncTask{
		source:   source,
		database: database,
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
		}
		if err := t.database.CreateCalendar(calendar); err != nil {
			return fmt.Errorf("failed to create calendar: %w", err)
		}
		log.Printf("Created calendar for ICS source: %s", calendar.Name)
	} else {
		calendar = calendars[0]
	}

	// Parse ICS events
	events, err := importer.ParseICS(icsData)
	if err != nil {
		return fmt.Errorf("failed to parse ICS: %w", err)
	}

	log.Printf("Parsed %d events from ICS URL", len(events))

	// Get existing events for comparison
	existingUIDs, err := t.database.GetAllEventUIDsForCalendar(calendar.ID)
	if err != nil {
		return fmt.Errorf("failed to get existing UIDs: %w", err)
	}

	// Collect changes for transactional application
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

		existing, err := t.database.GetEventByUID(calendar.ID, event.UID)
		if err != nil {
			log.Printf("Failed to check existing event: %v", err)
			continue
		}

		if existing != nil {
			// Check if event changed (compare ICalData hash/etag)
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

	// Queue deletes for events no longer in ICS
	for uid := range existingUIDs {
		if !seenUIDs[uid] {
			changes.DeleteUIDs = append(changes.DeleteUIDs, uid)
		}
	}

	// Apply all changes in a single transaction
	if len(changes.Creates) > 0 || len(changes.Updates) > 0 || len(changes.DeleteUIDs) > 0 {
		if err := t.database.ApplySyncChanges(changes); err != nil {
			return fmt.Errorf("failed to apply sync changes: %w", err)
		}
		log.Printf("ICS sync applied: %d created, %d updated, %d deleted",
			len(changes.Creates), len(changes.Updates), len(changes.DeleteUIDs))
	}

	// Update last sync time (clears error on success)
	if err := t.database.UpdateCalendarSourceLastSync(t.source.ID, time.Now(), ""); err != nil {
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
