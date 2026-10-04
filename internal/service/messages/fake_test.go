package messages

import (
	"context"
	"errors"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/db"
)

type fakeMessage struct {
	state       db.MessageSyncState
	from        string
	softDeleted bool
}

type fakeQueueEntry struct {
	accountID    int64
	remoteFolder string
	remoteUID    uint32
	flags        db.MessageFlags // Deleted = the queued delete
}

// fakeStore is an in-memory Store with real transaction semantics: every
// InTx works on a copy that is only published when fn returns nil.
type fakeFolder struct {
	userID  int64
	typ     string
	uidNext uint32
}

type fakeStore struct {
	messages map[int64]*fakeMessage
	queue    map[int64]fakeQueueEntry
	folders  map[int64]fakeFolder
	nextID   int64 // id of the next copied row
	writes   int   // flag writes, to prove no-ops write nothing

	// failOn makes the named Tx method fail (simulated DB error).
	failOn string
}

func newFakeStore(msgs ...fakeMessage) *fakeStore {
	s := &fakeStore{messages: map[int64]*fakeMessage{}, queue: map[int64]fakeQueueEntry{},
		folders: map[int64]fakeFolder{}, nextID: 1000}
	for i := range msgs {
		m := msgs[i]
		s.messages[m.state.ID] = &m
	}
	return s
}

var errInjected = errors.New("injected failure")

func (s *fakeStore) InTx(_ context.Context, fn func(Tx) error) error {
	tx := &fakeTx{store: s, messages: map[int64]*fakeMessage{}, queue: map[int64]fakeQueueEntry{},
		folders: map[int64]fakeFolder{}, nextID: s.nextID, writes: s.writes}
	for id, f := range s.folders {
		tx.folders[id] = f
	}
	for id, m := range s.messages {
		c := *m
		tx.messages[id] = &c
	}
	for id, e := range s.queue {
		tx.queue[id] = e
	}
	if err := fn(tx); err != nil {
		return err // rollback: the copies are dropped
	}
	s.messages, s.queue, s.folders, s.nextID, s.writes = tx.messages, tx.queue, tx.folders, tx.nextID, tx.writes
	return nil
}

type fakeTx struct {
	store    *fakeStore
	messages map[int64]*fakeMessage
	queue    map[int64]fakeQueueEntry
	folders  map[int64]fakeFolder
	nextID   int64
	writes   int
}

func (t *fakeTx) fail(op string) error {
	if t.store.failOn == op {
		return errInjected
	}
	return nil
}

func (t *fakeTx) LockMessageForUpdate(userID, messageID int64) (*db.MessageSyncState, error) {
	if err := t.fail("lock"); err != nil {
		return nil, err
	}
	m, ok := t.messages[messageID]
	if !ok || m.state.UserID != userID {
		return nil, db.ErrNotFound
	}
	st := m.state
	return &st, nil
}

func (t *fakeTx) SetMessageFlags(messageID int64, f db.MessageFlags) error {
	if err := t.fail("set"); err != nil {
		return err
	}
	t.messages[messageID].state.Flags = f
	t.writes++
	return nil
}

func (t *fakeTx) QueueFlagSync(messageID, accountID int64, remoteFolder string, remoteUID uint32, seen, flagged, answered, deleted bool) error {
	if err := t.fail("queue"); err != nil {
		return err
	}
	prev, had := t.queue[messageID]
	e := fakeQueueEntry{accountID: accountID, remoteFolder: remoteFolder, remoteUID: remoteUID,
		flags: db.MessageFlags{Seen: seen, Flagged: flagged, Answered: answered, Deleted: deleted}}
	if had && prev.flags.Deleted { // same sticky-delete rule as db.QueueFlagSync
		e.flags.Deleted = true
	}
	t.queue[messageID] = e
	return nil
}

func (t *fakeTx) SoftDeleteMessage(messageID int64) error {
	if err := t.fail("softdelete"); err != nil {
		return err
	}
	t.messages[messageID].softDeleted = true
	return nil
}

func (t *fakeTx) LockPurgeTargets(userID int64, ids []int64, addresses, domains []string) ([]*db.MessageSyncState, error) {
	if err := t.fail("purgetargets"); err != nil {
		return nil, err
	}
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []*db.MessageSyncState
	for id, m := range t.messages {
		if m.state.UserID != userID {
			continue
		}
		from := strings.ToLower(m.from)
		match := want[id]
		for _, a := range addresses {
			match = match || strings.Contains(from, "<"+strings.ToLower(a)+">")
		}
		for _, d := range domains {
			match = match || (strings.Contains(from, "@"+strings.ToLower(d)+">") && strings.Contains(from, "<"))
		}
		if match {
			st := m.state
			out = append(out, &st)
		}
	}
	return out, nil
}

func (t *fakeTx) HardDeleteUserMessages(userID int64, ids []int64) (int64, error) {
	if err := t.fail("harddelete"); err != nil {
		return 0, err
	}
	var n int64
	for _, id := range ids {
		if m, ok := t.messages[id]; ok && m.state.UserID == userID {
			delete(t.messages, id)
			n++
		}
	}
	return n, nil
}

func (t *fakeTx) ClaimFolderUID(userID, folderID int64) (uint32, string, error) {
	if err := t.fail("claim"); err != nil {
		return 0, "", err
	}
	f, ok := t.folders[folderID]
	if !ok || f.userID != userID {
		return 0, "", db.ErrNotFound
	}
	uid := f.uidNext
	f.uidNext++
	t.folders[folderID] = f
	return uid, f.typ, nil
}

func (t *fakeTx) MoveMessageRow(messageID, folderID int64, uid uint32) error {
	if err := t.fail("move"); err != nil {
		return err
	}
	m, ok := t.messages[messageID]
	if !ok {
		return db.ErrNotFound
	}
	m.state.FolderID, m.state.UID, m.state.Flags.Deleted = folderID, uid, false
	return nil
}

func (t *fakeTx) CopyMessageRow(messageID, folderID int64, uid uint32) (int64, error) {
	if err := t.fail("copy"); err != nil {
		return 0, err
	}
	m, ok := t.messages[messageID]
	if !ok {
		return 0, db.ErrNotFound
	}
	c := *m
	c.state.ID, c.state.FolderID, c.state.UID, c.state.Flags.Deleted = t.nextID, folderID, uid, false
	t.messages[t.nextID] = &c
	t.nextID++
	return c.state.ID, nil
}

func (t *fakeTx) UpstreamVanishGuard(messageID int64) (db.UpstreamVanishGuard, error) {
	if err := t.fail("guard"); err != nil {
		return db.UpstreamVanishGuard{}, err
	}
	m, ok := t.messages[messageID]
	if !ok {
		return db.UpstreamVanishGuard{}, db.ErrNotFound
	}
	_, pending := t.queue[messageID]
	return db.UpstreamVanishGuard{
		SoftDeleted: m.softDeleted,
		Pending:     pending,
		FolderType:  t.folders[m.state.FolderID].typ,
	}, nil
}

func (t *fakeTx) ClearRemoteUID(messageID int64) error {
	if err := t.fail("clearuid"); err != nil {
		return err
	}
	t.messages[messageID].state.RemoteUID = 0
	return nil
}
