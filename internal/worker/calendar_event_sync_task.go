package worker

import (
	"context"
	"fmt"
	"log"

	caldavclient "github.com/yourusername/mailserver/internal/caldav/client"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/task"
)

// CalendarEventSyncTask pushes local calendar event changes back to the remote CalDAV server
type CalendarEventSyncTask struct {
	source   *models.CalendarSource
	database *db.DB
	priority int
}

// NewCalendarEventSyncTask creates a new calendar event sync task
func NewCalendarEventSyncTask(source *models.CalendarSource, database *db.DB) *CalendarEventSyncTask {
	return &CalendarEventSyncTask{
		source:   source,
		database: database,
		priority: 2,
	}
}

func (t *CalendarEventSyncTask) Type() task.Type {
	return task.TypeIMAP // Uses same worker pool
}

func (t *CalendarEventSyncTask) Priority() int {
	return t.priority
}

func (t *CalendarEventSyncTask) String() string {
	return fmt.Sprintf("CalendarEventSync[%s]", t.source.Name)
}

func (t *CalendarEventSyncTask) Execute(ctx context.Context) error {
	entries, err := t.database.GetPendingCalendarEventSync(t.source.ID, 50)
	if err != nil {
		return fmt.Errorf("failed to get pending calendar event sync: %w", err)
	}

	if len(entries) == 0 {
		return nil
	}

	log.Printf("Calendar event sync: %d pending entries for %s", len(entries), t.source.Name)

	// Connect to remote CalDAV
	client := caldavclient.New(t.source, t.database)
	if err := client.Connect(); err != nil {
		return fmt.Errorf("failed to connect to CalDAV server: %w", err)
	}

	successCount := 0
	failCount := 0

	for _, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var err error
		switch entry.Operation {
		case "create", "update":
			remotePath := entry.RemoteID
			if remotePath == "" {
				// New event — construct path from calendar's remote ID and UID
				cal, calErr := t.database.GetCalendarByID(entry.CalendarID)
				if calErr != nil {
					log.Printf("Calendar event sync: failed to get calendar %d: %v", entry.CalendarID, calErr)
					failCount++
					continue
				}
				remotePath = fmt.Sprintf("%s%s.ics", cal.RemoteID, entry.UID)
			}
			err = client.PutEventRaw(ctx, remotePath, entry.ICalData)
			if err == nil && entry.RemoteID == "" {
				// Update the event's RemoteID in DB
				t.database.UpdateEventRemoteID(entry.EventID, remotePath)
			}

		case "delete":
			if entry.RemoteID == "" {
				// No remote path — nothing to delete on remote
				log.Printf("Calendar event sync: skip delete for %s (no remote ID)", entry.UID)
				t.database.DeleteCalendarEventSyncEntry(entry.ID)
				successCount++
				continue
			}
			err = client.DeleteEvent(ctx, entry.RemoteID)
		}

		if err != nil {
			log.Printf("Calendar event sync failed for %s (%s): %v", entry.UID, entry.Operation, err)
			failCount++
			continue
		}

		if err := t.database.DeleteCalendarEventSyncEntry(entry.ID); err != nil {
			log.Printf("Failed to delete calendar event sync entry %d: %v", entry.ID, err)
		}

		// Clear local_modified flag after successful push
		if entry.Operation != "delete" {
			t.database.MarkEventSynced(entry.EventID, "")
		}

		successCount++
	}

	log.Printf("Calendar event sync completed for %s: %d success, %d failed",
		t.source.Name, successCount, failCount)

	return nil
}
