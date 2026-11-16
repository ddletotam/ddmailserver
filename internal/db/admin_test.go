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
