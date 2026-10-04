package db

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// TestClaimOutboxMessage_ExactlyOneWinner is the double-send regression. Two
// send tasks for one row (two outbox scans racing — the periodic tick and
// TriggerOutbox — or a stale task still queued) both set status='sending'
// unconditionally and both delivered. The claim must hand the row to exactly
// one of any number of concurrent callers.
func TestClaimOutboxMessage_ExactlyOneWinner(t *testing.T) {
	db := requireTestDB(t)

	for round := 0; round < 20; round++ {
		id := insertTestOutboxMessage(t, db, "pending")

		const contenders = 8
		var winners int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < contenders; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ok, err := db.ClaimOutboxMessage(id)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if ok {
					atomic.AddInt32(&winners, 1)
				}
			}()
		}
		close(start)
		wg.Wait()

		if winners != 1 {
			t.Fatalf("round %d: %d callers claimed message %d, want exactly 1", round, winners, id)
		}
		var status string
		if err := db.DB.QueryRow(`SELECT status FROM outbox_messages WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "sending" {
			t.Fatalf("status after claim = %q, want sending", status)
		}
	}
}

// TestClaimOutboxMessage_RefusesNonPending: rows already being sent, sent,
// failed, or still backing off after a failure are not claimable — a task
// created before the state changed must not send.
func TestClaimOutboxMessage_RefusesNonPending(t *testing.T) {
	db := requireTestDB(t)

	for _, status := range []string{"sending", "sent", "failed"} {
		id := insertTestOutboxMessage(t, db, status)
		ok, err := db.ClaimOutboxMessage(id)
		if err != nil {
			t.Fatalf("claim %s: %v", status, err)
		}
		if ok {
			t.Errorf("claimed a message in status %q", status)
		}
	}

	// Pending, but the backoff after a failed attempt has not elapsed.
	id := insertTestOutboxMessage(t, db, "pending")
	if _, err := db.DB.Exec(`UPDATE outbox_messages SET next_attempt_at = $1 WHERE id = $2`,
		timeutil.Now()+60_000, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ClaimOutboxMessage(id); err != nil || ok {
		t.Errorf("claim during backoff: ok=%v err=%v, want refused", ok, err)
	}

	// A message that no longer exists (deleted by the user) is simply not won.
	if ok, err := db.ClaimOutboxMessage(-1); err != nil || ok {
		t.Errorf("claim of missing row: ok=%v err=%v, want false, nil", ok, err)
	}
}

// TestClaimAfterRecovery: a row stranded in 'sending' by a dead process is
// claimable again only after RecoverStrandedOutboxMessages, and then by one
// caller only.
func TestClaimAfterRecovery(t *testing.T) {
	db := requireTestDB(t)
	id := insertTestOutboxMessage(t, db, "sending")

	if ok, _ := db.ClaimOutboxMessage(id); ok {
		t.Fatal("claimed a row still in 'sending'")
	}
	if _, err := db.RecoverStrandedOutboxMessages(); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ClaimOutboxMessage(id); err != nil || !ok {
		t.Fatalf("claim after recovery: ok=%v err=%v", ok, err)
	}
	if ok, _ := db.ClaimOutboxMessage(id); ok {
		t.Fatal("second claim after recovery succeeded")
	}
}
