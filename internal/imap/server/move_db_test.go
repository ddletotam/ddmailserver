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

// rawSession is a line-level IMAP client: tests see every untagged response
// exactly as sent, in order.
type rawSession struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	n    int
}

// cmd sends a command and returns the lines up to and including its tagged
// response.
func (s *rawSession) cmd(format string, args ...interface{}) []string {
	s.t.Helper()
	s.n++
	tag := fmt.Sprintf("t%d", s.n)
	if err := s.conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		s.t.Fatalf("deadline: %v", err)
	}
	if _, err := fmt.Fprintf(s.conn, "%s %s\r\n", tag, fmt.Sprintf(format, args...)); err != nil {
		s.t.Fatalf("write: %v", err)
	}
	var lines []string
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			s.t.Fatalf("read after %q: %v (so far %q)", format, err, lines)
		}
		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, line)
		if strings.HasPrefix(line, tag+" ") {
			return lines
		}
	}
}

func lastLine(lines []string) string { return lines[len(lines)-1] }

func hasLine(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// TestCopyMove_DB drives COPY/MOVE over the wire against a real database
// (MAILSERVER_TEST_DSN; it creates its own user and removes it). The messages
// carry Message-IDs: COPY and MOVE of such messages used to fail on the
// per-user Message-ID unique index (an INSERT of a second row), get logged and
// answered OK — Thunderbird's delete (MOVE to Trash) silently did nothing.
func TestCopyMove_DB(t *testing.T) {
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

	tag := fmt.Sprintf("zzmove%d", time.Now().UnixNano())
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
			`DELETE FROM flag_sync_queue WHERE account_id IN (SELECT id FROM accounts WHERE user_id = $1)`,
			`DELETE FROM messages WHERE user_id = $1`,
			`DELETE FROM message_changes WHERE user_id = $1`,
			`DELETE FROM folder_subscriptions WHERE user_id = $1`,
			`DELETE FROM folders WHERE user_id = $1`,
			`DELETE FROM accounts WHERE user_id = $1`,
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
	trash, err := database.GetLocalFolderByType(user.ID, "trash")
	if err != nil || trash == nil {
		t.Fatalf("trash: %v", err)
	}

	var accountID int64
	if err := raw.QueryRow(`
		INSERT INTO accounts (user_id, name, email, imap_host, imap_port, imap_username, imap_password,
		                      smtp_host, smtp_port, smtp_username, smtp_password, enabled)
		VALUES ($1, 'ext', 'ext@example.org', 'imap.example.org', 993, 'u', 'p', 'smtp.example.org', 465, 'u', 'p', false)
		RETURNING id`, user.ID).Scan(&accountID); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	// UIDs 1..4: 1 and 2 local with Message-IDs, 3 from the external account
	// (remote UID 555), 4 a local draft without a Message-ID.
	insert := func(uid int, messageID string, account interface{}, remoteUID int) {
		t.Helper()
		if _, err := raw.Exec(`
			INSERT INTO messages (account_id, user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
			                      body, body_html, attachments, in_reply_to, message_references,
			                      uid, seen, flagged, answered, draft, deleted, date, size, remote_uid, remote_folder,
			                      created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'm', 'a@example.org', 'b@example.org', '', '', '', 'body', '', 0, '', '',
			        $5, true, false, false, false, false, 1000, 100, $6, 'INBOX', 0, 0)`,
			account, user.ID, inbox.ID, messageID, uid, remoteUID); err != nil {
			t.Fatalf("insert message uid %d: %v", uid, err)
		}
	}
	insert(1, "<1."+tag+"@example.org>", nil, 0)
	insert(2, "<2."+tag+"@example.org>", nil, 0)
	insert(3, "<3."+tag+"@example.org>", accountID, 555)
	insert(4, "", nil, 0)
	if _, err := raw.Exec(`UPDATE folders SET uid_next = 5 WHERE id = $1`, inbox.ID); err != nil {
		t.Fatalf("uid_next: %v", err)
	}

	// A server as server.go builds it, minus TLS.
	s := imapserver.New(NewBackendWithHub(database, nil))
	s.AllowInsecureAuth = true
	s.Enable(NewUIDPLUSExtension())
	moveExt := NewMoveExtension()
	s.Enable(moveExt)
	moveExt.Arm()
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

	folderOf := func(uidInInbox int) (int64, uint32) {
		t.Helper()
		var folderID int64
		var uid uint32
		if err := raw.QueryRow(`SELECT folder_id, uid FROM messages WHERE user_id = $1 AND message_id = $2`,
			user.ID, fmt.Sprintf("<%d.%s@example.org>", uidInInbox, tag)).Scan(&folderID, &uid); err != nil {
			t.Fatalf("locate message %d: %v", uidInInbox, err)
		}
		return folderID, uid
	}

	// Thunderbird's delete: UID MOVE to Trash. COPYUID comes untagged before
	// the EXPUNGE; the external message's delete is queued for the source.
	lines := sess.cmd("UID MOVE 3 Trash")
	if !strings.Contains(lastLine(lines), "OK") {
		t.Fatalf("UID MOVE: %q", lines)
	}
	folderID, newUID := folderOf(3)
	if folderID != trash.ID {
		t.Fatalf("message 3 is in folder %d, want Trash %d", folderID, trash.ID)
	}
	wantCopyUID := fmt.Sprintf("* OK [COPYUID %d 3 %d]", trash.UIDValidity, newUID)
	if !hasLine(lines, wantCopyUID) {
		t.Fatalf("no %q in %q", wantCopyUID, lines)
	}
	var queuedDelete bool
	if err := raw.QueryRow(`SELECT deleted FROM flag_sync_queue WHERE account_id = $1 AND remote_uid = 555`,
		accountID).Scan(&queuedDelete); err != nil || !queuedDelete {
		t.Fatalf("upstream delete not queued: deleted=%v err=%v", queuedDelete, err)
	}
	if l := sess.cmd("NOOP"); !hasLine(append(lines, l...), "* 3 EXPUNGE") {
		t.Fatalf("no EXPUNGE for the moved message: %q %q", lines, l)
	}

	// UID COPY of a message with a Message-ID moves it (it cannot exist twice).
	if _, err := raw.Exec(`INSERT INTO folders (user_id, name, path, type, uid_next, uid_validity, created_at, updated_at) VALUES ($1, 'Archive', 'Archive', 'archive', 1, 77, 0, 0)`, user.ID); err != nil {
		t.Fatalf("archive folder: %v", err)
	}
	lines = sess.cmd("UID COPY 1 Archive")
	if l := lastLine(lines); !strings.Contains(l, "OK [COPYUID 77 1 1]") {
		t.Fatalf("UID COPY: %q", lines)
	}
	var archiveID int64
	if err := raw.QueryRow(`SELECT id FROM folders WHERE user_id = $1 AND name = 'Archive'`, user.ID).Scan(&archiveID); err != nil {
		t.Fatalf("archive id: %v", err)
	}
	if folderID, _ := folderOf(1); folderID != archiveID {
		t.Fatalf("copied message is in folder %d, want Archive %d", folderID, archiveID)
	}
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM messages WHERE user_id = $1 AND message_id = $2`,
		user.ID, "<1."+tag+"@example.org>").Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows for message 1: %d (%v)", n, err)
	}

	// A draft without a Message-ID is copied for real.
	if l := lastLine(sess.cmd("UID COPY 4 Archive")); !strings.Contains(l, "OK [COPYUID 77 4 2]") {
		t.Fatalf("UID COPY of a draft: %s", l)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM messages WHERE user_id = $1 AND message_id = ''`, user.ID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("draft rows after COPY: %d (%v)", n, err)
	}

	// MOVE by sequence number: what is left in INBOX is UID 2 (seq 1) and the
	// draft UID 4 (seq 2).
	sess.cmd("NOOP")
	lines = sess.cmd("MOVE 1 Archive")
	if !strings.Contains(lastLine(lines), "OK") || !hasLine(lines, "* OK [COPYUID 77 2 3]") {
		t.Fatalf("MOVE 1: %q", lines)
	}
	if folderID, uid := folderOf(2); folderID != archiveID || uid != 3 {
		t.Fatalf("message 2 at folder %d uid %d", folderID, uid)
	}

	// No silent OK any more: a failure answers NO and moves nothing. A folder
	// whose uid_next is NULL cannot hand out a UID.
	if _, err := raw.Exec(`INSERT INTO folders (user_id, name, path, type, uid_next, uid_validity, created_at, updated_at) VALUES ($1, 'Broken', 'Broken', 'custom', NULL, 78, 0, 0)`, user.ID); err != nil {
		t.Fatalf("broken folder: %v", err)
	}
	lines = sess.cmd("UID MOVE 4 Broken")
	if !strings.Contains(lastLine(lines), " NO ") {
		t.Fatalf("failed MOVE answered %q", lines)
	}
	var draftFolder int64
	if err := raw.QueryRow(`SELECT folder_id FROM messages WHERE user_id = $1 AND message_id = '' AND uid = 4`, user.ID).Scan(&draftFolder); err != nil || draftFolder != inbox.ID {
		t.Fatalf("draft after failed MOVE: folder %d (%v)", draftFolder, err)
	}
}
