package client

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/authfail"
	imapClient "github.com/emersion/go-imap/client"
)

// loginAgainst runs a real go-imap LOGIN against a fake server that answers it
// with reply (the text after the tag) and returns the classified error the way
// Client.Connect does.
func loginAgainst(t *testing.T, reply string) error {
	t.Helper()
	srv, cli := net.Pipe()
	go func() {
		defer srv.Close()
		_ = srv.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := srv.Write([]byte("* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] ready\r\n")); err != nil {
			return
		}
		r := bufio.NewReader(srv)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			tag := f[0]
			switch strings.ToUpper(f[1]) {
			case "CAPABILITY":
				_, _ = srv.Write([]byte("* CAPABILITY IMAP4rev1 AUTH=PLAIN\r\n" + tag + " OK done\r\n"))
			case "LOGIN":
				if reply == "" { // connection dies mid-command
					return
				}
				_, _ = srv.Write([]byte(tag + " " + reply + "\r\n"))
			default:
				_, _ = srv.Write([]byte(tag + " OK\r\n"))
			}
		}
	}()

	c, err := imapClient.New(cli)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer c.Terminate()
	c.Timeout = 5 * time.Second
	err = c.Login("user@yandex.ru", "revoked-app-password")
	if err == nil {
		return nil
	}
	return authfail.MarkLoginRefusal(err)
}

// TestLoginRefusal_RealLibrary: the classification is applied to what go-imap
// actually returns, not to a hand-made error. The Yandex line is the one from
// the production logs.
func TestLoginRefusal_RealLibrary(t *testing.T) {
	cases := []struct {
		reply string
		want  bool
	}{
		{"NO LOGIN invalid credentials or IMAP is disabled", true},
		{"NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)", true},
		{"NO [UNAVAILABLE] Temporary authentication failure, try again later", false},
		{"", false}, // connection lost — the server judged nothing
	}
	for _, c := range cases {
		err := loginAgainst(t, c.reply)
		if err == nil {
			t.Fatalf("%q: login succeeded", c.reply)
		}
		if got := authfail.Is(err); got != c.want {
			t.Errorf("%q: Is(%v) = %v, want %v", c.reply, err, got, c.want)
		}
	}
}
