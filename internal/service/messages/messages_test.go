package messages

import (
	"context"
	"errors"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/db"
)

const user = int64(7)

func local(id int64, f db.MessageFlags) fakeMessage {
	return fakeMessage{state: db.MessageSyncState{ID: id, UserID: user, RemoteFolder: "INBOX", Flags: f}}
}

func external(id int64, f db.MessageFlags) fakeMessage {
	return fakeMessage{state: db.MessageSyncState{ID: id, UserID: user, AccountID: 3, RemoteUID: 900 + uint32(id), RemoteFolder: "INBOX", Flags: f}}
}

func TestSetFlags_LocalMessageWritesButDoesNotQueue(t *testing.T) {
	store := newFakeStore(local(1, db.MessageFlags{}))
	res, err := New(store).SetFlags(context.Background(), user, 1, FlagUpdate{Seen: Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed() || !res.VisibleChanged() || res.Queued {
		t.Fatalf("result = %+v", res)
	}
	if !store.messages[1].state.Flags.Seen {
		t.Fatal("seen not persisted")
	}
	if len(store.queue) != 0 {
		t.Fatalf("local message queued for upstream: %+v", store.queue)
	}
}

func TestSetFlags_ExternalMessageQueuesFullState(t *testing.T) {
	store := newFakeStore(external(1, db.MessageFlags{Flagged: true, Answered: true}))
	res, err := New(store).SetFlags(context.Background(), user, 1, FlagUpdate{Seen: Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Queued {
		t.Fatal("external change not queued")
	}
	got := store.queue[1]
	want := fakeQueueEntry{accountID: 3, remoteFolder: "INBOX", remoteUID: 901,
		flags: db.MessageFlags{Seen: true, Flagged: true, Answered: true}}
	if got != want {
		t.Fatalf("queued %+v, want %+v (upstream STORE is replace-all: the full state must be queued)", got, want)
	}
}

func TestSetFlags_NoOpWritesNothing(t *testing.T) {
	store := newFakeStore(external(1, db.MessageFlags{Seen: true}))
	res, err := New(store).SetFlags(context.Background(), user, 1, FlagUpdate{Seen: Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed() || res.Queued {
		t.Fatalf("no-op reported as change: %+v", res)
	}
	if store.writes != 0 || len(store.queue) != 0 {
		t.Fatalf("no-op wrote (%d) or queued (%v)", store.writes, store.queue)
	}
}

func TestSetFlags_DraftIsLocalOnly(t *testing.T) {
	store := newFakeStore(external(1, db.MessageFlags{}))
	res, err := New(store).SetFlags(context.Background(), user, 1, FlagUpdate{Draft: Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed() || res.VisibleChanged() || res.Queued {
		t.Fatalf("result = %+v", res)
	}
	if !store.messages[1].state.Flags.Draft {
		t.Fatal("draft not persisted")
	}
}

func TestSetFlags_DeletedFlagQueuesUpstreamDelete(t *testing.T) {
	store := newFakeStore(external(1, db.MessageFlags{Seen: true}))
	if _, err := New(store).SetFlags(context.Background(), user, 1, FlagUpdate{Deleted: Bool(true)}); err != nil {
		t.Fatal(err)
	}
	if e := store.queue[1]; !e.flags.Deleted || !e.flags.Seen {
		t.Fatalf("queued %+v, want deleted with seen kept", e)
	}
}

func TestSetFlags_NoRemoteUIDNothingToPush(t *testing.T) {
	m := external(1, db.MessageFlags{})
	m.state.RemoteUID = 0
	store := newFakeStore(m)
	res, err := New(store).SetFlags(context.Background(), user, 1, FlagUpdate{Seen: Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Queued || len(store.queue) != 0 {
		t.Fatal("queued a message whose source UID is unknown")
	}
}

func TestSetFlags_OtherUsersMessageIsNotFound(t *testing.T) {
	store := newFakeStore(local(1, db.MessageFlags{}))
	_, err := New(store).SetFlags(context.Background(), user+1, 1, FlagUpdate{Seen: Bool(true)})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if store.messages[1].state.Flags.Seen {
		t.Fatal("foreign message modified")
	}
}

func TestSetFlags_QueueFailureRollsBackFlagWrite(t *testing.T) {
	store := newFakeStore(external(1, db.MessageFlags{}))
	store.failOn = "queue"
	_, err := New(store).SetFlags(context.Background(), user, 1, FlagUpdate{Seen: Bool(true)})
	if !errors.Is(err, errInjected) {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	if store.messages[1].state.Flags.Seen {
		t.Fatal("flag stayed written although its upstream sync was not queued")
	}
}

func TestSetFlags_WriteFailureIsReported(t *testing.T) {
	store := newFakeStore(local(1, db.MessageFlags{}))
	store.failOn = "set"
	if _, err := New(store).SetFlags(context.Background(), user, 1, FlagUpdate{Seen: Bool(true)}); err == nil {
		t.Fatal("write failure swallowed")
	}
}

func TestDelete_LocalMessageGoesToVaultOnly(t *testing.T) {
	store := newFakeStore(local(1, db.MessageFlags{}))
	res, err := New(store).Delete(context.Background(), user, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Queued || len(store.queue) != 0 {
		t.Fatal("local message delete queued upstream")
	}
	if !store.messages[1].softDeleted {
		t.Fatal("message not soft-deleted")
	}
}

func TestDelete_ExternalMessageQueuesUpstreamDelete(t *testing.T) {
	store := newFakeStore(external(1, db.MessageFlags{Seen: true, Flagged: true}))
	res, err := New(store).Delete(context.Background(), user, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Queued {
		t.Fatal("external delete not queued")
	}
	if !store.messages[1].softDeleted {
		t.Fatal("not soft-deleted: deleting must keep the message recoverable in the vault")
	}
	want := fakeQueueEntry{accountID: 3, remoteFolder: "INBOX", remoteUID: 901,
		flags: db.MessageFlags{Seen: true, Flagged: true, Deleted: true}}
	if got := store.queue[1]; got != want {
		t.Fatalf("queued %+v, want %+v", got, want)
	}
}

func TestDelete_QueueFailureKeepsMessage(t *testing.T) {
	store := newFakeStore(external(1, db.MessageFlags{}))
	store.failOn = "queue"
	if _, err := New(store).Delete(context.Background(), user, 1); err == nil {
		t.Fatal("queue failure swallowed")
	}
	if store.messages[1].softDeleted {
		t.Fatal("message deleted locally although the upstream delete was not queued")
	}
}

func TestDelete_NotFound(t *testing.T) {
	store := newFakeStore()
	if _, err := New(store).Delete(context.Background(), user, 42); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPurge_DeletesSelectionAndQueuesExternal(t *testing.T) {
	spamA := external(1, db.MessageFlags{})
	spamA.from = "Spammer <a@spam.example>"
	spamB := local(2, db.MessageFlags{})
	spamB.from = "Other <zz@spam.example>"
	byID := local(3, db.MessageFlags{})
	byID.from = "Me <me@home.example>"
	keep := local(4, db.MessageFlags{})
	keep.from = "Friend <friend@home.example>"
	foreign := local(5, db.MessageFlags{})
	foreign.state.UserID = user + 1
	foreign.from = "Spammer <a@spam.example>"

	store := newFakeStore(spamA, spamB, byID, keep, foreign)
	res, err := New(store).Purge(context.Background(), user, PurgeSelector{
		IDs:           []int64{3},
		SenderDomains: []string{"spam.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 3 || res.Queued != 1 {
		t.Fatalf("result = %+v, want 3 deleted / 1 queued", res)
	}
	for _, id := range []int64{1, 2, 3} {
		if _, ok := store.messages[id]; ok {
			t.Errorf("message %d survived the purge", id)
		}
	}
	if _, ok := store.messages[4]; !ok {
		t.Error("unrelated message purged")
	}
	if _, ok := store.messages[5]; !ok {
		t.Error("another user's message purged")
	}
	if e, ok := store.queue[1]; !ok || !e.flags.Deleted {
		t.Fatalf("upstream delete for the external message missing: %+v", store.queue)
	}
}

func TestPurge_ByAddress(t *testing.T) {
	a := local(1, db.MessageFlags{})
	a.from = "X <x@spam.example>"
	b := local(2, db.MessageFlags{})
	b.from = "Y <y@spam.example>"
	store := newFakeStore(a, b)
	res, err := New(store).Purge(context.Background(), user, PurgeSelector{SenderAddresses: []string{"x@spam.example"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("deleted %d, want 1", res.Deleted)
	}
	if _, ok := store.messages[2]; !ok {
		t.Fatal("address purge removed another sender of the same domain")
	}
}

func TestPurge_EmptySelectorIsNoOp(t *testing.T) {
	store := newFakeStore(local(1, db.MessageFlags{}))
	res, err := New(store).Purge(context.Background(), user, PurgeSelector{})
	if err != nil || res.Deleted != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(store.messages) != 1 {
		t.Fatal("empty selector deleted something")
	}
}

func TestPurge_FailureDeletesNothing(t *testing.T) {
	m := external(1, db.MessageFlags{})
	store := newFakeStore(m)
	store.failOn = "harddelete"
	if _, err := New(store).Purge(context.Background(), user, PurgeSelector{IDs: []int64{1}}); err == nil {
		t.Fatal("failure swallowed")
	}
	if len(store.messages) != 1 || len(store.queue) != 0 {
		t.Fatal("partial purge left behind")
	}
}

func TestFlagUpdateFor(t *testing.T) {
	cases := map[string]func(FlagUpdate) *bool{
		`\Seen`:     func(u FlagUpdate) *bool { return u.Seen },
		`\Flagged`:  func(u FlagUpdate) *bool { return u.Flagged },
		`\Answered`: func(u FlagUpdate) *bool { return u.Answered },
		`\Deleted`:  func(u FlagUpdate) *bool { return u.Deleted },
		`\Draft`:    func(u FlagUpdate) *bool { return u.Draft },
	}
	for name, field := range cases {
		u, ok := FlagUpdateFor(name, true)
		if !ok || field(u) == nil || !*field(u) {
			t.Errorf("%s: update %+v ok=%v", name, u, ok)
		}
	}
	if _, ok := FlagUpdateFor(`\Recent`, true); ok {
		t.Error(`\Recent accepted`)
	}
	if _, ok := FlagUpdateFor("$Label1", true); ok {
		t.Error("keyword accepted")
	}
}

func TestFlagUpdateMergeAndApply(t *testing.T) {
	u := FlagUpdate{Seen: Bool(false), Flagged: Bool(false)}.Merge(FlagUpdate{Seen: Bool(true)})
	got := u.Apply(db.MessageFlags{Flagged: true, Draft: true})
	want := db.MessageFlags{Seen: true, Draft: true}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
