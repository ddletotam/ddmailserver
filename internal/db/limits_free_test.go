package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
	"github.com/ddletotam/ddmailserver/migrations"
)

func TestRecipientAddrs(t *testing.T) {
	got := recipientAddrs(`"Вася" <Vasya@Example.com>, petya@example.com`, "", "bad <x@y.z")
	want := []string{"vasya@example.com", "petya@example.com", "x@y.z"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("recipientAddrs = %v, want %v", got, want)
	}
}

func TestFreeTierLimits(t *testing.T) {
	now := int64(100 * 24 * 3600 * 1000)
	if m, r := freeTierLimits(now-24*3600*1000, now); m != FreeSendPerDay/2 || r != FreeRecipientsPerDay/2 {
		t.Fatalf("first week: %d %d", m, r)
	}
	if m, r := freeTierLimits(now-8*24*3600*1000, now); m != FreeSendPerDay || r != FreeRecipientsPerDay {
		t.Fatalf("after warm-up: %d %d", m, r)
	}
}

func TestFreeTierSendLimits(t *testing.T) {
	dsn := os.Getenv("MAILSERVER_TEST_DSN")
	if dsn == "" {
		t.Skip("set MAILSERVER_TEST_DSN to a Postgres DSN to run DB integration tests")
	}
	raw, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	d := &DB{DB: raw}
	if err := d.RunMigrations(context.Background(), migrations.FS()); err != nil {
		t.Fatal(err)
	}
	tag := time.Now().UnixNano()
	newUser := func(name string, signedUpAgo time.Duration) int64 {
		var id int64
		created := timeutil.Now() - signedUpAgo.Milliseconds()
		if err := raw.QueryRow(`INSERT INTO users (username, password_hash, recovery_key_hash, is_admin, created_at, updated_at)
			VALUES ($1, 'x', '', false, $2, $2) RETURNING id`, fmt.Sprintf("%s%d", name, tag), created).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	link := func(userID int64) {
		if _, err := raw.Exec(`INSERT INTO telegram_links (tg_user_id, user_id, terms_accepted_at, created_at) VALUES ($1, $2, 0, 0)`,
			tag%1_000_000_000+userID, userID); err != nil {
			t.Fatal(err)
		}
	}
	send := func(userID int64, to string) error {
		return d.CreateOutboxMessage(&models.OutboxMessage{UserID: userID, From: "a@b.c", To: to, Subject: "s", Status: "pending"})
	}

	// A fresh free account: 10 different recipients, 25 messages.
	fresh := newUser("zzfree", time.Hour)
	link(fresh)
	for i := 0; i < 10; i++ {
		if err := send(fresh, fmt.Sprintf("r%d@example.com", i)); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	if err := send(fresh, "r10@example.com"); !errors.Is(err, ErrRecipientLimit) || !errors.Is(err, ErrSendLimit) {
		t.Fatalf("11th recipient: %v", err)
	}
	for i := 10; i < 25; i++ { // the same recipients again are fine
		if err := send(fresh, "r1@example.com"); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	if err := send(fresh, "r1@example.com"); !errors.Is(err, ErrSendLimit) {
		t.Fatalf("26th message: %v", err)
	}

	// After the warm-up week: the full 20 recipients.
	old := newUser("zzfreeold", 10*24*time.Hour)
	link(old)
	for i := 0; i < 20; i++ {
		if err := send(old, fmt.Sprintf("r%d@example.com", i)); err != nil {
			t.Fatalf("old account, message %d: %v", i, err)
		}
	}
	if err := send(old, "r20@example.com"); !errors.Is(err, ErrRecipientLimit) {
		t.Fatalf("old account, 21st recipient: %v", err)
	}

	// Not signed up through the bot: no free-tier limits.
	admin := newUser("zzplain", time.Hour)
	for i := 0; i < 30; i++ {
		if err := send(admin, fmt.Sprintf("r%d@example.com", i)); err != nil {
			t.Fatalf("plain account, message %d: %v", i, err)
		}
	}

	// Mail to addresses that don't exist stops sending and tells the user once.
	bouncy := newUser("zzbouncy", 10*24*time.Hour)
	link(bouncy)
	sentFolder, err := d.GetOrCreateLocalFolder(bouncy, "Sent", "sent")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < FreeBounceStop; i++ {
		if _, err := raw.Exec(`INSERT INTO messages (user_id, folder_id, message_id, from_addr, uid, subject, created_at, delivery_status, delivery_reason)
			VALUES ($1, $2, $3, 'a@b.c', $4, 'x', $5, 'failed', 'mailbox_unavailable')`,
			bouncy, sentFolder.ID, fmt.Sprintf("<b%d-%d@test>", tag, i), i+1, timeutil.Now()); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := send(bouncy, "ok@example.com"); !errors.Is(err, ErrSendStopped) || !errors.Is(err, ErrSendLimit) {
			t.Fatalf("after bounces, try %d: %v", i, err)
		}
	}
	var notices int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM bot_notices WHERE tg_user_id = $1 AND kind = 'send_stopped'`,
		tag%1_000_000_000+bouncy).Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("send_stopped notices = %d, %v (want exactly one)", notices, err)
	}
	pending, err := d.PendingBotNotices(100)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range pending {
		if n.TgUserID == tag%1_000_000_000+bouncy {
			if err := d.MarkBotNoticeSent(n.ID); err != nil {
				t.Fatal(err)
			}
		}
	}

	if full, err := d.FreeTierOverQuota(fresh); err != nil || full {
		t.Fatalf("FreeTierOverQuota(empty) = %v, %v", full, err)
	}

	for _, id := range []int64{fresh, old, admin, bouncy} {
		_, _ = raw.Exec(`DELETE FROM outbox_messages WHERE user_id = $1`, id)
		_, _ = raw.Exec(`DELETE FROM messages WHERE user_id = $1`, id)
		_, _ = raw.Exec(`DELETE FROM telegram_links WHERE user_id = $1`, id)
		_, _ = raw.Exec(`DELETE FROM folders WHERE user_id = $1`, id)
		_, _ = raw.Exec(`DELETE FROM users WHERE id = $1`, id)
	}
}
