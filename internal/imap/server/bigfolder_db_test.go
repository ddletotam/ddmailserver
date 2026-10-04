package server

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/emersion/go-imap"
	_ "github.com/lib/pq"
)

// TestBigFolder_DB runs the IMAP mailbox against a real folder past the old
// 10 000-message cap (MAILSERVER_TEST_DSN; it borrows an existing user and
// removes everything it creates). Before, every command loaded only the
// first 10 000 messages: "*" meant message 10 000, and anything beyond was
// unreachable by FETCH, STORE, COPY, MOVE and SEARCH.
func TestBigFolder_DB(t *testing.T) {
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

	var userID int64
	var username string
	if err := raw.QueryRow(`SELECT id, username FROM users ORDER BY id LIMIT 1`).Scan(&userID, &username); err != nil {
		t.Skipf("no user in the test DB: %v", err)
	}

	tag := fmt.Sprintf("zz-bigfolder-%d", time.Now().UnixNano())
	var folderIDs []int64
	t.Cleanup(func() {
		for _, id := range folderIDs {
			if _, err := raw.Exec(`DELETE FROM messages WHERE folder_id = $1`, id); err != nil {
				t.Logf("cleanup messages of folder %d: %v", id, err)
			}
			if _, err := raw.Exec(`DELETE FROM folders WHERE id = $1`, id); err != nil {
				t.Logf("cleanup folder %d: %v", id, err)
			}
		}
		if _, err := raw.Exec(`DELETE FROM message_changes WHERE message_id LIKE $1`, tag+"%"); err != nil {
			t.Logf("cleanup message_changes: %v", err)
		}
	})

	// New folders get a real UIDVALIDITY, increasing from one to the next.
	before := uint32(time.Now().Unix())
	folder := &models.Folder{UserID: userID, Name: tag, Path: tag, Type: "custom", UIDNext: 1}
	if err := database.CreateFolder(folder); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	folderIDs = append(folderIDs, folder.ID)
	second := &models.Folder{UserID: userID, Name: tag + "-2", Path: tag + "-2", Type: "custom", UIDNext: 1}
	if err := database.CreateFolder(second); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	folderIDs = append(folderIDs, second.ID)
	if folder.UIDValidity < before {
		t.Errorf("UIDVALIDITY %d, want unix time >= %d", folder.UIDValidity, before)
	}
	if second.UIDValidity <= folder.UIDValidity {
		t.Errorf("second folder UIDVALIDITY %d not above the first's %d", second.UIDValidity, folder.UIDValidity)
	}
	stored, err := database.GetFolderByID(folder.ID)
	if err != nil {
		t.Fatalf("GetFolderByID: %v", err)
	}
	if stored.UIDValidity != folder.UIDValidity {
		t.Errorf("stored UIDVALIDITY %d, returned %d", stored.UIDValidity, folder.UIDValidity)
	}

	// 10 050 messages, UID = 2*n, even n seen. The last one has no Message-ID:
	// the per-user unique index on message_id refuses an IMAP COPY of any
	// message that has one (a separate, older problem this test isn't about).
	const n = 10050
	if _, err := raw.Exec(`
		INSERT INTO messages (user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		                      body, body_html, attachments, in_reply_to, message_references,
		                      uid, seen, flagged, answered, draft, deleted, date, size, created_at, updated_at)
		SELECT $1, $2, CASE WHEN g = $4 THEN '' ELSE $3 || '-' || g END, 'big ' || g, 'a@example.org', 'b@example.org', '', '', '',
		       '', '', 0, '', '',
		       2 * g, g % 2 = 0, false, false, false, false, g * 1000, 100, 0, 0
		FROM generate_series(1, $4::INTEGER) AS g
	`, userID, folder.ID, tag, n); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	mb := &Mailbox{
		name:       tag,
		folderType: "custom",
		user:       &User{username: username, userID: userID, database: database},
		database:   database,
		folderID:   folder.ID,
	}

	fetch := func(uid bool, set string, items ...imap.FetchItem) []*imap.Message {
		t.Helper()
		ch := make(chan *imap.Message, n+1)
		if err := mb.ListMessages(uid, parseSet(t, set), items, ch); err != nil {
			t.Fatalf("ListMessages(%v, %s): %v", uid, set, err)
		}
		var out []*imap.Message
		for msg := range ch {
			out = append(out, msg)
		}
		return out
	}

	got := fetch(false, "*", imap.FetchUid, imap.FetchFlags)
	if len(got) != 1 || got[0].SeqNum != n || got[0].Uid != 2*n {
		t.Fatalf("FETCH * = %+v, want seq %d uid %d", got, n, 2*n)
	}

	got = fetch(true, "20001:20004", imap.FetchUid, imap.FetchEnvelope)
	if len(got) != 2 || got[0].SeqNum != 10001 || got[1].SeqNum != 10002 {
		t.Fatalf("UID FETCH 20001:20004 picked %d messages: %+v", len(got), got)
	}
	if got[0].Envelope == nil || got[0].Envelope.Subject != "big 10001" {
		t.Fatalf("envelope of seq 10001 = %+v", got[0].Envelope)
	}

	if got := fetch(true, "1:*", imap.FetchUid, imap.FetchFlags); len(got) != n {
		t.Fatalf("UID FETCH 1:* returned %d of %d", len(got), n)
	}

	if err := mb.UpdateMessagesFlags(false, parseSet(t, fmt.Sprint(n)), imap.AddFlags, []string{imap.FlaggedFlag}); err != nil {
		t.Fatalf("STORE: %v", err)
	}
	var flagged bool
	if err := raw.QueryRow(`SELECT flagged FROM messages WHERE folder_id = $1 AND uid = $2`, folder.ID, 2*n).Scan(&flagged); err != nil {
		t.Fatalf("read flag: %v", err)
	}
	if !flagged {
		t.Fatalf("STORE %d +FLAGS \\Flagged did not reach the last message", n)
	}

	seqs, err := mb.SearchMessages(false, &imap.SearchCriteria{WithFlags: []string{imap.FlaggedFlag}})
	if err != nil {
		t.Fatalf("SEARCH: %v", err)
	}
	if len(seqs) != 1 || seqs[0] != n {
		t.Fatalf("SEARCH FLAGGED = %v, want [%d]", seqs, n)
	}

	// String searches over the whole folder (past the id-list limit, so the
	// query scans the folder) find exactly the one message.
	header := imap.NewSearchCriteria()
	header.Header.Add("Message-ID", fmt.Sprintf("%s-%d", tag, n-1))
	uids, err := mb.SearchMessages(true, header)
	if err != nil {
		t.Fatalf("UID SEARCH HEADER Message-ID: %v", err)
	}
	if len(uids) != 1 || uids[0] != 2*(n-1) {
		t.Fatalf("UID SEARCH HEADER Message-ID = %v, want [%d]", uids, 2*(n-1))
	}
	subject := imap.NewSearchCriteria()
	subject.Header.Add("Subject", "big 10001")
	subject.WithoutFlags = []string{imap.SeenFlag}
	if uids, err = mb.SearchMessages(true, subject); err != nil {
		t.Fatalf("UID SEARCH UNSEEN SUBJECT: %v", err)
	}
	if len(uids) != 1 || uids[0] != 20002 {
		t.Fatalf("UID SEARCH UNSEEN SUBJECT \"big 10001\" = %v, want [20002]", uids)
	}

	if err := mb.CopyMessages(true, parseSet(t, "*"), second.Name); err != nil {
		t.Fatalf("COPY: %v", err)
	}
	var copied int
	if err := raw.QueryRow(`SELECT count(*) FROM messages WHERE folder_id = $1`, second.ID).Scan(&copied); err != nil {
		t.Fatalf("count copies: %v", err)
	}
	if copied != 1 {
		t.Fatalf("UID COPY * copied %d messages, want 1", copied)
	}
}
