package client

import (
	"bufio"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/authfail"
)

// authAgainst runs net/smtp AUTH PLAIN against a fake server that answers the
// AUTH command with reply.
func authAgainst(t *testing.T, reply string) error {
	t.Helper()
	srv, cli := net.Pipe()
	go func() {
		defer srv.Close()
		_ = srv.SetDeadline(time.Now().Add(5 * time.Second))
		w := func(s string) { _, _ = srv.Write([]byte(s + "\r\n")) }
		w("220 fake ESMTP")
		r := bufio.NewReader(srv)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch cmd := strings.ToUpper(strings.Fields(line)[0]); cmd {
			case "EHLO":
				w("250-fake")
				w("250 AUTH PLAIN")
			case "AUTH":
				w(reply)
			case "QUIT":
				w("221 bye")
				return
			default:
				w("250 ok")
			}
		}
	}()

	c, err := smtp.NewClient(cli, "localhost")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer c.Close()
	return c.Auth(smtp.PlainAuth("", "user@yandex.ru", "revoked", "localhost"))
}

// TestSMTPAuthRejection_RealLibrary: a 535 from the submission server is a
// rejected password and must defer the outbox instead of burning retries; a
// temporary 454 is not.
func TestSMTPAuthRejection_RealLibrary(t *testing.T) {
	if err := authAgainst(t, "535 5.7.8 Error: authentication failed: Invalid user or password!"); !authfail.Is(err) {
		t.Errorf("535: Is(%v) = false", err)
	}
	if err := authAgainst(t, "454 4.7.0 Temporary authentication failure"); err == nil || authfail.Is(err) {
		t.Errorf("454: err=%v, Is=%v", err, authfail.Is(err))
	}
}

// TestXOAuth2ChallengeIsRejection: Gmail answers a dead XOAUTH2 token with a
// 334 challenge carrying a JSON error; that is the token being refused.
func TestXOAuth2ChallengeIsRejection(t *testing.T) {
	a := &xoauth2Auth{username: "u", token: "t"}
	_, err := a.Next([]byte(`eyJzdGF0dXMiOiI0MDAifQ==`), true)
	if !authfail.Is(err) {
		t.Fatalf("Is(%v) = false", err)
	}
}
