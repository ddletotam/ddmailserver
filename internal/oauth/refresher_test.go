package oauth

import (
	"testing"

	"github.com/ddletotam/ddmailserver/internal/authfail"
)

// TestRefreshError: a revoked grant pauses logins like a wrong password; any
// other token-endpoint error (a misconfigured client, a 5xx) does not.
func TestRefreshError(t *testing.T) {
	revoked := refreshError(map[string]interface{}{"error": "invalid_grant", "error_description": "Token has been expired or revoked."})
	if !authfail.Is(revoked) {
		t.Errorf("invalid_grant: Is(%v) = false", revoked)
	}
	other := refreshError(map[string]interface{}{"error": "invalid_client"})
	if authfail.Is(other) {
		t.Errorf("invalid_client: Is(%v) = true", other)
	}
	if authfail.Is(refreshError(nil)) {
		t.Error("empty body counted as rejection")
	}
}
