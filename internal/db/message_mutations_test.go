package db

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/models"
)

func TestEscapeLikePattern(t *testing.T) {
	cases := map[string]string{
		"spam.example": "spam.example",
		"a_b.com":      `a\_b.com`,
		"%":            `\%`,
		`back\slash`:   `back\\slash`,
		`50%_\`:        `50\%\_\\`,
	}
	for in, want := range cases {
		if got := EscapeLikePattern(in); got != want {
			t.Errorf("EscapeLikePattern(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSenderPatterns(t *testing.T) {
	got := senderPatterns([]string{"x@a_b.com", " "}, []string{"%", "a_b.com"})
	want := []string{`%<x@a\_b.com>%`, `%@\%>%`, `%@a\_b.com>%`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// insertPurgeTestMessage creates a throwaway message for an existing user and
// folder and removes it on cleanup.
func insertPurgeTestMessage(t *testing.T, d *DB, userID, folderID int64, from string, accountID int64, remoteUID uint32) int64 {
	t.Helper()
	msg := &models.Message{
		UserID: userID, FolderID: folderID, AccountID: accountID, RemoteUID: remoteUID,
		MessageID: fmt.Sprintf("<purge-test-%d-%s@example.org>", time.Now().UnixNano(), from),
		From:      from, Subject: "purge test", UID: uint32(time.Now().UnixNano() % 1e9),
	}
	if err := d.CreateMessage(msg); err != nil {
		t.Fatalf("create message: %v", err)
	}
	t.Cleanup(func() {
		if _, err := d.DB.Exec(`DELETE FROM messages WHERE id = $1`, msg.ID); err != nil {
			t.Logf("cleanup message %d: %v", msg.ID, err)
		}
		if _, err := d.DB.Exec(`DELETE FROM flag_sync_queue WHERE message_id = $1`, msg.ID); err != nil {
			t.Logf("cleanup queue %d: %v", msg.ID, err)
		}
	})
	return msg.ID
}

// TestLockPurgeTargets_MatchesLiterally is the regression for the purge that
// fed a client-supplied domain straight into ILIKE: "_" matched any character,
// so blocking "a_b.example" also wiped "axb.example".
func TestLockPurgeTargets_MatchesLiterally(t *testing.T) {
	d := requireTestDB(t)
	var userID, folderID int64
	if err := d.DB.QueryRow(`SELECT user_id, id FROM folders LIMIT 1`).Scan(&userID, &folderID); err != nil {
		t.Skipf("no folder in the test DB: %v", err)
	}
	hit := insertPurgeTestMessage(t, d, userID, folderID, "Spam <s@a_b.example>", 0, 0)
	miss := insertPurgeTestMessage(t, d, userID, folderID, "Ok <o@axb.example>", 0, 0)

	err := d.InTx(context.Background(), func(tx *Tx) error {
		targets, err := tx.LockPurgeTargets(userID, nil, nil, []string{"a_b.example"})
		if err != nil {
			return err
		}
		found := map[int64]bool{}
		for _, st := range targets {
			found[st.ID] = true
		}
		if !found[hit] {
			t.Errorf("message from the blocked domain not selected")
		}
		if found[miss] {
			t.Errorf("'_' in the domain matched another domain")
		}
		return fmt.Errorf("rollback") // read-only probe
	})
	if err == nil || err.Error() != "rollback" {
		t.Fatalf("InTx: %v", err)
	}
}
