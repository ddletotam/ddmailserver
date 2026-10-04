package client

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	_ "github.com/lib/pq"
)

// TestIdleWatcher_RespectsAuthPause (MAILSERVER_TEST_DSN): the IDLE watcher
// of an account whose password the provider rejected opens no connection
// while the pause lasts — its own reconnect loop used to retry every 10 s…5 min
// forever — and connects promptly once the pause is lifted (password edited).
func TestIdleWatcher_RespectsAuthPause(t *testing.T) {
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
	database.SetEncryptionKey("idle-auth-test-key")

	// The "provider": counts connections, says nothing.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var dials int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&dials, 1)
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	tag := fmt.Sprintf("zzidleauth%d", time.Now().UnixNano())
	user, err := database.CreateUser(tag, "x", "", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM auth_backoff WHERE user_id = $1`,
			`DELETE FROM account_logs WHERE account_id IN (SELECT id FROM accounts WHERE user_id = $1)`,
			`DELETE FROM accounts WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`,
		} {
			if _, err := raw.Exec(q, user.ID); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	})
	acc := &models.Account{
		UserID: user.ID, Name: "Yandex", Email: tag + "@example.org",
		IMAPHost: "127.0.0.1", IMAPPort: port, IMAPUsername: tag, IMAPPassword: "revoked", IMAPTLS: true,
		SMTPHost: "127.0.0.1", SMTPPort: 465, SMTPUsername: tag, SMTPPassword: "revoked", SMTPTLS: true,
		Enabled: true, SyncMode: "idle", PollInterval: 300,
	}
	if err := database.CreateAccount(acc); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if _, _, err := database.RecordAuthFailure(models.AuthSubjectIMAP, acc.ID, user.ID, "LOGIN invalid credentials or IMAP is disabled"); err != nil {
		t.Fatal(err)
	}

	m := NewIdleManager(database)
	m.authPollInterval = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.watchAccount(ctx, acc)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	time.Sleep(600 * time.Millisecond)
	if n := atomic.LoadInt32(&dials); n != 0 {
		t.Fatalf("watcher connected %d time(s) during the auth pause", n)
	}

	// The user saves a new password: the pause is gone.
	acc.IMAPPassword = "new"
	if err := database.UpdateAccount(acc); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&dials) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("watcher did not connect after the pause was lifted")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
