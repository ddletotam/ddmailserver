package worker

import (
	"context"
	"fmt"
	"log"
	"strings"

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
			log.Printf("Calendar event sync failed for %s (%s, retry=%d): %v",
				entry.UID, entry.Operation, entry.RetryCount, err)
			// Permanent-failure cutoff: a 4xx that survived this many
			// backoff rounds (the tail is 24h apart) will never succeed.
			// Canonical case: Google's auto-generated birthday events are
			// read-only — any PUT gets 400 forever, and the daily retry
			// kept the warning emails firing daily. Drop the entry and
			// clear local_modified so the next pull re-adopts the remote
			// truth instead of shielding the stale local copy.
			const giveUpAfter = 8
			if entry.RetryCount+1 >= giveUpAfter && isPermanentSyncError(err) {
				log.Printf("Calendar event sync: giving up on %s (%s) after %d attempts — retiring to dead letters; remote state wins",
					entry.UID, entry.Operation, entry.RetryCount+1)
				// Retired, not deleted: the body is the only evidence of what
				// the server refused, and deleting it once cost days of
				// guesswork (see migrations/046).
				if dbErr := t.database.DeadLetterCalendarEventSync(entry.ID, err.Error()); dbErr != nil {
					log.Printf("retire poisoned sync entry %d: %v", entry.ID, dbErr)
				}
				if entry.Operation != "delete" {
					t.database.MarkEventSynced(entry.EventID, "")
				}
				failCount++
				continue
			}
			// Persist failure for backoff + DLQ warning logic. Best-effort:
			// if we can't update the DB the entry just re-fires on the next
			// cycle, which is still correct (just chattier).
			if dbErr := t.database.MarkCalendarEventSyncFailed(entry.ID, err.Error()); dbErr != nil {
				log.Printf("MarkCalendarEventSyncFailed: %v", dbErr)
			}
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
	return t.finish(failCount)
}

// isPermanentSyncError reports whether the remote rejected the operation in
// a way that retrying can't fix: client errors except auth-shaped ones
// (401/407/429 can heal after re-auth or backoff; 5xx is the remote's
// problem and worth retrying).
func isPermanentSyncError(err error) bool {
	s := err.Error()
	for _, code := range []string{"status 400", "status 403", "status 404", "status 405", "status 409", "status 410", "status 412", "status 415"} {
		if strings.Contains(s, code) {
			return true
		}
	}
	return false
}

// finish resets the warning counter when the round was clean and the queue
// drained — the daily reminder loop should stop then.
func (t *CalendarEventSyncTask) finish(failCount int) error {
	if failCount == 0 {
		left, _ := t.database.CountPendingCalendarSyncFailures(t.source.ID, 1)
		if left == 0 {
			_ = t.database.SetCalendarSourceWarning(t.source.ID, 0, 0)
		}
	}

	return nil
}
