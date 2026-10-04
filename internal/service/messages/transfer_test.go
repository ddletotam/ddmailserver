package messages

import (
	"context"
	"errors"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/db"
)

const (
	inboxFolder = int64(10)
	trashFolder = int64(11)
	otherFolder = int64(12)
)

// inFolder places m in the inbox under uid, with or without a Message-ID.
func inFolder(m fakeMessage, uid uint32, messageID string) fakeMessage {
	m.state.FolderID, m.state.UID, m.state.MessageID = inboxFolder, uid, messageID
	return m
}

func transferStore(msgs ...fakeMessage) *fakeStore {
	s := newFakeStore(msgs...)
	s.folders[inboxFolder] = fakeFolder{userID: user, typ: "inbox", uidNext: 100}
	s.folders[trashFolder] = fakeFolder{userID: user, typ: "trash", uidNext: 50}
	s.folders[otherFolder] = fakeFolder{userID: user, typ: "custom", uidNext: 7}
	return s
}

func TestTransfer_MoveKeepsTheRowAndAssignsDestUIDs(t *testing.T) {
	store := transferStore(inFolder(local(1, db.MessageFlags{Seen: true}), 3, "<a@x>"), inFolder(local(2, db.MessageFlags{}), 4, "<b@x>"))
	res, err := New(store).Transfer(context.Background(), user, []int64{1, 2}, otherFolder, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []Transferred{{MessageID: 1, SrcUID: 3, DestUID: 7, Removed: true}, {MessageID: 2, SrcUID: 4, DestUID: 8, Removed: true}}
	if len(res) != 2 || res[0] != want[0] || res[1] != want[1] {
		t.Fatalf("result = %+v, want %+v", res, want)
	}
	if st := store.messages[1].state; st.FolderID != otherFolder || st.UID != 7 || !st.Flags.Seen {
		t.Fatalf("moved row = %+v", st)
	}
	if len(store.messages) != 2 {
		t.Fatalf("a move created rows: %d", len(store.messages))
	}
	if len(store.queue) != 0 {
		t.Fatal("a move outside Trash was queued upstream")
	}
}

func TestTransfer_CopyOfMessageWithMessageIDIsAMove(t *testing.T) {
	store := transferStore(inFolder(local(1, db.MessageFlags{}), 3, "<a@x>"))
	res, err := New(store).Transfer(context.Background(), user, []int64{1}, otherFolder, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || !res[0].Removed || res[0].DestUID != 7 {
		t.Fatalf("result = %+v", res)
	}
	if len(store.messages) != 1 || store.messages[1].state.FolderID != otherFolder {
		t.Fatal("message with a Message-ID was duplicated instead of moved")
	}
}

func TestTransfer_CopyWithoutMessageIDCopies(t *testing.T) {
	store := transferStore(inFolder(local(1, db.MessageFlags{Flagged: true}), 3, ""))
	res, err := New(store).Transfer(context.Background(), user, []int64{1}, otherFolder, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Removed || res[0].DestUID != 7 {
		t.Fatalf("result = %+v", res)
	}
	if store.messages[1].state.FolderID != inboxFolder {
		t.Fatal("source of a real copy moved")
	}
	c := store.messages[1000]
	if c == nil || c.state.FolderID != otherFolder || c.state.UID != 7 || !c.state.Flags.Flagged {
		t.Fatalf("copy = %+v", c)
	}
}

func TestTransfer_MoveToTrashQueuesUpstreamDelete(t *testing.T) {
	store := transferStore(
		inFolder(external(1, db.MessageFlags{Seen: true}), 3, "<a@x>"),
		inFolder(local(2, db.MessageFlags{}), 4, "<b@x>"),
	)
	res, err := New(store).Transfer(context.Background(), user, []int64{1, 2}, trashFolder, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Queued || res[1].Queued {
		t.Fatalf("result = %+v: only the external message has an upstream to delete from", res)
	}
	want := fakeQueueEntry{accountID: 3, remoteFolder: "INBOX", remoteUID: 901,
		flags: db.MessageFlags{Seen: true, Deleted: true}}
	if got := store.queue[1]; got != want {
		t.Fatalf("queued %+v, want %+v", got, want)
	}
	if store.messages[1].state.FolderID != trashFolder || store.messages[1].softDeleted {
		t.Fatal("the local row must stay, live, in Trash")
	}
}

func TestTransfer_MoveIntoSameFolderIsANoOp(t *testing.T) {
	store := transferStore(inFolder(external(1, db.MessageFlags{}), 3, "<a@x>"))
	res, err := New(store).Transfer(context.Background(), user, []int64{1}, inboxFolder, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Removed || res[0].DestUID != 3 {
		t.Fatalf("result = %+v", res)
	}
	if store.folders[inboxFolder].uidNext != 100 {
		t.Fatal("a no-op move claimed a UID")
	}
}

func TestTransfer_FailureChangesNothing(t *testing.T) {
	store := transferStore(inFolder(external(1, db.MessageFlags{}), 3, "<a@x>"), inFolder(external(2, db.MessageFlags{}), 4, "<b@x>"))
	store.failOn = "queue"
	if _, err := New(store).Transfer(context.Background(), user, []int64{1, 2}, trashFolder, true); err == nil {
		t.Fatal("queue failure swallowed")
	}
	if store.messages[1].state.FolderID != inboxFolder || store.folders[trashFolder].uidNext != 50 {
		t.Fatal("a failed MOVE left a partial move behind")
	}
}

func TestTransfer_ForeignFolderOrMessage(t *testing.T) {
	store := transferStore(inFolder(local(1, db.MessageFlags{}), 3, "<a@x>"))
	store.folders[99] = fakeFolder{userID: user + 1, typ: "custom", uidNext: 1}
	if _, err := New(store).Transfer(context.Background(), user, []int64{1}, 99, true); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("move into another user's folder: err = %v", err)
	}
	if _, err := New(store).Transfer(context.Background(), user+1, []int64{1}, otherFolder, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("move of another user's message: err = %v", err)
	}
}
