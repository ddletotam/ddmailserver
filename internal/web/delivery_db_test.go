package web

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/notify"
	"github.com/ddletotam/ddmailserver/internal/outbox"
	smtpclient "github.com/ddletotam/ddmailserver/internal/smtp/client"
	"github.com/ddletotam/ddmailserver/internal/smtp/localdelivery"
	"github.com/ddletotam/ddmailserver/migrations"
	_ "github.com/lib/pq"
)

// Integration tests of the send path against a real database
// (MAILSERVER_TEST_DSN; the schema is migrated up to date first). Nothing
// leaves the machine: MX delivery is replaced, the relay is a fake SMTP
// server on 127.0.0.1.

type deliveryEnv struct {
	t      *testing.T
	db     *db.DB
	srv    *Server
	hub    *notify.Hub
	events chan notify.Event
	user   *models.User
	domain string
	me     string
}

func newDeliveryEnv(t *testing.T) *deliveryEnv {
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
	database.SetEncryptionKey("delivery-test-key")
	if err := database.RunMigrations(context.Background(), migrations.FS()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	tag := fmt.Sprintf("zzdeliv%d", time.Now().UnixNano())
	user, err := database.CreateUser(tag, "x", "", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	user.Language = "ru"
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM attachments WHERE message_id IN (SELECT id FROM messages WHERE user_id = $1)`,
			`DELETE FROM messages WHERE user_id = $1`,
			`DELETE FROM message_changes WHERE user_id = $1`,
			`DELETE FROM outbox_messages WHERE user_id = $1`,
			`DELETE FROM folder_subscriptions WHERE user_id = $1`,
			`DELETE FROM folders WHERE user_id = $1`,
			`DELETE FROM accounts WHERE user_id = $1`,
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
		t.Fatalf("CreateDomain: %v", err)
	}
	if err := database.CreateMailbox(&models.Mailbox{UserID: user.ID, DomainID: d.ID, LocalPart: "me", Enabled: true}); err != nil {
		t.Fatalf("CreateMailbox: %v", err)
	}

	hub := notify.NewHub()
	events := hub.Subscribe(user.ID)
	t.Cleanup(func() { hub.Unsubscribe(user.ID, events) })

	i18n := NewI18nManager()
	srv := &Server{database: database, notifyHub: hub, i18nManager: i18n, i18n: i18n.Get("en")}
	return &deliveryEnv{t: t, db: database, srv: srv, hub: hub, events: events, user: user, domain: domain, me: "me@" + domain}
}

func (e *deliveryEnv) call(handler http.HandlerFunc, method, path string, body interface{}) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			e.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req = req.WithContext(setUserContext(req.Context(), e.user))
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
	}
	return rec
}

// send queues a message through POST /send and returns the outbox row id and
// the Message-ID of the copy.
func (e *deliveryEnv) send(from, to string) (int64, string) {
	e.t.Helper()
	rec := e.call(e.srv.HandleDesktopSend, "POST", "/api/desktop/v1/send", map[string]interface{}{
		"from": from, "to": []string{to}, "subject": "Проверка доставки", "text": "привет",
	})
	var resp struct {
		Status         string `json:"status"`
		MessageID      int64  `json:"message_id"`
		RFCMessageID   string `json:"rfc_message_id"`
		DeliveryStatus string `json:"delivery_status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		e.t.Fatal(err)
	}
	if resp.Status != "queued" || resp.RFCMessageID == "" || resp.DeliveryStatus != outbox.StatusPending {
		e.t.Fatalf("send response %+v", resp)
	}
	return resp.MessageID, resp.RFCMessageID
}

func (e *deliveryEnv) conversations() []DesktopConversation {
	e.t.Helper()
	rec := e.call(e.srv.HandleDesktopConversations, "GET", "/api/desktop/v1/conversations?since=0", nil)
	var resp struct {
		Conversations []DesktopConversation `json:"conversations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		e.t.Fatal(err)
	}
	return resp.Conversations
}

func (e *deliveryEnv) conversationWith(addr string) *DesktopConversation {
	for _, c := range e.conversations() {
		for _, cp := range c.Counterparts {
			if cp.Addr == addr {
				c := c
				return &c
			}
		}
	}
	return nil
}

func (e *deliveryEnv) body(rfcID string) DesktopMessageBody {
	e.t.Helper()
	rec := e.call(e.srv.HandleDesktopConversationMessages, "POST", "/api/desktop/v1/conversations/messages",
		[]DesktopMessageRef{{MessageID: rfcID}})
	var bodies []DesktopMessageBody
	if err := json.Unmarshal(rec.Body.Bytes(), &bodies); err != nil {
		e.t.Fatal(err)
	}
	if len(bodies) != 1 {
		e.t.Fatalf("got %d bodies for %s", len(bodies), rfcID)
	}
	return bodies[0]
}

func (e *deliveryEnv) changesHave(since int64, rfcID string) (bool, int64) {
	e.t.Helper()
	rec := e.call(e.srv.HandleDesktopChanges, "GET", fmt.Sprintf("/api/desktop/v1/changes?since=%d", since), nil)
	var resp DesktopChangesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		e.t.Fatal(err)
	}
	found := false
	for _, c := range resp.Entries {
		if c.MessageID == rfcID {
			found = true
		}
	}
	return found, resp.LatestSeq
}

// drain collects the events published so far.
func (e *deliveryEnv) drain() []notify.Event {
	var out []notify.Event
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case ev := <-e.events:
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
}

func (e *deliveryEnv) outboxRow(id int64) *models.OutboxMessage {
	e.t.Helper()
	// Due now: the backoff after a failed attempt would otherwise make the
	// next claim lose.
	if _, err := e.db.Exec(`UPDATE outbox_messages SET next_attempt_at = 0 WHERE id = $1`, id); err != nil {
		e.t.Fatal(err)
	}
	row, err := e.db.GetOutboxMessageByID(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return row
}

// directTask is the scheduler's direct-send wiring, with MX delivery
// replaced by external.
func (e *deliveryEnv) directTask(row *models.OutboxMessage, external func(addr string) error) *smtpclient.DirectSendTask {
	task := smtpclient.NewDirectSendTask(row, e.db, "mx."+e.domain, nil)
	task.SetDeliveryTracker(outbox.New(e.db, e.hub))
	task.SetLocalDeliverer(localdelivery.New(e.db, e.hub, nil, nil, nil))
	task.SetExternalSender(func(_ string, to []string, _ []byte, _ string) map[string]error {
		out := make(map[string]error, len(to))
		for _, a := range to {
			out[a] = external(a)
		}
		return out
	})
	return task
}

func (e *deliveryEnv) countSent(rfcID string) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(`
		SELECT count(*) FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.user_id = $1 AND f.type = 'sent' AND m.message_id = $2`, e.user.ID, rfcID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "connect: connection timed out" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// The case from 2026-11-13: a message to a dead domain. The conversation must
// exist right after «Отправить», stay «pending» through the retries, end as
// «not delivered» with a reason a person can read, and the sender must get a
// bounce linked to the message.
func TestDelivery_DeadDomain_DB(t *testing.T) {
	e := newDeliveryEnv(t)
	to := "someone@dead-" + e.domain
	_, seq0 := e.changesHave(0, "")

	outboxID, rfcID := e.send(e.me, to)

	// Immediately: conversation, journal, quiet event.
	conv := e.conversationWith(to)
	if conv == nil {
		t.Fatal("no conversation with the new correspondent right after send")
	}
	if conv.LastDeliveryStatus != outbox.StatusPending || len(conv.Messages) != 1 ||
		conv.Messages[0].MessageID != rfcID || conv.Messages[0].DeliveryStatus != outbox.StatusPending {
		t.Fatalf("conversation %+v: want one pending message %s", conv, rfcID)
	}
	if found, _ := e.changesHave(seq0, rfcID); !found {
		t.Fatal("/changes does not report the new copy")
	}
	sentEvent := false
	for _, ev := range e.drain() {
		if ev.Type == notify.EventMessageSent && ev.RFCMessageID == rfcID && ev.MessageID != 0 {
			sentEvent = true
		}
		if ev.Type == notify.EventNewMessage {
			t.Errorf("own message announced as new_message: %+v", ev)
		}
	}
	if !sentEvent {
		t.Fatal("no message_sent event at acceptance")
	}
	if b := e.body(rfcID); b.DeliveryStatus != outbox.StatusPending || !b.IsOutgoing {
		t.Fatalf("body %+v: want outgoing, pending", b)
	}

	// Every attempt times out.
	dead := func(string) error {
		return fmt.Errorf("dial failed: %w", &net.OpError{Op: "dial", Net: "tcp", Err: timeoutError{}})
	}
	for attempt := 1; attempt <= 6; attempt++ {
		_ = e.directTask(e.outboxRow(outboxID), dead).Execute(context.Background())
		if attempt == 1 {
			if b := e.body(rfcID); b.DeliveryStatus != outbox.StatusPending || b.DeliveryReason != outbox.ReasonUnreachable {
				t.Fatalf("after attempt 1: body %+v, want pending/unreachable", b)
			}
		}
	}

	row, err := e.db.GetOutboxMessageByID(outboxID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "failed" || !strings.Contains(row.LastError, "timed out") {
		t.Fatalf("outbox row: status %q, last_error %q", row.Status, row.LastError)
	}

	b := e.body(rfcID)
	if b.DeliveryStatus != outbox.StatusFailed || b.DeliveryReason != outbox.ReasonUnreachable ||
		b.DeliveryError != "Почтовый сервер получателя не отвечает." ||
		len(b.DeliveryFailed) != 1 || b.DeliveryFailed[0] != to {
		t.Fatalf("body after the last attempt: %+v", b)
	}
	if strings.Contains(b.DeliveryError, "dial") {
		t.Fatalf("raw error in delivery_error: %q", b.DeliveryError)
	}
	if conv := e.conversationWith(to); conv == nil || conv.LastDeliveryStatus != outbox.StatusFailed {
		t.Fatalf("conversation row after failure: %+v", conv)
	}
	if e.countSent(rfcID) != 1 {
		t.Fatalf("copies in Sent: %d, want 1", e.countSent(rfcID))
	}

	var statuses []string
	for _, ev := range e.drain() {
		if ev.Type == notify.EventDeliveryStatus && ev.RFCMessageID == rfcID {
			statuses = append(statuses, ev.DeliveryStatus+"/"+ev.DeliveryReason)
		}
	}
	// One event per change: pending/unreachable once (the same reason on
	// later retries is not a change), then failed.
	if strings.Join(statuses, ",") != "pending/unreachable,failed/unreachable" {
		t.Fatalf("delivery_status events: %v", statuses)
	}

	// The bounce: in INBOX, from MAILER-DAEMON, linked to the message.
	var from, subject, inReplyTo, body string
	err = e.db.QueryRow(`
		SELECT m.from_addr, m.subject, m.in_reply_to, m.body
		FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.user_id = $1 AND f.type = 'inbox'`, e.user.ID).Scan(&from, &subject, &inReplyTo, &body)
	if err != nil {
		t.Fatalf("no bounce in INBOX: %v", err)
	}
	if !strings.Contains(from, "MAILER-DAEMON@mx."+e.domain) || !strings.Contains(inReplyTo, rfcID) ||
		!strings.HasPrefix(subject, "Письмо не доставлено") || !strings.Contains(body, "Почтовый сервер получателя не отвечает.") {
		t.Fatalf("bounce: from %q subject %q in-reply-to %q body %q", from, subject, inReplyTo, body)
	}
}

// Delivered: the status turns «delivered», and the copy made at acceptance
// stays the only one.
func TestDelivery_Delivered_NoDuplicateCopy_DB(t *testing.T) {
	e := newDeliveryEnv(t)
	to := "friend@ok-" + e.domain
	outboxID, rfcID := e.send(e.me, to)

	if err := e.directTask(e.outboxRow(outboxID), func(string) error { return nil }).Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b := e.body(rfcID); b.DeliveryStatus != outbox.StatusDelivered || b.DeliveryReason != "" || b.DeliveryError != "" {
		t.Fatalf("body %+v: want delivered", b)
	}
	if n := e.countSent(rfcID); n != 1 {
		t.Fatalf("copies in Sent: %d, want 1", n)
	}
	conv := e.conversationWith(to)
	if conv == nil || len(conv.Messages) != 1 || conv.LastDeliveryStatus != outbox.StatusDelivered {
		t.Fatalf("conversation %+v", conv)
	}
	var inbox int
	if err := e.db.QueryRow(`SELECT count(*) FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.user_id = $1 AND f.type = 'inbox'`, e.user.ID).Scan(&inbox); err != nil {
		t.Fatal(err)
	}
	if inbox != 0 {
		t.Fatalf("%d messages in INBOX after a delivery — no bounce expected", inbox)
	}
}

// fakeRelay is a provider's submission server on 127.0.0.1: AUTH accepted,
// RCPT answered with rcptReply.
func fakeRelay(t *testing.T, rcptReply string) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
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
				inData := false
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if inData {
						if line == ".\r\n" {
							inData = false
							w("250 2.0.0 queued")
						}
						continue
					}
					f := strings.Fields(line)
					if len(f) == 0 {
						continue
					}
					switch strings.ToUpper(f[0]) {
					case "EHLO", "HELO":
						w("250-fake")
						w("250 AUTH PLAIN")
					case "AUTH":
						w("235 2.7.0 ok")
					case "RCPT":
						w(rcptReply)
					case "DATA":
						inData = true
						w("354 go")
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
	return ln.Addr().(*net.TCPAddr).Port
}

func (e *deliveryEnv) relayAccount(port int) *models.Account {
	e.t.Helper()
	acc := &models.Account{
		UserID: e.user.ID, Name: "Provider", Email: "me@provider-" + e.domain,
		IMAPHost: "127.0.0.1", IMAPPort: 993, IMAPUsername: "me", IMAPPassword: "x", IMAPTLS: true,
		// Plain SMTP to 127.0.0.1: net/smtp allows PLAIN to localhost.
		SMTPHost: "127.0.0.1", SMTPPort: port, SMTPUsername: "me", SMTPPassword: "x", SMTPTLS: false,
		Enabled: true, SyncMode: "poll", PollInterval: 300,
	}
	if err := e.db.CreateAccount(acc); err != nil {
		e.t.Fatalf("CreateAccount: %v", err)
	}
	return acc
}

func (e *deliveryEnv) relayTask(row *models.OutboxMessage, acc *models.Account) *smtpclient.SendTask {
	task := smtpclient.NewSendTask(row, acc, e.db)
	task.SetDeliveryTracker(outbox.New(e.db, e.hub))
	task.SetBounce(localdelivery.New(e.db, e.hub, nil, nil, nil), "mx."+e.domain)
	return task
}

// Through an external account: delivered once the provider takes it, and the
// provider's own copy in its Sent, coming back with the sync, is the same
// message as ours (same Message-ID) — no second copy, no second bubble.
func TestDelivery_Relay_ProviderCopyGlues_DB(t *testing.T) {
	e := newDeliveryEnv(t)
	acc := e.relayAccount(fakeRelay(t, "250 2.1.5 ok"))
	to := "friend@elsewhere-" + e.domain
	outboxID, rfcID := e.send(acc.Email, to)

	if conv := e.conversationWith(to); conv == nil {
		t.Fatal("no conversation right after a relay send")
	}
	if err := e.relayTask(e.outboxRow(outboxID), acc).Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b := e.body(rfcID); b.DeliveryStatus != outbox.StatusDelivered {
		t.Fatalf("body %+v: want delivered", b)
	}

	// The provider's Sent copy arrives with the IMAP sync of its Sent folder.
	provSent := &models.Folder{UserID: e.user.ID, AccountID: acc.ID, Name: "Sent", Path: "Sent", Type: "sent", UIDNext: 1}
	if err := e.db.CreateFolder(provSent); err != nil {
		t.Fatal(err)
	}
	err := e.db.CreateMessage(&models.Message{
		AccountID: acc.ID, UserID: e.user.ID, FolderID: provSent.ID, MessageID: rfcID,
		Subject: "Проверка доставки", From: acc.Email, To: to, Date: time.Now().UnixMilli(), UID: 1, RemoteUID: 1, Seen: true,
	})
	if !errors.Is(err, db.ErrDuplicateMessage) {
		t.Fatalf("provider's copy: CreateMessage = %v, want ErrDuplicateMessage", err)
	}
	conv := e.conversationWith(to)
	if conv == nil || len(conv.Messages) != 1 {
		t.Fatalf("conversation %+v: want exactly one message", conv)
	}
}

// The provider refuses the recipient: «not delivered» at once and a bounce
// into the sender's mailbox — the provider never took the message, so it will
// not send one.
func TestDelivery_Relay_Refused_DB(t *testing.T) {
	e := newDeliveryEnv(t)
	acc := e.relayAccount(fakeRelay(t, "550 5.1.1 no such user"))
	to := "ghost@elsewhere-" + e.domain
	outboxID, rfcID := e.send(acc.Email, to)

	_ = e.relayTask(e.outboxRow(outboxID), acc).Execute(context.Background())

	b := e.body(rfcID)
	if b.DeliveryStatus != outbox.StatusFailed || b.DeliveryReason != outbox.ReasonMailboxUnavailable ||
		b.DeliveryError == "" || len(b.DeliveryFailed) != 1 || b.DeliveryFailed[0] != to {
		t.Fatalf("body %+v: want failed / mailbox_unavailable", b)
	}
	var inReplyTo string
	if err := e.db.QueryRow(`SELECT m.in_reply_to FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.user_id = $1 AND f.type = 'inbox'`, e.user.ID).Scan(&inReplyTo); err != nil {
		t.Fatalf("no bounce in INBOX: %v", err)
	}
	if !strings.Contains(inReplyTo, rfcID) {
		t.Fatalf("bounce In-Reply-To %q, want %s", inReplyTo, rfcID)
	}
}

// A row queued by the previous server version (no Message-ID on the row, no
// copy in Sent — like id 1463) gets its copy and its status on the first
// attempt after the upgrade, under one stable Message-ID.
func TestDelivery_LegacyQueuedRow_DB(t *testing.T) {
	e := newDeliveryEnv(t)
	to := "old@legacy-" + e.domain
	legacy := &models.OutboxMessage{UserID: e.user.ID, From: e.me, To: to, Subject: "старое", Body: "в очереди", Status: "pending"}
	if err := e.db.CreateOutboxMessage(legacy); err != nil {
		t.Fatal(err)
	}
	if conv := e.conversationWith(to); conv != nil {
		t.Fatal("conversation before the first attempt — the test setup is wrong")
	}

	refused := func(string) error {
		return fmt.Errorf("dial failed: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")})
	}
	_ = e.directTask(e.outboxRow(legacy.ID), refused).Execute(context.Background())
	row := e.outboxRow(legacy.ID)
	if row.MessageID == "" || len(row.RawEmail) == 0 {
		t.Fatalf("legacy row not prepared: message_id %q, raw %d bytes", row.MessageID, len(row.RawEmail))
	}
	conv := e.conversationWith(to)
	if conv == nil || conv.LastDeliveryStatus != outbox.StatusPending {
		t.Fatalf("conversation after the first attempt: %+v", conv)
	}
	_ = e.directTask(row, refused).Execute(context.Background())
	if again := e.outboxRow(legacy.ID); again.MessageID != row.MessageID {
		t.Fatalf("Message-ID changed between attempts: %q → %q", row.MessageID, again.MessageID)
	}
	if n := e.countSent(row.MessageID); n != 1 {
		t.Fatalf("copies in Sent: %d, want 1", n)
	}
}
