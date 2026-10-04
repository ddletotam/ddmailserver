package server

import (
	"bufio"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	imapserver "github.com/emersion/go-imap/server"
	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

// TestSearch_DB drives SEARCH over the wire against a real database
// (MAILSERVER_TEST_DSN; it creates its own user and removes it). On
// production `UID SEARCH HEADER Message-ID <x>` answered with nearly the
// whole folder: only \Seen and \Flagged were ever checked, every other
// criterion was ignored.
func TestSearch_DB(t *testing.T) {
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

	folds, err := database.FoldsUnicodeCase()
	if err != nil {
		t.Fatalf("FoldsUnicodeCase: %v", err)
	}
	t.Logf("database folds Unicode case: %v", folds)

	tag := fmt.Sprintf("zzsearch%d", time.Now().UnixNano())
	hash, err := bcrypt.GenerateFromPassword([]byte("pw-"+tag), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	user, err := database.CreateUser(tag, string(hash), "", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM messages WHERE user_id = $1`,
			`DELETE FROM message_changes WHERE user_id = $1`,
			`DELETE FROM folder_subscriptions WHERE user_id = $1`,
			`DELETE FROM folders WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`,
		} {
			if _, err := raw.Exec(q, user.ID); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	})
	if err := database.EnsureDefaultFolders(user.ID); err != nil {
		t.Fatalf("EnsureDefaultFolders: %v", err)
	}
	inbox, err := database.GetLocalFolderByType(user.ID, "inbox")
	if err != nil || inbox == nil {
		t.Fatalf("inbox: %v", err)
	}

	day := func(s string) int64 {
		d, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatalf("date %q: %v", s, err)
		}
		return d.UnixMilli()
	}
	type msg struct {
		uid                         int
		seen, flagged               bool
		subject, from, body, rawSrc string
		date                        int64
		size                        int
	}
	msgs := []msg{
		{uid: 1, seen: true, subject: "Hello world", from: "Alice <alice@example.org>", body: "Quarterly report",
			date: day("2024-03-01 10:00"), size: 1000,
			rawSrc: "From: Alice <alice@example.org>\r\nX-Priority: 1\r\n\r\nQuarterly report\r\n"},
		{uid: 2, flagged: true, subject: "Счёт за МАРТ", from: "Иван Петров <ivan@example.ru>", body: "ОПЛАТИТЕ счёт до пятницы",
			date: day("2024-03-02 23:30"), size: 5000},
		{uid: 3, subject: "Re: Hello", from: "bob@example.org", body: "100% sure, a_b",
			date: day("2024-03-05 08:00"), size: 300},
		{uid: 4, subject: "other", from: "bob@example.org", body: "100 percent, axb",
			date: day("2024-04-01 12:00"), size: 0}, // synced before sizes were stored
	}
	for _, m := range msgs {
		var src interface{}
		if m.rawSrc != "" {
			src = []byte(m.rawSrc)
		}
		if _, err := raw.Exec(`
			INSERT INTO messages (user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
			                      body, body_html, attachments, in_reply_to, message_references,
			                      uid, seen, flagged, answered, draft, deleted, date, size, raw_email, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'me@example.org', '', '', '', $6, '', 0, '', '',
			        $7, $8, $9, false, false, false, $10, $11, $12, 0, 0)`,
			user.ID, inbox.ID, fmt.Sprintf("<%d.%s@example.org>", m.uid, tag), m.subject, m.from, m.body,
			m.uid, m.seen, m.flagged, m.date, m.size, src); err != nil {
			t.Fatalf("insert message uid %d: %v", m.uid, err)
		}
	}
	if _, err := raw.Exec(`UPDATE folders SET uid_next = 5 WHERE id = $1`, inbox.ID); err != nil {
		t.Fatalf("uid_next: %v", err)
	}

	s := imapserver.New(NewBackendWithHub(database, nil))
	s.AllowInsecureAuth = true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	sess := &rawSession{t: t, conn: conn, r: bufio.NewReader(conn)}
	if _, err := sess.r.ReadString('\n'); err != nil { // greeting
		t.Fatalf("greeting: %v", err)
	}
	if l := lastLine(sess.cmd("LOGIN %s pw-%s", tag, tag)); !strings.Contains(l, "OK") {
		t.Fatalf("LOGIN: %s", l)
	}
	if l := lastLine(sess.cmd("SELECT INBOX")); !strings.Contains(l, "OK") {
		t.Fatalf("SELECT: %s", l)
	}

	searchLine := func(lines []string) string {
		t.Helper()
		if !strings.Contains(lastLine(lines), "OK") {
			t.Fatalf("SEARCH failed: %q", lines)
		}
		for _, l := range lines {
			if strings.HasPrefix(l, "* SEARCH") {
				return strings.TrimSpace(strings.TrimPrefix(l, "* SEARCH"))
			}
		}
		t.Fatalf("no * SEARCH in %q", lines)
		return ""
	}

	cases := []struct{ query, want string }{
		{fmt.Sprintf("UID SEARCH HEADER Message-ID <2.%s@example.org>", tag), "2"},
		{fmt.Sprintf(`UID SEARCH HEADER Message-ID "3.%s"`, tag), "3"},
		{"UID SEARCH HEADER Message-ID <nobody@example.org>", ""},
		{"UID SEARCH HEADER X-Priority 1", "1"},
		{"UID SEARCH FROM alice", "1"},
		{`UID SEARCH CHARSET UTF-8 FROM "иван"`, "2"},
		{`UID SEARCH CHARSET UTF-8 SUBJECT "счёт за март"`, "2"},
		{`UID SEARCH CHARSET UTF-8 BODY "оплатите"`, "2"},
		{`UID SEARCH CHARSET UTF-8 TEXT "ПЯТНИЦЫ"`, "2"},
		{`UID SEARCH BODY "100%"`, "3"},
		{`UID SEARCH BODY "a_b"`, "3"},
		{"UID SEARCH SUBJECT hello", "1 3"},
		{"UID SEARCH UNSEEN", "2 3 4"},
		{"UID SEARCH OR SEEN FLAGGED", "1 2"},
		{"UID SEARCH NOT SUBJECT hello", "2 4"},
		{"UID SEARCH SINCE 2-Mar-2024 BEFORE 1-Apr-2024", "2 3"},
		{"UID SEARCH SENTON 5-Mar-2024", "3"},
		{"UID SEARCH LARGER 4000", "2"},
		{"UID SEARCH SMALLER 1000", "3 4"},
		{"UID SEARCH UID 3:*", "3 4"},
		{"SEARCH FLAGGED", "2"},
		{"SEARCH 2:* SUBJECT hello", "3"},
	}
	for _, c := range cases {
		if got := searchLine(sess.cmd("%s", c.query)); got != c.want {
			t.Errorf("%s = %q, want %q", c.query, got, c.want)
		}
	}

	// iOS and Outlook send non-ASCII strings as literals.
	word := "оплатите"
	sess.n++
	ltag := fmt.Sprintf("t%d", sess.n)
	if _, err := fmt.Fprintf(sess.conn, "%s UID SEARCH CHARSET UTF-8 BODY {%d}\r\n", ltag, len(word)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if l, err := sess.r.ReadString('\n'); err != nil || !strings.HasPrefix(l, "+") {
		t.Fatalf("literal continuation: %q %v", l, err)
	}
	if _, err := fmt.Fprintf(sess.conn, "%s\r\n", word); err != nil {
		t.Fatalf("write literal: %v", err)
	}
	var lines []string
	for {
		l, err := sess.r.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		l = strings.TrimRight(l, "\r\n")
		lines = append(lines, l)
		if strings.HasPrefix(l, ltag+" ") {
			break
		}
	}
	if got := searchLine(lines); got != "2" {
		t.Errorf("literal BODY search = %q, want 2", got)
	}

	// LARGER/SMALLER computed the missing size and stored it, as FETCH does.
	var size int64
	if err := raw.QueryRow(`SELECT size FROM messages WHERE folder_id = $1 AND uid = 4`, inbox.ID).Scan(&size); err != nil || size == 0 {
		t.Errorf("size of message 4 after SEARCH LARGER: %d (%v)", size, err)
	}

	// Under LC_CTYPE=C the database cannot fold Cyrillic case: non-ASCII
	// strings are then matched in Go, row by row.
	mb := &Mailbox{
		name:       "INBOX",
		folderType: "inbox",
		user:       &User{username: tag, userID: user.ID, database: database},
		database:   database,
		folderID:   inbox.ID,
	}
	noFold := false
	for _, c := range []struct{ query, want string }{
		{`CHARSET UTF-8 SUBJECT "СЧЁТ"`, "[2]"},
		{`CHARSET UTF-8 FROM "иван"`, "[2]"},
		{`CHARSET UTF-8 TEXT "пятницы"`, "[2]"},
		{`CHARSET UTF-8 NOT BODY "оплатите"`, "[1 3 4]"},
		{`CHARSET UTF-8 HEADER Message-ID "3.` + tag + `"`, "[3]"},
	} {
		got, err := runSearch(&dbSearchSource{m: mb, folds: &noFold}, true, parseSearch(t, c.query))
		if err != nil {
			t.Fatalf("%s: %v", c.query, err)
		}
		if fmt.Sprint(got) != c.want {
			t.Errorf("without DB folding, UID SEARCH %s = %v, want %s", c.query, got, c.want)
		}
	}
}
