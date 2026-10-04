package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/authlimit"
	"github.com/ddletotam/ddmailserver/internal/clientip"
)

// A throttled request is rejected before the password is checked (no
// database here: reaching it would panic). The client is identified by
// X-Forwarded-For because the peer is the loopback proxy.
func TestAuthThrottledSkipsPasswordCheck(t *testing.T) {
	l, err := authlimit.New(authlimit.Config{MaxFailuresPerIP: 1})
	if err != nil {
		t.Fatal(err)
	}
	l.Failure("198.51.100.20", "", "guess")

	s := New(nil, "/caldav/")
	s.SetAuthLimiter(l, clientip.Default())

	req := httptest.NewRequest("PROPFIND", "/caldav/", nil)
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set("X-Forwarded-For", "198.51.100.20")
	req.SetBasicAuth("alice", "secret")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}
