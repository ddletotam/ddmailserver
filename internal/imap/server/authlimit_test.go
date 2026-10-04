package server

import (
	"net"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/authlimit"
	"github.com/emersion/go-imap"
)

// A throttled login must fail before the password is checked: the backend
// here has no database, so reaching AuthenticateProtocol would panic.
func TestLoginThrottledSkipsPasswordCheck(t *testing.T) {
	l, err := authlimit.New(authlimit.Config{MaxFailuresPerIP: 1})
	if err != nil {
		t.Fatal(err)
	}
	l.Failure("2001:db8::5", "", "guess")

	b := &Backend{authLimiter: l}
	conn := &imap.ConnInfo{RemoteAddr: &net.TCPAddr{IP: net.ParseIP("2001:db8::9"), Port: 993}}
	if _, err := b.Login(conn, "alice", "secret"); err == nil {
		t.Fatal("throttled login succeeded")
	}
}
