package web

import (
	"testing"

	"github.com/ddletotam/ddmailserver/internal/models"
)

func TestProviderPasswordIgnoresOwnLoginPassword(t *testing.T) {
	hash, err := HashPassword("ddmail-login")
	if err != nil {
		t.Fatal(err)
	}
	u := &models.User{PasswordHash: hash}
	if got := providerPassword(u, "carddav_password", "ddmail-login"); got != "" {
		t.Errorf("autofilled login password must count as blank, got %q", got)
	}
	if got := providerPassword(u, "carddav_password", "app-password"); got != "app-password" {
		t.Errorf("a real provider password passes through, got %q", got)
	}
	if got := providerPassword(u, "carddav_password", ""); got != "" {
		t.Errorf("blank stays blank, got %q", got)
	}
	if got := providerPassword(nil, "carddav_password", "x"); got != "x" {
		t.Errorf("no user in context: value passes through, got %q", got)
	}
}
