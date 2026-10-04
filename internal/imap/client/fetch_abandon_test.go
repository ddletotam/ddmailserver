package client

import (
	"bufio"
	"fmt"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/emersion/go-imap"
	imapClient "github.com/emersion/go-imap/client"
)

// fetchFloodIMAP is a fake server whose UID FETCH answers with `total`
// messages — far more than the client-side channel buffers hold — so a
// consumer that stops reading leaves the stream stuck mid-way.
func fetchFloodIMAP(t *testing.T, total int) net.Addr {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))

		w := bufio.NewWriter(conn)
		say := func(s string) bool {
			if _, err := w.WriteString(s); err != nil {
				return false
			}
			return w.Flush() == nil
		}
		if !say("* OK [CAPABILITY IMAP4rev1] flood ready\r\n") {
			return
		}
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			tag, cmd := fields[0], strings.ToUpper(fields[1])
			switch cmd {
			case "LOGIN":
				say(tag + " OK logged in\r\n")
			case "SELECT":
				say(fmt.Sprintf("* %d EXISTS\r\n* OK [UIDVALIDITY 1] ok\r\n%s OK [READ-WRITE] selected\r\n", total, tag))
			case "UID":
				for i := 1; i <= total; i++ {
					if _, err := fmt.Fprintf(w, "* %d FETCH (UID %d FLAGS ())\r\n", i, i); err != nil {
						return
					}
				}
				if !say(tag + " OK fetch done\r\n") {
					return
				}
			case "LOGOUT":
				say("* BYE\r\n" + tag + " OK bye\r\n")
				return
			default:
				say(tag + " BAD unexpected\r\n")
			}
		}
	}()
	return ln.Addr()
}

func dialFlood(t *testing.T, total int) *Client {
	t.Helper()
	conn, err := imapClient.Dial(fetchFloodIMAP(t, total).String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.Login("u", "p"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := conn.Select("INBOX", false); err != nil {
		t.Fatalf("select: %v", err)
	}
	return &Client{account: &models.Account{Email: "flood@test"}, conn: conn}
}

func fetchGoroutinesAlive() bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Contains(string(buf[:n]), "(*Client).startFetch")
}

// TestDisconnectAfterAbandonedFetch is the sync-cancellation leak: the sync
// loop stops reading on ctx cancel and returns, its deferred Disconnect sent
// LOGOUT — which the server answers only after the rest of the FETCH stream,
// which go-imap's reader could not deliver into the abandoned channel. LOGOUT
// hung forever, the worker never returned, the service could not stop.
func TestDisconnectAfterAbandonedFetch(t *testing.T) {
	c := dialFlood(t, 2000)

	uids := new(imap.SeqSet)
	uids.AddRange(1, 0)
	messages, _ := c.FetchMessagesByUID(uids, []imap.FetchItem{imap.FetchUid, imap.FetchFlags})

	select {
	case m := <-messages:
		if m == nil || m.Uid != 1 {
			t.Fatalf("first message = %+v, want UID 1", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message arrived")
	}
	// Consumer walks away here, like syncOneFolder on ctx.Err().

	disconnected := make(chan error, 1)
	go func() { disconnected <- c.Disconnect() }()
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("Disconnect hung after an abandoned FETCH")
	}

	deadline := time.Now().Add(5 * time.Second)
	for fetchGoroutinesAlive() {
		if time.Now().After(deadline) {
			t.Fatal("fetch goroutine still alive after Disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestFetchFullyConsumedLogsOut: a consumer that reads everything sees the
// same contract as before — every message in order, channel closed, then the
// result — and Disconnect is a normal LOGOUT.
func TestFetchFullyConsumedLogsOut(t *testing.T) {
	const total = 500
	c := dialFlood(t, total)

	uids := new(imap.SeqSet)
	uids.AddRange(1, 0)
	messages, done := c.FetchMessagesByUID(uids, []imap.FetchItem{imap.FetchUid, imap.FetchFlags})

	want := uint32(1)
	for m := range messages {
		if m.Uid != want {
			t.Fatalf("got UID %d, want %d", m.Uid, want)
		}
		want++
	}
	if want != total+1 {
		t.Fatalf("received %d messages, want %d", want-1, total)
	}
	if err := <-done; err != nil {
		t.Fatalf("fetch result: %v", err)
	}
	if err := c.Disconnect(); err != nil {
		t.Fatalf("logout: %v", err)
	}
}
