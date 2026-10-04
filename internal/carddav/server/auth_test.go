package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/authlimit"
)

// A user blocked for too many failures is rejected before the password is
// checked (no database here: reaching it would panic).
func TestAuthThrottledSkipsPasswordCheck(t *testing.T) {
	l, err := authlimit.New(authlimit.Config{MaxFailuresPerUser: 1})
	if err != nil {
		t.Fatal(err)
	}
	l.Failure("203.0.113.1", "alice", "guess")

	s := New(nil, "/carddav/")
	s.SetAuthLimiter(l, nil)

	req := httptest.NewRequest("PROPFIND", "/carddav/", nil)
	req.RemoteAddr = "198.51.100.30:5000"
	req.SetBasicAuth("Alice@example.com", "secret")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}
