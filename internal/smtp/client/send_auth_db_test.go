package client

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
	_ "github.com/lib/pq"
)

// fakeSubmission is an SMTP server on 127.0.0.1 that rejects every AUTH with
// 535 and counts AUTH attempts.
func fakeSubmission(t *testing.T) (port int, auths *int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var n int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				w := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
				w("220 fake ESMTP")
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					f := strings.Fields(line)
					if len(f) == 0 {
						continue
					}
					switch strings.ToUpper(f[0]) {
					case "EHLO", "HELO":
						w("250-fake")
						w("250 AUTH PLAIN LOGIN")
					case "AUTH":
						atomic.AddInt32(&n, 1)
						w("535 5.7.8 Error: authentication failed: Invalid user or password!")
					case "QUIT":
						w("221 bye")
						return
					default:
						w("250 ok")
					}
				}
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, &n
}

// TestSendTask_RejectedPasswordDefersMessage (MAILSERVER_TEST_DSN): a relay
// send refused with 535 leaves the message pending with a readable reason and
// its retry budget untouched (six 535s used to mark it failed — lost), pauses
// the account's SMTP login, and the next task during the pause does not even
// connect.
func TestSendTask_RejectedPasswordDefersMessage(t *testing.T) {
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
	database.SetEncryptionKey("send-auth-test-key")

	port, auths := fakeSubmission(t)

	tag := fmt.Sprintf("zzsendauth%d", time.Now().UnixNano())
	user, err := database.CreateUser(tag, "x", "", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM auth_backoff WHERE user_id = $1`,
			`DELETE FROM outbox_messages WHERE user_id = $1`,
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
		IMAPHost: "127.0.0.1", IMAPPort: 993, IMAPUsername: tag, IMAPPassword: "revoked", IMAPTLS: true,
		// Plain SMTP to 127.0.0.1: net/smtp allows PLAIN to localhost.
		SMTPHost: "127.0.0.1", SMTPPort: port, SMTPUsername: tag, SMTPPassword: "revoked", SMTPTLS: false,
		Enabled: true, SyncMode: "poll", PollInterval: 300,
	}
	if err := database.CreateAccount(acc); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	now := timeutil.Now()
	newMessage := func() *models.OutboxMessage {
		var id int64
		if err := raw.QueryRow(`
			INSERT INTO outbox_messages (user_id, account_id, from_addr, to_addr, cc, bcc, subject, body, body_html, status, retries, last_error, created_at, updated_at, next_attempt_at)
			VALUES ($1, $2, $3, 'rcpt@example.org', '', '', 'auth deferral test', 'body', '', 'pending', 0, '', $4, $4, 0)
			RETURNING id`, user.ID, acc.ID, acc.Email, now).Scan(&id); err != nil {
			t.Fatalf("insert outbox: %v", err)
		}
		msg, err := database.GetOutboxMessageByID(id)
		if err != nil {
			t.Fatal(err)
		}
		return msg
	}

	msg := newMessage()
	if err := NewSendTask(msg, acc, database).Execute(context.Background()); err == nil {
		t.Fatal("send with a rejected password succeeded")
	}
	var status, lastError string
	var retries int
	if err := raw.QueryRow(`SELECT status, retries, last_error FROM outbox_messages WHERE id = $1`, msg.ID).
		Scan(&status, &retries, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || retries != 0 || !strings.HasPrefix(lastError, "deferred:") {
		t.Fatalf("message after 535: status=%q retries=%d last_error=%q", status, retries, lastError)
	}
	st, err := database.GetAuthBackoff(models.AuthSubjectSMTP, acc.ID)
	if err != nil || st == nil || st.Failures != 1 {
		t.Fatalf("SMTP pause: %+v %v", st, err)
	}
	if imap, _ := database.GetAuthBackoff(models.AuthSubjectIMAP, acc.ID); imap != nil {
		t.Fatal("SMTP rejection paused IMAP")
	}

	// During the pause: no connection at all, message untouched.
	before := atomic.LoadInt32(auths)
	msg2 := newMessage()
	if err := NewSendTask(msg2, acc, database).Execute(context.Background()); err != nil {
		t.Fatalf("paused send: %v", err)
	}
	if got := atomic.LoadInt32(auths); got != before {
		t.Fatalf("paused send reached the server (%d AUTH)", got-before)
	}
	if err := raw.QueryRow(`SELECT status, retries FROM outbox_messages WHERE id = $1`, msg2.ID).Scan(&status, &retries); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || retries != 0 {
		t.Fatalf("paused message: status=%q retries=%d", status, retries)
	}
}
