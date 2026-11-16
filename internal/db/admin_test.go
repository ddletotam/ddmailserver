package db

import (
	"fmt"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// An administrator account has no mail: IMAP, SMTP and DAV refuse it even
// with the right password (migration 061).
func TestAuthenticateProtocolRefusesAdmin(t *testing.T) {
	db := requireTestDB(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("admin-pass-1"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("test-admin-%d", time.Now().UnixNano())
	u, err := db.CreateAdmin(name, string(hash))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteUser(u.ID) })

	if _, err := db.AuthenticateProtocol(name, "admin-pass-1"); err != ErrInvalidCredentials {
		t.Fatalf("admin on IMAP/SMTP: err = %v, want ErrInvalidCredentials", err)
	}

	if err := db.LogAdminAction(AdminAction{AdminID: u.ID, AdminName: name, Action: "test", TargetType: "user", TargetID: 42}); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListAdminActions(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].AdminName != name || got[0].TargetID != 42 {
		t.Fatalf("audit log: %+v", got)
	}
}

// A mailbox with its own account signs in by full address; the local part
// alone does not reach it.
func TestMailboxPrincipalSignIn(t *testing.T) {
	db := requireTestDB(t)
	var mailboxID int64
	var address string
	err := db.QueryRow(`SELECT m.id, m.local_part || '@' || d.domain FROM mailboxes m JOIN domains d ON d.id = m.domain_id
		WHERE m.principal_id IS NULL AND m.enabled AND d.enabled AND d.verified LIMIT 1`).Scan(&mailboxID, &address)
	if err != nil {
		t.Skipf("no convertible mailbox in the test DB: %v", err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("mailbox-pass-1"), bcrypt.MinCost)
	u, err := db.CreateMailboxPrincipal(mailboxID, address, string(hash))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`UPDATE mailboxes SET principal_id = NULL WHERE id = $1`, mailboxID)
		_ = db.DeleteUser(u.ID)
	})
	got, err := db.AuthenticateProtocol(address, "mailbox-pass-1")
	if err != nil || got.ID != u.ID {
		t.Fatalf("address sign-in: %v %+v", err, got)
	}
	if _, err := db.AuthenticateProtocol(address, "wrong"); err != ErrInvalidCredentials {
		t.Fatalf("wrong password: %v", err)
	}
}
