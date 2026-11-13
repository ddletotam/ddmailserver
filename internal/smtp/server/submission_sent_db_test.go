package server

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/notify"
	"github.com/ddletotam/ddmailserver/internal/outbox"
	"github.com/ddletotam/ddmailserver/migrations"
	_ "github.com/lib/pq"
)

// TestSubmission_CopyInSentAtAcceptance (MAILSERVER_TEST_DSN): a message
// submitted over SMTP is in Sent, «pending», the moment the server answers
// 250 — not after delivery — the clients hear message_sent, and the outbox is
// kicked.
func TestSubmission_CopyInSentAtAcceptance(t *testing.T) {
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
	if err := database.RunMigrations(context.Background(), migrations.FS()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	tag := fmt.Sprintf("zzsubm%d", time.Now().UnixNano())
	user, err := database.CreateUser(tag, "x", "", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM messages WHERE user_id = $1`,
			`DELETE FROM message_changes WHERE user_id = $1`,
			`DELETE FROM outbox_messages WHERE user_id = $1`,
			`DELETE FROM folders WHERE user_id = $1`,
			`DELETE FROM mailboxes WHERE user_id = $1`,
			`DELETE FROM domains WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`,
		} {
			if _, err := raw.Exec(q, user.ID); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	})
	domain := tag + ".test"
	d := &models.Domain{Domain: domain, UserID: user.ID, Enabled: true}
	if err := database.CreateDomain(d); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateMailbox(&models.Mailbox{UserID: user.ID, DomainID: d.ID, LocalPart: "me", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	hub := notify.NewHub()
	events := hub.Subscribe(user.ID)
	defer hub.Unsubscribe(user.ID, events)
	kicked := 0
	s := &Session{database: database, hub: hub, kickOutbox: func() { kicked++ }, userID: user.ID, username: tag}

	me := "me@" + domain
	if err := s.Mail(me, nil); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if err := s.Rcpt("someone@dead-"+domain, nil); err != nil {
		t.Fatalf("RCPT: %v", err)
	}
	msgID := "subm." + tag + "@" + domain
	body := "From: " + me + "\r\nTo: someone@dead-" + domain + "\r\nSubject: test\r\nMessage-ID: <" + msgID + ">\r\n\r\nhi\r\n"
	if err := s.Data(strings.NewReader(body)); err != nil {
		t.Fatalf("DATA: %v", err)
	}

	var status, folderType string
	if err := raw.QueryRow(`SELECT m.delivery_status, f.type FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.user_id = $1 AND m.message_id = $2`, user.ID, msgID).Scan(&status, &folderType); err != nil {
		t.Fatalf("no copy of the submitted message: %v", err)
	}
	if status != outbox.StatusPending || folderType != "sent" {
		t.Fatalf("copy: status %q in %q, want pending in sent", status, folderType)
	}
	var outboxID string
	if err := raw.QueryRow(`SELECT message_id FROM outbox_messages WHERE user_id = $1`, user.ID).Scan(&outboxID); err != nil || outboxID != msgID {
		t.Fatalf("outbox message_id %q (%v), want %q", outboxID, err, msgID)
	}
	if kicked != 1 {
		t.Fatalf("outbox kicked %d times, want 1", kicked)
	}
	select {
	case ev := <-events:
		if ev.Type != notify.EventMessageSent || ev.RFCMessageID != msgID {
			t.Fatalf("event %+v, want message_sent for %s", ev, msgID)
		}
	case <-time.After(time.Second):
		t.Fatal("no message_sent event")
	}

	// The client re-submits the same message (eM Client does): already in
	// Sent → accepted without queueing it again.
	s.Reset()
	_ = s.Mail(me, nil)
	_ = s.Rcpt("someone@dead-"+domain, nil)
	if err := s.Data(strings.NewReader(body)); err != nil {
		t.Fatalf("DATA again: %v", err)
	}
	var queued int
	if err := raw.QueryRow(`SELECT count(*) FROM outbox_messages WHERE user_id = $1`, user.ID).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("outbox rows after a re-submission: %d (%v), want 1", queued, err)
	}
}
