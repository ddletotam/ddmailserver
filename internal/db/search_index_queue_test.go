package db

import (
	"testing"
)

func queuedSeq(t *testing.T, d *DB, id int64) int64 {
	t.Helper()
	var seq int64
	err := d.DB.QueryRow(`SELECT COALESCE((SELECT seq FROM search_index_queue WHERE message_row_id = $1), 0)`, id).Scan(&seq)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	return seq
}

// TestSearchIndexQueue_Trigger covers migration 060: every write that changes
// the search document queues the message, other updates do not, and an ack
// only removes the entry it was given.
func TestSearchIndexQueue_Trigger(t *testing.T) {
	d := requireTestDB(t)
	var userID, folderID, otherFolderID int64
	if err := d.DB.QueryRow(`SELECT user_id, id FROM folders LIMIT 1`).Scan(&userID, &folderID); err != nil {
		t.Skipf("no folder in the test DB: %v", err)
	}
	if err := d.DB.QueryRow(`SELECT id FROM folders WHERE user_id = $1 AND id <> $2 LIMIT 1`, userID, folderID).Scan(&otherFolderID); err != nil {
		t.Skipf("need two folders of one user: %v", err)
	}
	id := insertPurgeTestMessage(t, d, userID, folderID, "Idx <idx@example.org>", 0, 0)
	t.Cleanup(func() { _, _ = d.DB.Exec(`DELETE FROM search_index_queue WHERE message_row_id = $1`, id) })

	s1 := queuedSeq(t, d, id)
	if s1 == 0 {
		t.Fatal("insert did not queue the message")
	}

	if _, err := d.DB.Exec(`UPDATE messages SET remote_uid = 42, updated_at = updated_at + 1 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if got := queuedSeq(t, d, id); got != s1 {
		t.Fatalf("bookkeeping update re-queued: seq %d → %d", s1, got)
	}

	if _, err := d.DB.Exec(`UPDATE messages SET folder_id = $2 WHERE id = $1`, id, otherFolderID); err != nil {
		t.Fatal(err)
	}
	s2 := queuedSeq(t, d, id)
	if s2 <= s1 {
		t.Fatalf("move did not re-queue: seq %d → %d", s1, s2)
	}

	// Stale ack (the worker read s1, the move happened meanwhile) keeps it.
	if err := d.AckSearchIndex([]SearchIndexEntry{{MessageRowID: id, Seq: s1}}); err != nil {
		t.Fatal(err)
	}
	if queuedSeq(t, d, id) != s2 {
		t.Fatal("stale ack removed a newer entry")
	}
	if err := d.AckSearchIndex([]SearchIndexEntry{{MessageRowID: id, Seq: s2}}); err != nil {
		t.Fatal(err)
	}
	if queuedSeq(t, d, id) != 0 {
		t.Fatal("ack did not remove the entry")
	}

	msgs, err := d.GetMessagesForIndexingByIDs([]int64{id})
	if err != nil || len(msgs) != 1 || msgs[0].FolderID != otherFolderID {
		t.Fatalf("GetMessagesForIndexingByIDs = %v, %v", msgs, err)
	}

	if _, err := d.DB.Exec(`DELETE FROM messages WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if queuedSeq(t, d, id) == 0 {
		t.Fatal("delete did not queue the message")
	}
	exists, err := d.MessageRowsExist([]int64{id})
	if err != nil || exists[id] {
		t.Fatalf("MessageRowsExist after delete = %v, %v", exists, err)
	}
}
