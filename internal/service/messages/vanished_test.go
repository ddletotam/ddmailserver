package messages

import (
	"context"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/db"
)

func vanishedRef(id int64) VanishedRef {
	return VanishedRef{MessageID: id, AccountID: 3, RemoteFolder: "INBOX", RemoteUID: 900 + uint32(id)}
}

func TestDeleteVanishedUpstream_SoftDeletesWithoutQueue(t *testing.T) {
	store := newFakeStore(external(1, db.MessageFlags{Seen: true}))
	store.messages[1].state.FolderID, store.messages[1].state.UID = 10, 55
	res, err := New(store).DeleteVanishedUpstream(context.Background(), user, vanishedRef(1))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Deleted || res.FolderID != 10 || res.UID != 55 {
		t.Fatalf("result = %+v", res)
	}
	m := store.messages[1]
	if !m.softDeleted {
		t.Fatal("not moved to the vault")
	}
	if m.state.RemoteUID != 0 {
		t.Fatal("remote UID kept: a vault restore would be deleted again by the next sync")
	}
	if len(store.queue) != 0 {
		t.Fatalf("delete queued upstream for a message already gone there: %+v", store.queue)
	}
}

func TestDeleteVanishedUpstream_Skips(t *testing.T) {
	cases := []struct {
		name  string
		setup func(s *fakeStore)
		ref   VanishedRef
		want  string
	}{
		{"pointer moved", func(s *fakeStore) { s.messages[1].state.RemoteFolder = "Archive" }, vanishedRef(1), VanishedSkipMoved},
		{"uid renumbered", func(s *fakeStore) { s.messages[1].state.RemoteUID = 5 }, vanishedRef(1), VanishedSkipMoved},
		{"other account", func(s *fakeStore) { s.messages[1].state.AccountID = 4 }, vanishedRef(1), VanishedSkipMoved},
		{"already in vault", func(s *fakeStore) { s.messages[1].softDeleted = true }, vanishedRef(1), VanishedSkipVault},
		{"pending local change", func(s *fakeStore) {
			s.queue[1] = fakeQueueEntry{accountID: 3, remoteFolder: "INBOX", remoteUID: 901}
		}, vanishedRef(1), VanishedSkipPending},
		{"deleted flag", func(s *fakeStore) { s.messages[1].state.Flags.Deleted = true }, vanishedRef(1), VanishedSkipDeletedFlag},
		{"local trash", func(s *fakeStore) {
			s.folders[20] = fakeFolder{userID: user, typ: "trash"}
			s.messages[1].state.FolderID = 20
		}, vanishedRef(1), VanishedSkipTrash},
		{"row gone", func(s *fakeStore) { delete(s.messages, 1) }, vanishedRef(1), VanishedSkipGone},
	}
	for _, c := range cases {
		store := newFakeStore(external(1, db.MessageFlags{}))
		c.setup(store)
		res, err := New(store).DeleteVanishedUpstream(context.Background(), user, c.ref)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if res.Deleted || res.Skip != c.want {
			t.Fatalf("%s: result %+v, want skip %q", c.name, res, c.want)
		}
		if m, ok := store.messages[1]; ok && c.want != VanishedSkipVault && m.softDeleted {
			t.Fatalf("%s: message deleted anyway", c.name)
		}
	}
}

func TestDeleteVanishedUpstream_ErrorRollsBack(t *testing.T) {
	store := newFakeStore(external(1, db.MessageFlags{}))
	store.failOn = "clearuid"
	if _, err := New(store).DeleteVanishedUpstream(context.Background(), user, vanishedRef(1)); err == nil {
		t.Fatal("want error")
	}
	if store.messages[1].softDeleted || store.messages[1].state.RemoteUID == 0 {
		t.Fatal("partial delete committed")
	}
}
