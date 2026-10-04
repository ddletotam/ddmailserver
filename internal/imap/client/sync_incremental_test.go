package client

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/emersion/go-imap"
	_ "github.com/lib/pq"
)

// fakeRemote is an in-memory remote mailbox speaking just enough UID FETCH
// semantics: `*` is the highest UID, and n:m is the same set as m:n — so
// `n:*` with n above the highest UID yields the last message, as RFC 3501
// demands and real servers do.
type fakeRemote struct {
	status *imap.MailboxStatus
	msgs   []*fakeMsg
	calls  []fakeFetch
}

type fakeMsg struct {
	uid       uint32
	messageID string
	flags     []string
}

type fakeFetch struct {
	set  string
	body bool
}

func (f *fakeRemote) SelectFolder(name string) (*imap.MailboxStatus, error) {
	return &imap.MailboxStatus{
		Name:        name,
		Messages:    uint32(len(f.msgs)),
		UidValidity: f.status.UidValidity,
		UidNext:     f.status.UidNext,
	}, nil
}

func (f *fakeRemote) FetchMessagesByUID(set *imap.SeqSet, items []imap.FetchItem) (chan *imap.Message, chan error) {
	body := false
	for _, it := range items {
		if strings.HasPrefix(string(it), "BODY") {
			body = true
		}
	}
	f.calls = append(f.calls, fakeFetch{set: set.String(), body: body})
	var maxUID uint32
	for _, m := range f.msgs {
		if m.uid > maxUID {
			maxUID = m.uid
		}
	}
	in := func(uid uint32) bool {
		for _, s := range set.Set {
			lo, hi := s.Start, s.Stop
			if lo == 0 {
				lo = maxUID
			}
			if hi == 0 {
				hi = maxUID
			}
			if lo > hi {
				lo, hi = hi, lo
			}
			if uid >= lo && uid <= hi {
				return true
			}
		}
		return false
	}
	ch := make(chan *imap.Message, len(f.msgs))
	done := make(chan error, 1)
	for _, m := range f.msgs {
		if !in(m.uid) {
			continue
		}
		out := &imap.Message{Uid: m.uid, Flags: append([]string(nil), m.flags...)}
		for _, it := range items {
			if it == imap.FetchEnvelope {
				out.Envelope = &imap.Envelope{
					Date:      time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
					Subject:   "test " + m.messageID,
					From:      []*imap.Address{{MailboxName: "sender", HostName: "example.org"}},
					To:        []*imap.Address{{MailboxName: "box", HostName: "example.org"}},
					MessageId: m.messageID,
				}
			}
		}
		if body {
			raw := fmt.Sprintf("From: sender@example.org\r\nTo: box@example.org\r\nSubject: test\r\nMessage-ID: <%s>\r\n\r\nbody %d\r\n", m.messageID, m.uid)
			out.Body = map[*imap.BodySectionName]imap.Literal{
				{Peek: true}: bytes.NewBufferString(raw),
			}
		}
		ch <- out
	}
	close(ch)
	done <- nil
	return ch, done
}

func (f *fakeRemote) bodyFetches() []string {
	var out []string
	for _, c := range f.calls {
		if c.body {
			out = append(out, c.set)
		}
	}
	return out
}

func TestPlanFolderSync(t *testing.T) {
	cases := []struct {
		name     string
		st       *db.RemoteFolderState
		validity uint32
		want     folderPlan
	}{
		{"no state → full, persist", nil, 7, folderPlan{full: true, reason: "no saved state", persist: true}},
		{"same validity → incremental", &db.RemoteFolderState{UIDValidity: 7, LastSeenUID: 42}, 7, folderPlan{lastSeen: 42, persist: true}},
		{"validity changed → full", &db.RemoteFolderState{UIDValidity: 7, LastSeenUID: 42}, 8, folderPlan{full: true, reason: "UIDVALIDITY changed 7→8", persist: true}},
		{"no validity → full, never persist", &db.RemoteFolderState{UIDValidity: 7, LastSeenUID: 42}, 0, folderPlan{full: true, reason: "server reports no UIDVALIDITY"}},
	}
	for _, c := range cases {
		if got := planFolderSync(c.st, c.validity); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestHasUIDsAbove(t *testing.T) {
	if hasUIDsAbove(10, 11) {
		t.Error("UIDNEXT 11 after last 10: nothing new")
	}
	if !hasUIDsAbove(10, 12) {
		t.Error("UIDNEXT 12 after last 10: UID 11 may exist")
	}
	if !hasUIDsAbove(10, 0) {
		t.Error("unknown UIDNEXT must not skip the scan")
	}
}

func TestNextLastSeen(t *testing.T) {
	cases := []struct {
		prev, max uint32
		failed    []uint32
		want      uint32
	}{
		{0, 0, nil, 0},
		{10, 15, nil, 15},
		{10, 0, nil, 10},           // nothing scanned keeps the bookmark
		{10, 15, []uint32{13}, 12}, // retry the failed one next run
		{10, 15, []uint32{14, 12}, 11},
		{0, 9, []uint32{1}, 0},
		{10, 15, []uint32{5}, 15}, // below prev cannot happen; ignored
	}
	for _, c := range cases {
		if got := nextLastSeen(c.prev, c.max, c.failed); got != c.want {
			t.Errorf("nextLastSeen(%d,%d,%v) = %d, want %d", c.prev, c.max, c.failed, got, c.want)
		}
	}
}

// `UID FETCH 6:*` on a mailbox whose highest UID is 5 answers with UID 5;
// that message must not come back as "new".
func TestFetchUIDRangeFiltersStarQuirk(t *testing.T) {
	f := &fakeRemote{status: &imap.MailboxStatus{}, msgs: []*fakeMsg{{uid: 3}, {uid: 5}}}
	set := new(imap.SeqSet)
	set.AddRange(6, 0)
	got, err := fetchUIDRange(f, set, []imap.FetchItem{imap.FetchUid}, 6, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want no messages above UID 5, got %d (uid %d)", len(got), got[0].Uid)
	}
	set = new(imap.SeqSet)
	set.AddRange(4, 0)
	got, err = fetchUIDRange(f, set, []imap.FetchItem{imap.FetchUid}, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Uid != 5 {
		t.Fatalf("want UID 5 only, got %+v", got)
	}
}

// --- DB integration: the whole folder cycle against a real schema ---------

func requireSyncTestDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("MAILSERVER_TEST_DSN")
	if dsn == "" {
		t.Skip("set MAILSERVER_TEST_DSN to a Postgres DSN to run DB integration tests")
	}
	raw, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := raw.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return &db.DB{DB: raw}
}

func TestSyncOneFolderIncremental(t *testing.T) {
	database := requireSyncTestDB(t)
	var userID int64
	if err := database.QueryRow(`SELECT id FROM users ORDER BY id LIMIT 1`).Scan(&userID); err != nil {
		t.Skipf("no user in the test DB: %v", err)
	}
	tag := fmt.Sprintf("incr%d", time.Now().UnixNano())
	var accountID int64
	err := database.QueryRow(`
		INSERT INTO accounts (user_id, name, email, imap_host, imap_port, imap_username, imap_password,
		                      smtp_host, smtp_port, smtp_username, smtp_password)
		VALUES ($1, $2, $3, 'imap.invalid', 993, 'u', 'p', 'smtp.invalid', 465, 'u', 'p')
		RETURNING id`, userID, tag, "box@example.org"+"."+tag).Scan(&accountID)
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	t.Cleanup(func() {
		if _, err := database.Exec(`DELETE FROM accounts WHERE id = $1`, accountID); err != nil {
			t.Errorf("cleanup account: %v", err)
		}
	})
	account := &models.Account{ID: accountID, UserID: userID, Email: "box@example.org"}
	inbox, err := database.GetOrCreateLocalInbox(userID)
	if err != nil {
		t.Fatalf("local inbox: %v", err)
	}
	mid := func(i int) string { return fmt.Sprintf("%d.%s@example.org", i, tag) }
	remote := &fakeRemote{
		status: &imap.MailboxStatus{UidValidity: 7, UidNext: 4},
		msgs: []*fakeMsg{
			{uid: 1, messageID: mid(1)},
			{uid: 2, messageID: mid(2)},
			{uid: 3, messageID: mid(3)},
		},
	}
	run := func() (folderSyncResult, int) {
		t.Helper()
		task := NewSyncTask(account, database)
		remote.calls = nil
		res, err := task.syncOneFolder(context.Background(), remote, inbox, "INBOX", folderInbox)
		if err != nil {
			t.Fatalf("syncOneFolder: %v", err)
		}
		return res, task.flagsChanged
	}
	state := func() db.RemoteFolderState {
		t.Helper()
		st, err := database.GetRemoteFolderState(accountID, "INBOX")
		if err != nil || st == nil {
			t.Fatalf("state: %v / %v", st, err)
		}
		return *st
	}

	// 1. First contact: full pass, every body once, bookmark written.
	res, _ := run()
	if res.fullReason != "no saved state" || res.newCount != 3 || res.bodies != 3 {
		t.Fatalf("first run: %+v", res)
	}
	if got := state(); got != (db.RemoteFolderState{UIDValidity: 7, LastSeenUID: 3}) {
		t.Fatalf("state after first run: %+v", got)
	}

	// 2. Nothing new, one message read elsewhere: FLAGS-only, no bodies,
	//    no envelope scan (UIDNEXT says nothing is above the bookmark).
	remote.msgs[1].flags = []string{imap.SeenFlag}
	res, changed := run()
	if res.fullReason != "" || res.bodies != 0 || res.newCount != 0 || changed != 1 {
		t.Fatalf("second run: %+v changed=%d", res, changed)
	}
	if len(remote.calls) != 1 || remote.calls[0].set != "1:3" || remote.calls[0].body {
		t.Fatalf("second run fetches: %+v", remote.calls)
	}
	var seen bool
	if err := database.QueryRow(`SELECT seen FROM messages WHERE user_id = $1 AND message_id = $2`,
		userID, mid(2)).Scan(&seen); err != nil || !seen {
		t.Fatalf("remote \\Seen not pulled: seen=%v err=%v", seen, err)
	}

	// 3. Unpushed local change wins: message 1 read locally (queued), the
	//    remote still says unseen — the flag pass must not undo it.
	var msg1 int64
	if err := database.QueryRow(`SELECT id FROM messages WHERE user_id = $1 AND message_id = $2`,
		userID, mid(1)).Scan(&msg1); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE messages SET seen = true WHERE id = $1`, msg1); err != nil {
		t.Fatal(err)
	}
	if err := database.QueueFlagSync(msg1, accountID, "INBOX", 1, true, false, false, false); err != nil {
		t.Fatal(err)
	}
	run()
	if err := database.QueryRow(`SELECT seen FROM messages WHERE id = $1`, msg1).Scan(&seen); err != nil || !seen {
		t.Fatalf("pending local \\Seen overwritten by remote: seen=%v err=%v", seen, err)
	}

	// 4. New mail: body fetched for exactly the new UID.
	remote.msgs = append(remote.msgs, &fakeMsg{uid: 4, messageID: mid(4)})
	remote.status.UidNext = 5
	res, _ = run()
	if res.newCount != 1 || res.bodies != 1 {
		t.Fatalf("fourth run: %+v", res)
	}
	if bf := remote.bodyFetches(); len(bf) != 1 || bf[0] != "4" {
		t.Fatalf("body fetches: %v", bf)
	}
	if got := state(); got.LastSeenUID != 4 {
		t.Fatalf("bookmark not advanced: %+v", got)
	}

	// 5. UIDNEXT not reported: `5:*` answers with UID 4, which must not be
	//    re-processed as new.
	remote.status.UidNext = 0
	res, _ = run()
	if res.bodies != 0 || res.newCount != 0 || res.skipped != 0 {
		t.Fatalf("star quirk run: %+v", res)
	}

	// 6. UIDVALIDITY reset with renumbered UIDs: full pass, but every
	//    message is known by Message-ID — envelopes only, no bodies, and the
	//    rows follow the new UIDs.
	for i, m := range remote.msgs {
		m.uid = uint32(10 + i)
	}
	remote.status = &imap.MailboxStatus{UidValidity: 8, UidNext: 14}
	res, _ = run()
	if res.fullReason != "UIDVALIDITY changed 7→8" || res.bodies != 0 || res.newCount != 0 || res.skipped != 4 {
		t.Fatalf("uidvalidity run: %+v", res)
	}
	if got := state(); got != (db.RemoteFolderState{UIDValidity: 8, LastSeenUID: 13}) {
		t.Fatalf("state after uidvalidity reset: %+v", got)
	}
	var ruid int64
	if err := database.QueryRow(`SELECT remote_uid FROM messages WHERE user_id = $1 AND message_id = $2`,
		userID, mid(4)).Scan(&ruid); err != nil || ruid != 13 {
		t.Fatalf("remote_uid not renumbered: %d err=%v", ruid, err)
	}
}
