package db

import (
	"testing"

	"github.com/yourusername/mailserver/internal/timeutil"
)

// TestDeadLetterCalendarEventSync_PreservesTheBody is the guard on the whole
// point of the dead-letter table: the request body must survive the entry being
// retired. It used to be deleted along with the queue row, which is how a PUT
// that iCloud refused for four days ended up unexplainable — by the time anyone
// looked, the bytes were gone.
//
// Needs a Postgres via MAILSERVER_TEST_DSN with migrations applied; the
// INSERT … SELECT is exactly the kind of statement only a real server checks.
func TestDeadLetterCalendarEventSync_PreservesTheBody(t *testing.T) {
	db := requireTestDB(t)

	var sourceID, calendarID int64
	err := db.DB.QueryRow(`
		SELECT cs.id, c.id
		FROM calendars c
		JOIN calendar_sources cs ON cs.id = c.source_id
		LIMIT 1
	`).Scan(&sourceID, &calendarID)
	if err != nil {
		t.Skipf("no calendar/source in the test DB: %v", err)
	}

	const (
		uid  = "dead-letter-test@ddmailserver"
		body = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:dead-letter-test@ddmailserver\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	)
	queuedAt := timeutil.Now() - 60_000

	var entryID int64
	err = db.DB.QueryRow(`
		INSERT INTO calendar_event_sync_queue
			(event_id, calendar_id, source_id, uid, remote_id, ical_data, operation, created_at, retry_count, last_error, next_attempt_at)
		VALUES (-9001, $1, $2, $3, '', $4, 'create', $5, 7, 'PUT failed with status 403: ', 0)
		RETURNING id
	`, calendarID, sourceID, uid, body, queuedAt).Scan(&entryID)
	if err != nil {
		t.Fatalf("insert queue entry: %v", err)
	}

	t.Cleanup(func() {
		db.DB.Exec(`DELETE FROM calendar_event_sync_queue WHERE id = $1`, entryID)
		db.DB.Exec(`DELETE FROM calendar_event_sync_dead_letters WHERE uid = $1`, uid)
	})

	if err := db.DeadLetterCalendarEventSync(entryID, "PUT failed with status 403: "); err != nil {
		t.Fatalf("DeadLetterCalendarEventSync: %v", err)
	}

	// Gone from the queue, so it stops being retried.
	var stillQueued int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM calendar_event_sync_queue WHERE id = $1`, entryID).Scan(&stillQueued); err != nil {
		t.Fatalf("count queue: %v", err)
	}
	if stillQueued != 0 {
		t.Error("entry is still in the queue after being retired")
	}

	// Present in dead letters, byte for byte.
	var (
		gotBody      string
		gotOperation string
		gotRetries   int
		gotError     string
		gotQueuedAt  int64
		gotDiedAt    int64
	)
	err = db.DB.QueryRow(`
		SELECT ical_data, operation, retry_count, last_error, queued_at, died_at
		FROM calendar_event_sync_dead_letters WHERE uid = $1
	`, uid).Scan(&gotBody, &gotOperation, &gotRetries, &gotError, &gotQueuedAt, &gotDiedAt)
	if err != nil {
		t.Fatalf("read dead letter: %v", err)
	}

	if gotBody != body {
		t.Errorf("body was not preserved:\nwant %q\ngot  %q", body, gotBody)
	}
	if gotOperation != "create" {
		t.Errorf("operation = %q, want %q", gotOperation, "create")
	}
	if gotRetries != 7 {
		t.Errorf("retry_count = %d, want 7", gotRetries)
	}
	if gotError == "" {
		t.Error("last_error was not carried over")
	}
	if gotQueuedAt != queuedAt {
		t.Errorf("queued_at = %d, want the original %d — when the change was made matters", gotQueuedAt, queuedAt)
	}
	if gotDiedAt <= queuedAt {
		t.Errorf("died_at = %d, want it stamped after queued_at (%d)", gotDiedAt, queuedAt)
	}
}

// TestDeadLetterCalendarEventSync_MissingEntryIsNotAnError: the worker calls this
// on a best-effort basis, and a racing drain must not turn into a logged failure.
func TestDeadLetterCalendarEventSync_MissingEntryIsNotAnError(t *testing.T) {
	db := requireTestDB(t)

	if err := db.DeadLetterCalendarEventSync(-424242, "whatever"); err != nil {
		t.Errorf("retiring a non-existent entry returned %v, want no error", err)
	}
}
