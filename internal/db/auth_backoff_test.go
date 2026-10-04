package db

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// authTestAccount creates a throwaway user with one external account and
// removes both (and their auth_backoff rows) afterwards.
func authTestAccount(t *testing.T, db *DB) (*models.User, *models.Account) {
	t.Helper()
	db.SetEncryptionKey("auth-backoff-test-key")
	tag := fmt.Sprintf("zzauth%d", time.Now().UnixNano())
	user, err := db.CreateUser(tag, "x", "", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM auth_backoff WHERE user_id = $1`,
			`DELETE FROM accounts WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`,
		} {
			if _, err := db.DB.Exec(q, user.ID); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	})
	acc := &models.Account{
		UserID: user.ID, Name: "Yandex", Email: tag + "@example.org",
		IMAPHost: "imap.example.org", IMAPPort: 993, IMAPUsername: tag, IMAPPassword: "old", IMAPTLS: true,
		SMTPHost: "smtp.example.org", SMTPPort: 465, SMTPUsername: tag, SMTPPassword: "old", SMTPTLS: true,
		Enabled: false, SyncMode: "poll", PollInterval: 300,
	}
	if err := db.CreateAccount(acc); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	return user, acc
}

// TestAuthBackoff_Lifecycle: rejection → pause → expired pause leases one
// attempt → success clears.
func TestAuthBackoff_Lifecycle(t *testing.T) {
	db := requireTestDB(t)
	user, acc := authTestAccount(t, db)
	kind := models.AuthSubjectIMAP

	ok, st, err := db.AcquireAuthAttempt(kind, acc.ID)
	if err != nil || !ok || st != nil {
		t.Fatalf("fresh subject: ok=%v st=%v err=%v", ok, st, err)
	}

	st, counted, err := db.RecordAuthFailure(kind, acc.ID, user.ID, "LOGIN invalid credentials or IMAP is disabled")
	if err != nil || !counted {
		t.Fatalf("RecordAuthFailure: counted=%v err=%v", counted, err)
	}
	if st.Failures != 1 || st.NextAttemptAt-st.FirstFailureAt != 60_000 || st.NotifiedAt != 0 {
		t.Fatalf("first failure: %+v", st)
	}

	// Paused.
	if ok, _, err := db.AcquireAuthAttempt(kind, acc.ID); err != nil || ok {
		t.Fatalf("acquire during pause: ok=%v err=%v", ok, err)
	}
	paused, err := db.GetAuthPausedIDs(kind)
	if err != nil || !paused[acc.ID] {
		t.Fatalf("GetAuthPausedIDs: %v %v", paused, err)
	}
	if other, _ := db.GetAuthPausedIDs(models.AuthSubjectSMTP); other[acc.ID] {
		t.Fatal("IMAP pause leaked into SMTP")
	}

	// A second rejection in the same burst does not escalate.
	st2, counted, err := db.RecordAuthFailure(kind, acc.ID, user.ID, "again")
	if err != nil || counted || st2.Failures != 1 || st2.NextAttemptAt != st.NextAttemptAt {
		t.Fatalf("burst: counted=%v st=%+v err=%v", counted, st2, err)
	}

	// Let the pause run out (moved into the past rather than waited for).
	if _, err := db.DB.Exec(`UPDATE auth_backoff SET next_attempt_at = $3::bigint, last_failure_at = $3::bigint - 60000, first_failure_at = $3::bigint - 60000
		WHERE subject_kind = $1 AND subject_id = $2`, kind, acc.ID, timeutil.Now()-1); err != nil {
		t.Fatal(err)
	}
	ok, st, err = db.AcquireAuthAttempt(kind, acc.ID)
	if err != nil || !ok || st == nil {
		t.Fatalf("acquire after pause: ok=%v st=%v err=%v", ok, st, err)
	}
	// The lease holds the window for this one attempt.
	if ok, _, _ := db.AcquireAuthAttempt(kind, acc.ID); ok {
		t.Fatal("second path got the same window")
	}

	st, counted, err = db.RecordAuthFailure(kind, acc.ID, user.ID, "still")
	if err != nil || !counted || st.Failures != 2 {
		t.Fatalf("retry rejected: counted=%v st=%+v err=%v", counted, st, err)
	}
	if got := st.NextAttemptAt - st.LastFailureAt; got != 5*60_000 {
		t.Fatalf("second pause %d ms, want 5 min", got)
	}

	prev, err := db.ClearAuthBackoff(kind, acc.ID)
	if err != nil || prev == nil || prev.Failures != 2 {
		t.Fatalf("clear: prev=%+v err=%v", prev, err)
	}
	if prev, err := db.ClearAuthBackoff(kind, acc.ID); err != nil || prev != nil {
		t.Fatalf("clear twice: prev=%+v err=%v", prev, err)
	}
	if st, err := db.GetAuthBackoff(kind, acc.ID); err != nil || st != nil {
		t.Fatalf("after clear: %+v %v", st, err)
	}
}

// TestAuthBackoff_OneWinnerPerWindow: when the pause ends, IDLE, the poll,
// flag sync and the outbox ask at once — exactly one may log in.
func TestAuthBackoff_OneWinnerPerWindow(t *testing.T) {
	db := requireTestDB(t)
	user, acc := authTestAccount(t, db)
	kind := models.AuthSubjectIMAP
	if _, _, err := db.RecordAuthFailure(kind, acc.ID, user.ID, "nope"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`UPDATE auth_backoff SET next_attempt_at = 1 WHERE subject_kind = $1 AND subject_id = $2`, kind, acc.ID); err != nil {
		t.Fatal(err)
	}

	var winners int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, _, err := db.AcquireAuthAttempt(kind, acc.ID)
			if err != nil {
				t.Errorf("acquire: %v", err)
			}
			if ok {
				atomic.AddInt32(&winners, 1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if winners != 1 {
		t.Fatalf("%d winners, want 1", winners)
	}
}

// TestAuthBackoff_ConcurrentFirstFailures: several paths failing at the same
// moment make one row with one counted failure, not an insert conflict.
func TestAuthBackoff_ConcurrentFirstFailures(t *testing.T) {
	db := requireTestDB(t)
	user, acc := authTestAccount(t, db)
	var wg sync.WaitGroup
	var counted int32
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, c, err := db.RecordAuthFailure(models.AuthSubjectCalDAV, acc.ID, user.ID, "401")
			if err != nil {
				t.Errorf("record: %v", err)
			}
			if c {
				atomic.AddInt32(&counted, 1)
			}
		}()
	}
	wg.Wait()
	st, err := db.GetAuthBackoff(models.AuthSubjectCalDAV, acc.ID)
	if err != nil || st == nil || st.Failures != 1 {
		t.Fatalf("state: %+v %v", st, err)
	}
	if counted != 1 {
		t.Fatalf("%d counted, want 1", counted)
	}
}

// TestAuthBackoff_UpdateAccountResets: saving the account (new password in
// the web form, the API, a .mobileconfig replace) lifts both pauses at once.
func TestAuthBackoff_UpdateAccountResets(t *testing.T) {
	db := requireTestDB(t)
	user, acc := authTestAccount(t, db)
	for _, kind := range []string{models.AuthSubjectIMAP, models.AuthSubjectSMTP} {
		if _, _, err := db.RecordAuthFailure(kind, acc.ID, user.ID, "nope"); err != nil {
			t.Fatal(err)
		}
	}
	acc.IMAPPassword = "new-app-password"
	acc.SMTPPassword = "new-app-password"
	if err := db.UpdateAccount(acc); err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}
	for _, kind := range []string{models.AuthSubjectIMAP, models.AuthSubjectSMTP} {
		if ok, _, err := db.AcquireAuthAttempt(kind, acc.ID); err != nil || !ok {
			t.Fatalf("%s after edit: ok=%v err=%v", kind, ok, err)
		}
	}

	// The transactional path of the .mobileconfig import too.
	if _, _, err := db.RecordAuthFailure(models.AuthSubjectIMAP, acc.ID, user.ID, "nope"); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.UpdateAccount(acc); err != nil {
		_ = tx.Rollback()
		t.Fatalf("tx.UpdateAccount: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if st, _ := db.GetAuthBackoff(models.AuthSubjectIMAP, acc.ID); st != nil {
		t.Fatalf("tx edit left the pause: %+v", st)
	}
}

// TestAuthBackoff_NotifyOnce: the scheduler's push reads unnotified states
// and marks them; a new incident after recovery is unnotified again.
func TestAuthBackoff_NotifyOnce(t *testing.T) {
	db := requireTestDB(t)
	user, acc := authTestAccount(t, db)
	st, _, err := db.RecordAuthFailure(models.AuthSubjectIMAP, acc.ID, user.ID, "nope")
	if err != nil {
		t.Fatal(err)
	}
	if !hasState(t, db, acc.ID) {
		t.Fatal("new state not in the unnotified list")
	}
	if err := db.MarkAuthBackoffNotified(models.AuthSubjectIMAP, acc.ID, st.FirstFailureAt); err != nil {
		t.Fatal(err)
	}
	if hasState(t, db, acc.ID) {
		t.Fatal("notified state still listed")
	}
	// Further rejections of the same incident stay notified.
	if _, err := db.DB.Exec(`UPDATE auth_backoff SET last_failure_at = last_failure_at - 120000 WHERE subject_kind = $1 AND subject_id = $2`,
		models.AuthSubjectIMAP, acc.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.RecordAuthFailure(models.AuthSubjectIMAP, acc.ID, user.ID, "again"); err != nil {
		t.Fatal(err)
	}
	if hasState(t, db, acc.ID) {
		t.Fatal("repeated rejection re-notified")
	}
	// Recovery, then a new incident.
	if _, err := db.ClearAuthBackoff(models.AuthSubjectIMAP, acc.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.RecordAuthFailure(models.AuthSubjectIMAP, acc.ID, user.ID, "revoked again"); err != nil {
		t.Fatal(err)
	}
	if !hasState(t, db, acc.ID) {
		t.Fatal("new incident not notified")
	}
}

func hasState(t *testing.T, db *DB, accountID int64) bool {
	t.Helper()
	states, err := db.GetUnnotifiedAuthBackoffs()
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range states {
		if st.SubjectKind == models.AuthSubjectIMAP && st.SubjectID == accountID {
			return true
		}
	}
	return false
}

// TestAuthBackoff_AttachAndCleanup: the account list gets the state for
// display; a deleted account's state is removed by the daily cleanup.
func TestAuthBackoff_AttachAndCleanup(t *testing.T) {
	db := requireTestDB(t)
	user, acc := authTestAccount(t, db)
	if _, _, err := db.RecordAuthFailure(models.AuthSubjectSMTP, acc.ID, user.ID, "535 5.7.8"); err != nil {
		t.Fatal(err)
	}
	accounts, err := db.GetAccountsByUserID(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AttachAccountAuthFailures(user.ID, accounts); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || len(accounts[0].AuthFailures) != 1 || accounts[0].AuthFailures[0].Service != "smtp" {
		t.Fatalf("attached: %+v", accounts[0].AuthFailures)
	}

	if err := db.DeleteAccount(acc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CleanupOrphanAuthBackoffs(); err != nil {
		t.Fatal(err)
	}
	if st, _ := db.GetAuthBackoff(models.AuthSubjectSMTP, acc.ID); st != nil {
		t.Fatalf("orphan survived: %+v", st)
	}
}
