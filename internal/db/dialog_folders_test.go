package db

import (
	"fmt"
	"testing"
	"time"
)

// TestDialogFolders_OptimisticVersion: first save needs base 0, later saves
// the current version; a stale base writes nothing and reports the current
// version.
func TestDialogFolders_OptimisticVersion(t *testing.T) {
	db := requireTestDB(t)
	tag := fmt.Sprintf("zzfold%d", time.Now().UnixNano())
	user, err := db.CreateUser(tag, "x", "", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM dialog_folders WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`,
		} {
			if _, err := db.DB.Exec(q, user.ID); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	})

	if doc, v, err := db.GetDialogFolders(user.ID); err != nil || doc != "" || v != 0 {
		t.Fatalf("empty: doc=%q v=%d err=%v", doc, v, err)
	}

	v, ok, err := db.SaveDialogFolders(user.ID, `{"folders":[]}`, 0)
	if err != nil || !ok || v != 1 {
		t.Fatalf("first save: v=%d ok=%v err=%v", v, ok, err)
	}
	// A second "first save" from another station loses.
	if v, ok, err := db.SaveDialogFolders(user.ID, `{"folders":[1]}`, 0); err != nil || ok || v != 1 {
		t.Fatalf("racing first save: v=%d ok=%v err=%v", v, ok, err)
	}

	v, ok, err = db.SaveDialogFolders(user.ID, `{"folders":[2]}`, 1)
	if err != nil || !ok || v != 2 {
		t.Fatalf("second save: v=%d ok=%v err=%v", v, ok, err)
	}
	if v, ok, err := db.SaveDialogFolders(user.ID, `{"folders":[3]}`, 1); err != nil || ok || v != 2 {
		t.Fatalf("stale save: v=%d ok=%v err=%v", v, ok, err)
	}

	doc, v, err := db.GetDialogFolders(user.ID)
	if err != nil || doc != `{"folders":[2]}` || v != 2 {
		t.Fatalf("final: doc=%q v=%d err=%v", doc, v, err)
	}
}
