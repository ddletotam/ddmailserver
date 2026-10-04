package server

import (
	"testing"

	"github.com/yourusername/mailserver/internal/authlimit"
)

// A blocked user's AUTH fails before the password is checked (no database
// here: reaching it would panic).
func TestAuthPlainThrottledSkipsPasswordCheck(t *testing.T) {
	l, err := authlimit.New(authlimit.Config{MaxFailuresPerUser: 1})
	if err != nil {
		t.Fatal(err)
	}
	l.Failure("203.0.113.1", "alice", "guess")

	s := &Session{authLimiter: l}
	if err := s.AuthPlain("alice", "secret"); err == nil {
		t.Fatal("throttled AUTH succeeded")
	}
	if s.userID != 0 || s.username != "" {
		t.Fatal("throttled AUTH set session identity")
	}
}
