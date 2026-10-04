package worker

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/notify"
	_ "github.com/lib/pq"
)

func authTestDB(t *testing.T) (*db.DB, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("MAILSERVER_TEST_DSN")
	if dsn == "" {
		t.Skip("set MAILSERVER_TEST_DSN to a Postgres DSN to run DB integration tests")
	}
	raw, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	database := &db.DB{DB: raw}
	database.SetEncryptionKey("worker-auth-test-key")
	return database, raw
}

func authTestAccount(t *testing.T, database *db.DB, raw *sql.DB) (*models.User, *models.Account) {
	t.Helper()
	tag := fmt.Sprintf("zzwauth%d", time.Now().UnixNano())
	user, err := database.CreateUser(tag, "x", "", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM auth_backoff WHERE user_id = $1`,
			`DELETE FROM flag_sync_queue WHERE account_id IN (SELECT id FROM accounts WHERE user_id = $1)`,
			`DELETE FROM accounts WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`,
		} {
			if _, err := raw.Exec(q, user.ID); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	})
	acc := &models.Account{
		UserID: user.ID, Name: "Yandex 360", Email: tag + "@example.org",
		IMAPHost: "127.0.0.1", IMAPPort: 1, IMAPUsername: tag, IMAPPassword: "revoked", IMAPTLS: true,
		SMTPHost: "127.0.0.1", SMTPPort: 1, SMTPUsername: tag, SMTPPassword: "revoked", SMTPTLS: true,
		Enabled: true, SyncMode: "poll", PollInterval: 300,
	}
	if err := database.CreateAccount(acc); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	return user, acc
}

// queued reports whether the scheduler put t in the pool: a second Submit of
// the same logical task is refused as a duplicate only if it is waiting there.
func queued(t *testing.T, p *Pool, task Task) bool {
	t.Helper()
	err := p.Submit(task)
	if err != nil && !errors.Is(err, ErrDuplicateTask) {
		t.Fatalf("submit: %v", err)
	}
	return errors.Is(err, ErrDuplicateTask)
}

// TestScheduler_SkipsPausedAccount (MAILSERVER_TEST_DSN): with the IMAP login
// paused, the flag-sync worker is not even submitted; once the pause is
// lifted (password edited) it is, on the very next pass.
func TestScheduler_SkipsPausedAccount(t *testing.T) {
	database, raw := authTestDB(t)
	user, acc := authTestAccount(t, database, raw)
	if _, err := raw.Exec(`
		INSERT INTO flag_sync_queue (message_id, account_id, remote_folder, remote_uid, seen, flagged, answered, deleted)
		VALUES ($1, $2, 'INBOX', 17, true, false, false, false)`, -time.Now().UnixNano(), acc.ID); err != nil {
		t.Fatalf("insert flag sync entry: %v", err)
	}
	if _, _, err := database.RecordAuthFailure(models.AuthSubjectIMAP, acc.ID, user.ID, "LOGIN invalid credentials or IMAP is disabled"); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(1, 1, 1000)
	s := NewScheduler(SchedulerDeps{Pool: pool, Database: database, IntervalSeconds: 60})
	s.scheduleFlagSync(s.loadAuthPaused())
	if queued(t, pool, NewFlagSyncTask(acc, database)) {
		t.Fatal("flag sync submitted for a paused account")
	}

	acc.IMAPPassword = "new-app-password"
	if err := database.UpdateAccount(acc); err != nil {
		t.Fatal(err)
	}
	pool2 := NewPool(1, 1, 1000)
	s2 := NewScheduler(SchedulerDeps{Pool: pool2, Database: database, IntervalSeconds: 60})
	s2.scheduleFlagSync(s2.loadAuthPaused())
	if !queued(t, pool2, NewFlagSyncTask(acc, database)) {
		t.Fatal("flag sync not submitted after the pause was lifted")
	}
}

// TestScheduler_PushesAuthFailureOnce (MAILSERVER_TEST_DSN): the user hears
// about the rejected password once, on the transition — not every tick.
func TestScheduler_PushesAuthFailureOnce(t *testing.T) {
	database, raw := authTestDB(t)
	user, acc := authTestAccount(t, database, raw)
	hub := notify.NewHub()
	ch := hub.Subscribe(user.ID)
	defer hub.Unsubscribe(user.ID, ch)

	s := NewScheduler(SchedulerDeps{Pool: NewPool(1, 1, 10), Database: database, IntervalSeconds: 60, NotifyHub: hub})
	st, _, err := database.RecordAuthFailure(models.AuthSubjectSMTP, acc.ID, user.ID, "535 5.7.8 authentication failed")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		s.publishAuthFailures()
	}

	var got []notify.Event
	timeout := time.After(500 * time.Millisecond)
collect:
	for {
		select {
		case ev := <-ch:
			if ev.Type == notify.EventAuthFailed {
				got = append(got, ev)
			}
		case <-timeout:
			break collect
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d auth_failed pushes, want 1", len(got))
	}
	ev := got[0]
	if ev.Identity != acc.Email || ev.Service != "smtp" || ev.Since != st.FirstFailureAt || ev.NextAttemptAt != st.NextAttemptAt {
		t.Fatalf("push: %+v", ev)
	}
}
