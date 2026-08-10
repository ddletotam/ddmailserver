package db

import (
	"testing"

	"github.com/yourusername/mailserver/internal/timeutil"
)

// insertTestOutboxMessage creates a throwaway queued message and removes it on
// cleanup. It borrows an existing user so the FK on user_id is satisfiable.
func insertTestOutboxMessage(t *testing.T, db *DB, status string) int64 {
	t.Helper()

	var userID int64
	if err := db.DB.QueryRow(`SELECT id FROM users LIMIT 1`).Scan(&userID); err != nil {
		t.Skipf("no user in the test DB: %v", err)
	}

	now := timeutil.Now()
	var id int64
	err := db.DB.QueryRow(`
		INSERT INTO outbox_messages (user_id, from_addr, to_addr, subject, body, status, retries, created_at, updated_at, next_attempt_at)
		VALUES ($1, 'sender@example.org', 'rcpt@example.org', 'retry bookkeeping test', 'body', $2, 0, $3, $3, 0)
		RETURNING id
	`, userID, status, now).Scan(&id)
	if err != nil {
		t.Fatalf("insert test outbox message: %v", err)
	}

	t.Cleanup(func() {
		if _, err := db.DB.Exec(`DELETE FROM outbox_messages WHERE id = $1`, id); err != nil {
			t.Logf("cleanup of outbox message %d failed: %v", id, err)
		}
	})

	return id
}

// TestIncrementOutboxMessageRetries_CountsAndBacksOff is the regression for a
// statement that failed every single time it ran.
//
// $2 appeared both as a bigint column value and as the left operand of an
// addition with integer literals, and Postgres refused to deduce one type for
// both: "inconsistent types deduced for parameter $2". The error was logged and
// swallowed, so nothing looked broken — but no attempt was ever counted and
// next_attempt_at stayed 0, which meant a permanently rejected message was
// retried on every scheduler cycle forever instead of backing off six times and
// being marked failed. One message spent a day and a half retrying once a
// minute.
//
// The same bug class was already fixed once in MarkCalendarEventSyncFailed; only
// a real Postgres catches it, which is why this test needs MAILSERVER_TEST_DSN.
func TestIncrementOutboxMessageRetries_CountsAndBacksOff(t *testing.T) {
	db := requireTestDB(t)
	id := insertTestOutboxMessage(t, db, "sending")

	before := timeutil.Now()

	// First failure: counter reaches 1, next attempt a minute out.
	retries, err := db.IncrementOutboxMessageRetries(id, "first failure")
	if err != nil {
		t.Fatalf("IncrementOutboxMessageRetries: %v", err)
	}
	if retries != 1 {
		t.Errorf("returned retries = %d, want 1", retries)
	}

	var stored int
	var nextAttempt int64
	var lastError string
	err = db.DB.QueryRow(`
		SELECT COALESCE(retries, 0), next_attempt_at, COALESCE(last_error, '')
		FROM outbox_messages WHERE id = $1
	`, id).Scan(&stored, &nextAttempt, &lastError)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != 1 {
		t.Errorf("stored retries = %d, want 1", stored)
	}
	if lastError != "first failure" {
		t.Errorf("last_error = %q, want %q", lastError, "first failure")
	}
	if delay := nextAttempt - before; delay < 55_000 || delay > 65_000 {
		t.Errorf("first retry scheduled %d ms out, want about 60000", delay)
	}

	// Second failure: the backoff grows rather than repeating.
	retries, err = db.IncrementOutboxMessageRetries(id, "second failure")
	if err != nil {
		t.Fatalf("IncrementOutboxMessageRetries (second): %v", err)
	}
	if retries != 2 {
		t.Errorf("returned retries = %d, want 2", retries)
	}

	if err := db.DB.QueryRow(`SELECT next_attempt_at FROM outbox_messages WHERE id = $1`, id).Scan(&nextAttempt); err != nil {
		t.Fatalf("read back second: %v", err)
	}
	if delay := nextAttempt - before; delay < 295_000 || delay > 310_000 {
		t.Errorf("second retry scheduled %d ms out, want about 300000", delay)
	}
}

// TestRecoverStrandedOutboxMessages_FreesSendingRows covers the other way a
// message goes quiet: 'sending' is set just before a send begins and cleared
// only when it ends, so a restart in between leaves a row the scheduler will
// never look at again. One had been sitting there for two months.
func TestRecoverStrandedOutboxMessages_FreesSendingRows(t *testing.T) {
	db := requireTestDB(t)
	stranded := insertTestOutboxMessage(t, db, "sending")
	untouched := insertTestOutboxMessage(t, db, "failed")

	if _, err := db.RecoverStrandedOutboxMessages(); err != nil {
		t.Fatalf("RecoverStrandedOutboxMessages: %v", err)
	}

	var status string
	if err := db.DB.QueryRow(`SELECT status FROM outbox_messages WHERE id = $1`, stranded).Scan(&status); err != nil {
		t.Fatalf("read back stranded: %v", err)
	}
	if status != "pending" {
		t.Errorf("stranded message status = %q, want %q", status, "pending")
	}

	// Recovery must not resurrect anything that was deliberately given up on.
	if err := db.DB.QueryRow(`SELECT status FROM outbox_messages WHERE id = $1`, untouched).Scan(&status); err != nil {
		t.Fatalf("read back failed message: %v", err)
	}
	if status != "failed" {
		t.Errorf("failed message status = %q, want it left alone", status)
	}
}
