package db

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"
)

// requireTestDB opens the integration-test database via MAILSERVER_TEST_DSN
// and skips the test when the env var is unset. The DSN should point at a
// non-prod Postgres with the project's schema applied.
func requireTestDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("MAILSERVER_TEST_DSN")
	if dsn == "" {
		t.Skip("set MAILSERVER_TEST_DSN to a Postgres DSN to run DB integration tests")
	}
	raw, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := raw.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return &DB{DB: raw}
}

// TestMarkCalendarEventSyncFailed_UpdatesRow guards against the pq
// "inconsistent types deduced for parameter $N" class of bug we hit in
// production: when the same placeholder appears in two arithmetic
// contexts, the driver occasionally fails to prepare. The function must
// (a) succeed, (b) bump retry_count, (c) persist last_error,
// (d) schedule next_attempt_at one minute out for the first retry.
//
// Requires a Postgres reachable via MAILSERVER_TEST_DSN with the
// migrations from /migrations applied AND at least one caldav source +
// calendar present (so FK constraints on the queue row are satisfiable).
// The test creates one queue row and removes it on cleanup.
func TestMarkCalendarEventSyncFailed_UpdatesRow(t *testing.T) {
	db := requireTestDB(t)

	var sourceID, calendarID int64
	err := db.DB.QueryRow(`
		SELECT cs.id, c.id
		FROM calendars c
		JOIN calendar_sources cs ON cs.id = c.source_id
		WHERE cs.source_type = 'caldav'
		LIMIT 1
	`).Scan(&sourceID, &calendarID)
	if err != nil {
		t.Skipf("no caldav calendar/source in the test DB: %v", err)
	}

	var queueID int64
	err = db.DB.QueryRow(`
		INSERT INTO calendar_event_sync_queue
		    (event_id, calendar_id, source_id, uid, ical_data, operation,
		     created_at, retry_count, last_error, last_attempt_at, next_attempt_at)
		VALUES (NULL, $1, $2, $3, '', 'create',
		        0, 0, '', 0, 0)
		RETURNING id
	`, calendarID, sourceID, "test-uid-mark-failed").Scan(&queueID)
	if err != nil {
		t.Fatalf("insert queue row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.DB.Exec(`DELETE FROM calendar_event_sync_queue WHERE id = $1`, queueID)
	})

	if err := db.MarkCalendarEventSyncFailed(queueID, "boom: PUT returned 400"); err != nil {
		t.Fatalf("MarkCalendarEventSyncFailed: %v", err)
	}

	var retryCount int
	var lastError string
	var lastAttemptAt, nextAttemptAt int64
	err = db.DB.QueryRow(`
		SELECT retry_count, last_error, last_attempt_at, next_attempt_at
		FROM calendar_event_sync_queue WHERE id = $1
	`, queueID).Scan(&retryCount, &lastError, &lastAttemptAt, &nextAttemptAt)
	if err != nil {
		t.Fatalf("select after update: %v", err)
	}

	if retryCount != 1 {
		t.Errorf("retry_count = %d, want 1", retryCount)
	}
	if !strings.Contains(lastError, "PUT returned 400") {
		t.Errorf("last_error = %q, want substring 'PUT returned 400'", lastError)
	}
	if nextAttemptAt <= lastAttemptAt {
		t.Errorf("next_attempt_at (%d) must be > last_attempt_at (%d)", nextAttemptAt, lastAttemptAt)
	}
	// First retry → 60_000 ms backoff per the function's schedule.
	if delta := nextAttemptAt - lastAttemptAt; delta != 60000 {
		t.Errorf("first-retry delta = %d ms, want 60000", delta)
	}
}
