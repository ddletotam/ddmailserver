package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/emersion/go-imap"
)

// fakeAccount is a remote account of several fakeRemote folders.
type fakeAccount struct {
	order      []string
	attrs      map[string][]string
	folders    map[string]*fakeRemote
	selected   string
	failSelect map[string]bool
	failSearch map[string]bool
	searches   int
}

func newFakeAccount() *fakeAccount {
	return &fakeAccount{attrs: map[string][]string{}, folders: map[string]*fakeRemote{},
		failSelect: map[string]bool{}, failSearch: map[string]bool{}}
}

func (a *fakeAccount) add(name string, validity uint32, attrs []string, msgs ...*fakeMsg) *fakeRemote {
	var next uint32 = 1
	for _, m := range msgs {
		if m.uid >= next {
			next = m.uid + 1
		}
	}
	f := &fakeRemote{status: &imap.MailboxStatus{UidValidity: validity, UidNext: next}, msgs: msgs}
	a.order = append(a.order, name)
	a.attrs[name] = attrs
	a.folders[name] = f
	return f
}

// move takes a message out of one folder and appends it to another under a
// fresh UID, as an IMAP MOVE does.
func (a *fakeAccount) move(from, to, messageID string) {
	src, dst := a.folders[from], a.folders[to]
	for i, m := range src.msgs {
		if m.messageID == messageID {
			src.msgs = append(src.msgs[:i], src.msgs[i+1:]...)
			uid := dst.status.UidNext
			dst.msgs = append(dst.msgs, &fakeMsg{uid: uid, messageID: messageID, flags: m.flags})
			dst.status.UidNext = uid + 1
			return
		}
	}
	panic("move: no " + messageID + " in " + from)
}

func (a *fakeAccount) remove(folder, messageID string) {
	f := a.folders[folder]
	for i, m := range f.msgs {
		if m.messageID == messageID {
			f.msgs = append(f.msgs[:i], f.msgs[i+1:]...)
			return
		}
	}
	panic("remove: no " + messageID + " in " + folder)
}

func (a *fakeAccount) ListFolders() ([]*imap.MailboxInfo, error) {
	var out []*imap.MailboxInfo
	for _, n := range a.order {
		out = append(out, &imap.MailboxInfo{Name: n, Attributes: a.attrs[n]})
	}
	return out, nil
}

func (a *fakeAccount) SelectFolder(name string) (*imap.MailboxStatus, error) {
	f, ok := a.folders[name]
	if !ok || a.failSelect[name] {
		return nil, fmt.Errorf("select %s: NO", name)
	}
	a.selected = name
	return f.SelectFolder(name)
}

func (a *fakeAccount) FetchMessagesByUID(set *imap.SeqSet, items []imap.FetchItem) (chan *imap.Message, chan error) {
	return a.folders[a.selected].FetchMessagesByUID(set, items)
}

func (a *fakeAccount) UIDSearch(c *imap.SearchCriteria) ([]uint32, error) {
	a.searches++
	if a.failSearch[a.selected] {
		return nil, errors.New("SEARCH: BAD")
	}
	want := c.Header.Get("Message-Id")
	var out []uint32
	for _, m := range a.folders[a.selected].msgs {
		if want != "" && strings.Contains(m.messageID, want) {
			out = append(out, m.uid)
		}
	}
	return out, nil
}

// --- pure decision ------------------------------------------------------

func ptr(id int64, folder string, uid uint32) db.RemoteRowPointer {
	return db.RemoteRowPointer{ID: id, MessageID: fmt.Sprintf("m%d", id), AccountID: 5, RemoteFolder: folder, RemoteUID: uid, FolderType: "inbox"}
}

func byUID(name string, lastSeen uint32, present []uint32, before map[uint32]int64) folderPresence {
	p := folderPresence{name: name, mode: presenceByUID, lastSeen: lastSeen, present: map[uint32]bool{}, before: map[uint32]db.RemoteMessageRef{}}
	for _, u := range present {
		p.present[u] = true
	}
	for uid, id := range before {
		p.before[uid] = db.RemoteMessageRef{ID: id, MessageID: fmt.Sprintf("m%d", id)}
	}
	return p
}

func TestFolderPresenceGone(t *testing.T) {
	p := byUID("INBOX", 10, []uint32{1, 3}, map[uint32]int64{1: 101, 2: 102, 3: 103, 11: 111})
	got := p.gone()
	if len(got) != 1 || got[0].id != 102 || got[0].uid != 2 {
		t.Fatalf("gone = %+v (UID 11 is above last_seen and must not be judged)", got)
	}
	none := folderPresence{name: "INBOX", before: p.before}
	if g := none.gone(); len(g) != 0 {
		t.Fatalf("presenceNone proved absence: %+v", g)
	}
	msgid := folderPresence{name: "INBOX", mode: presenceByMessageID, before: p.before}
	if g := msgid.gone(); len(g) != 4 {
		t.Fatalf("by Message-ID every baseline row is a candidate until seen: %+v", g)
	}
}

func TestPlanVanished(t *testing.T) {
	pres := []folderPresence{byUID("INBOX", 10, nil, map[uint32]int64{
		1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 6: 6, 7: 7,
	})}
	cur := map[int64]db.RemoteRowPointer{
		1: ptr(1, "INBOX", 1),   // plain delete
		2: ptr(2, "Archive", 9), // moved upstream, already re-pointed
		3: ptr(3, "INBOX", 3),   // seen elsewhere this cycle
		4: ptr(4, "INBOX", 4),   // pending local change
		5: ptr(5, "INBOX", 5),   // in local trash
		6: ptr(6, "INBOX", 6),   // already in the vault
		// 7: row gone from the DB
	}
	r := cur[4]
	r.Pending = true
	cur[4] = r
	r = cur[5]
	r.FolderType = "trash"
	cur[5] = r
	r = cur[6]
	r.SoftDeleted = true
	cur[6] = r
	plan := planVanished(5, pres, map[string]bool{"m3": true}, cur, map[string]int{"INBOX": 100})
	if len(plan.candidates) != 1 || plan.candidates[0].id != 1 {
		t.Fatalf("candidates = %+v", plan.candidates)
	}
	if plan.moved != 2 || plan.pending != 2 {
		t.Fatalf("moved %d pending %d, want 2/2", plan.moved, plan.pending)
	}
}

func TestPlanVanishedMassGuard(t *testing.T) {
	before := map[uint32]int64{}
	cur := map[int64]db.RemoteRowPointer{}
	for i := 1; i <= 30; i++ {
		before[uint32(i)] = int64(i)
		cur[int64(i)] = ptr(int64(i), "INBOX", uint32(i))
	}
	pres := []folderPresence{byUID("INBOX", 40, nil, before)}

	plan := planVanished(5, pres, nil, cur, map[string]int{"INBOX": 40})
	if len(plan.candidates) != 0 || plan.held["INBOX"] != 30 {
		t.Fatalf("30 of 40 gone must be held: %+v", plan)
	}
	// The same 30 out of 1000: a plausible cleanup, not a glitch.
	plan = planVanished(5, pres, nil, cur, map[string]int{"INBOX": 1000})
	if len(plan.candidates) != 30 || len(plan.held) != 0 {
		t.Fatalf("30 of 1000 must pass: %d held %v", len(plan.candidates), plan.held)
	}
	// A small folder emptied entirely: under the absolute floor — real.
	small := byUID("Work", 5, nil, map[uint32]int64{1: 1, 2: 2})
	cur[1], cur[2] = ptr(1, "Work", 1), ptr(2, "Work", 2)
	plan = planVanished(5, []folderPresence{small}, nil, cur, map[string]int{"Work": 2})
	if len(plan.candidates) != 2 {
		t.Fatalf("small folder emptied: %+v", plan)
	}
	// A spam folder expired wholesale by the provider: no guard.
	spam := byUID("Junk", 40, nil, before)
	spam.junk = true
	for i := 1; i <= 30; i++ {
		cur[int64(i)] = ptr(int64(i), "Junk", uint32(i))
	}
	plan = planVanished(5, []folderPresence{spam}, nil, cur, map[string]int{"Junk": 31})
	if len(plan.candidates) != 30 || len(plan.held) != 0 {
		t.Fatalf("expired spam must pass the guard: %d held %v", len(plan.candidates), plan.held)
	}
}

func TestConfirmVanished(t *testing.T) {
	acc := newFakeAccount()
	acc.add("INBOX", 1, nil, &fakeMsg{uid: 7, messageID: "<keep@x>"})
	acc.add("Work", 1, nil)
	acc.add("[Gmail]/All Mail", 1, []string{`\All`}, &fakeMsg{uid: 50, messageID: "<archived@x>"})
	cands := []goneRef{
		{id: 1, messageID: "<keep@x>", folder: "Work", uid: 3},
		{id: 2, messageID: "<archived@x>", folder: "INBOX", uid: 4},
		{id: 3, messageID: "<gone@x>", folder: "INBOX", uid: 5},
		{id: 4, messageID: "<noid.abc@ddmail.invalid>", folder: "INBOX", uid: 6},
	}
	found, unknown := confirmVanished(context.Background(), acc, cands, []string{"INBOX", "Work", "[Gmail]/All Mail"})
	if found[1] != (foundAt{"INBOX", 7}) || found[2] != (foundAt{"[Gmail]/All Mail", 50}) {
		t.Fatalf("found = %+v", found)
	}
	if _, ok := found[3]; ok || unknown[3] {
		t.Fatalf("really gone message: found=%v unknown=%v", found, unknown)
	}
	if _, ok := found[4]; ok || unknown[4] {
		t.Fatal("synthetic Message-ID must not be searched")
	}

	// A folder that can't be searched leaves everything not found unknown.
	acc.failSearch["Work"] = true
	found, unknown = confirmVanished(context.Background(), acc, cands, []string{"INBOX", "Work"})
	if !unknown[3] || unknown[1] {
		t.Fatalf("search error: found=%v unknown=%v", found, unknown)
	}
	acc.failSearch["Work"] = false
	acc.failSelect["Work"] = true
	_, unknown = confirmVanished(context.Background(), acc, cands, []string{"INBOX", "Work"})
	if !unknown[3] {
		t.Fatal("select error must make the candidate unknown")
	}
}

// --- whole cycle against a real schema ------------------------------------

type vanishEnv struct {
	t        *testing.T
	database *db.DB
	account  *models.Account
	inbox    *models.Folder
	acc      *fakeAccount
	tag      string
	notices  []ExpungeNotice
	last     vanishStats
}

func newVanishEnv(t *testing.T) *vanishEnv {
	database := requireSyncTestDB(t)
	var userID int64
	if err := database.QueryRow(`SELECT id FROM users ORDER BY id LIMIT 1`).Scan(&userID); err != nil {
		t.Skipf("no user in the test DB: %v", err)
	}
	tag := fmt.Sprintf("vanish%d", time.Now().UnixNano())
	var accountID int64
	err := database.QueryRow(`
		INSERT INTO accounts (user_id, name, email, imap_host, imap_port, imap_username, imap_password,
		                      smtp_host, smtp_port, smtp_username, smtp_password)
		VALUES ($1, $2, $3, 'imap.invalid', 993, 'u', 'p', 'smtp.invalid', 465, 'u', 'p')
		RETURNING id`, userID, tag, "box@example.org."+tag).Scan(&accountID)
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	t.Cleanup(func() {
		if _, err := database.Exec(`DELETE FROM flag_sync_queue WHERE account_id = $1`, accountID); err != nil {
			t.Errorf("cleanup queue: %v", err)
		}
		if _, err := database.Exec(`DELETE FROM messages WHERE account_id = $1`, accountID); err != nil {
			t.Errorf("cleanup messages: %v", err)
		}
		if _, err := database.Exec(`DELETE FROM accounts WHERE id = $1`, accountID); err != nil {
			t.Errorf("cleanup account: %v", err)
		}
	})
	inbox, err := database.GetOrCreateLocalInbox(userID)
	if err != nil {
		t.Fatalf("local inbox: %v", err)
	}
	return &vanishEnv{t: t, database: database, inbox: inbox, tag: tag, acc: newFakeAccount(),
		account: &models.Account{ID: accountID, UserID: userID, Email: "box@example.org"}}
}

func (e *vanishEnv) mid(name string) string { return fmt.Sprintf("<%s.%s@example.org>", name, e.tag) }

func (e *vanishEnv) msg(uid uint32, name string) *fakeMsg {
	return &fakeMsg{uid: uid, messageID: e.mid(name)}
}

func (e *vanishEnv) cycle() {
	e.t.Helper()
	task := NewSyncTask(e.account, e.database)
	task.SetExpungeNotifyFunc(func(n ExpungeNotice) { e.notices = append(e.notices, n) })
	if err := task.syncAllRemoteFolders(context.Background(), e.acc, e.inbox); err != nil {
		e.t.Fatalf("sync: %v", err)
	}
	e.last = task.vanished
}

// state reports (soft_deleted, remote_folder, remote_uid) of a message.
func (e *vanishEnv) state(name string) (bool, string, int64) {
	e.t.Helper()
	var del bool
	var folder string
	var uid int64
	err := e.database.QueryRow(`SELECT COALESCE(soft_deleted, false), COALESCE(remote_folder, ''), COALESCE(remote_uid, 0)
		FROM messages WHERE user_id = $1 AND message_id = $2`, e.account.UserID, e.mid(name)).Scan(&del, &folder, &uid)
	if err != nil {
		e.t.Fatalf("state of %s: %v", name, err)
	}
	return del, folder, uid
}

func (e *vanishEnv) wantLive(names ...string) {
	e.t.Helper()
	for _, n := range names {
		if del, _, _ := e.state(n); del {
			e.t.Fatalf("%s deleted locally, must stay", n)
		}
	}
}

func (e *vanishEnv) wantVault(names ...string) {
	e.t.Helper()
	for _, n := range names {
		del, _, uid := e.state(n)
		if !del || uid != 0 {
			e.t.Fatalf("%s: soft_deleted=%v remote_uid=%d, want vault + cleared uid", n, del, uid)
		}
		if q, err := e.queued(n); err != nil || q != 0 {
			e.t.Fatalf("%s: delete-sync queued upstream (%d, %v) — loop", n, q, err)
		}
	}
}

func (e *vanishEnv) queued(name string) (int, error) {
	var n int
	err := e.database.QueryRow(`SELECT COUNT(*) FROM flag_sync_queue q JOIN messages m ON m.id = q.message_id
		WHERE m.user_id = $1 AND m.message_id = $2`, e.account.UserID, e.mid(name)).Scan(&n)
	return n, err
}

func TestUpstreamDeletionCycle(t *testing.T) {
	e := newVanishEnv(t)
	e.acc.add("INBOX", 7, nil, e.msg(1, "a"), e.msg(2, "b"), e.msg(3, "c"), e.msg(4, "d"), e.msg(5, "e"))
	e.acc.add("Archive", 9, nil, e.msg(1, "x"))
	e.acc.add("Trash", 3, []string{`\Trash`})

	e.cycle() // first contact: full pass, nothing judged
	e.cycle() // bookmark in place
	e.wantLive("a", "b", "c", "d", "e", "x")

	// 1. Plain delete upstream.
	e.acc.remove("INBOX", e.mid("a"))
	e.cycle()
	e.wantVault("a")
	e.wantLive("b", "c", "d", "e", "x")
	if e.last.removed != 1 {
		t.Fatalf("summary: %+v, want 1 removed", e.last)
	}
	var kind int
	if err := e.database.QueryRow(`SELECT kind FROM message_changes WHERE user_id = $1 AND message_id = $2
		ORDER BY seq DESC LIMIT 1`, e.account.UserID, e.mid("a")).Scan(&kind); err != nil || kind != 2 {
		t.Fatalf("no delete tombstone in the change journal: kind=%d err=%v", kind, err)
	}
	if len(e.notices) != 1 || e.notices[0].FolderID != e.inbox.ID || len(e.notices[0].SeqNums) != 1 {
		t.Fatalf("expunge notices: %+v", e.notices)
	}

	// 2. Moved to a folder synced EARLIER in the cycle (Archive → INBOX)
	//    and to one synced LATER (INBOX → Archive): both are moves.
	e.acc.move("Archive", "INBOX", e.mid("x"))
	e.acc.move("INBOX", "Archive", e.mid("b"))
	e.cycle()
	e.wantLive("x", "b")
	if e.last.moved != 2 {
		t.Fatalf("moves counted: %+v, want 2", e.last)
	}
	if _, f, _ := e.state("b"); f != "Archive" {
		t.Fatalf("b not re-pointed to Archive: %q", f)
	}
	if _, f, _ := e.state("x"); f != "INBOX" {
		t.Fatalf("x not re-pointed to INBOX: %q", f)
	}

	// 3. Moved to Trash upstream (not synced) = deleted.
	e.acc.move("INBOX", "Trash", e.mid("c"))
	e.cycle()
	e.wantVault("c")

	// 4. Broken answer: the flags FETCH fails — nothing is judged gone.
	e.acc.remove("INBOX", e.mid("d"))
	e.acc.folders["INBOX"].failFetch = errors.New("connection reset")
	task := NewSyncTask(e.account, e.database)
	if err := task.syncAllRemoteFolders(context.Background(), e.acc, e.inbox); err != nil {
		t.Fatal(err)
	}
	e.wantLive("d")
	e.acc.folders["INBOX"].failFetch = nil

	// 5. Pending local change on d: left alone while queued.
	var dID int64
	if err := e.database.QueryRow(`SELECT id FROM messages WHERE user_id = $1 AND message_id = $2`,
		e.account.UserID, e.mid("d")).Scan(&dID); err != nil {
		t.Fatal(err)
	}
	_, _, dUID := e.state("d")
	if err := e.database.QueueFlagSync(dID, e.account.ID, "INBOX", uint32(dUID), true, false, false, false); err != nil {
		t.Fatal(err)
	}
	e.cycle()
	e.wantLive("d")
	if _, err := e.database.Exec(`DELETE FROM flag_sync_queue WHERE message_id = $1`, dID); err != nil {
		t.Fatal(err)
	}
	e.cycle()
	e.wantVault("d")

	// 6. UIDVALIDITY reset with renumbered UIDs: the full pass re-points what
	//    is still there and deletes by Message-ID only what is gone.
	in := e.acc.folders["INBOX"]
	var keep []*fakeMsg
	for _, m := range in.msgs {
		if m.messageID != e.mid("e") {
			keep = append(keep, &fakeMsg{uid: m.uid + 100, messageID: m.messageID})
		}
	}
	in.msgs = keep
	in.status = &imap.MailboxStatus{UidValidity: 8, UidNext: 300}
	e.cycle()
	e.wantVault("e")
	e.wantLive("x")
	if _, _, u := e.state("x"); u < 100 {
		t.Fatalf("x not renumbered: %d", u)
	}

	// 7. Restored from the vault: local-only now, the next sync leaves it.
	if _, err := e.database.Exec(`UPDATE messages SET soft_deleted = false WHERE user_id = $1 AND message_id = $2`,
		e.account.UserID, e.mid("a")); err != nil {
		t.Fatal(err)
	}
	e.cycle()
	e.wantLive("a")
}

func TestUpstreamDeletionMassGuard(t *testing.T) {
	e := newVanishEnv(t)
	var msgs []*fakeMsg
	for i := 1; i <= 30; i++ {
		msgs = append(msgs, e.msg(uint32(i), fmt.Sprintf("m%d", i)))
	}
	e.acc.add("INBOX", 7, nil, msgs...)
	e.cycle()
	e.cycle()
	// 25 of 30 disappear in one go: a glitch, not a cleanup.
	in := e.acc.folders["INBOX"]
	in.msgs = in.msgs[25:]
	e.cycle()
	for i := 1; i <= 25; i++ {
		e.wantLive(fmt.Sprintf("m%d", i))
	}
	if len(e.notices) != 0 {
		t.Fatalf("notices sent for held deletes: %+v", e.notices)
	}
	if e.last.held != 25 || e.last.removed != 0 {
		t.Fatalf("summary: %+v, want 25 held", e.last)
	}

	// A folder gone from LIST entirely: messages stay.
	e.acc.order = nil
	e.acc.add("Other", 1, nil)
	e.cycle()
	e.wantLive("m26", "m30")
}
