package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/outbox"
	"github.com/ddletotam/ddmailserver/internal/smtp/localdelivery"
)

// The case from 2026-11-13: a domain whose A record points at a host that
// never answers on port 25. Every attempt times out; after the last one the
// sender gets «not delivered» with a reason in words and a DSN linked to the
// original — and the raw dial error stays out of both.
func TestDirectSendDeadDomainBouncesWithReadableReason(t *testing.T) {
	dialErr := fmt.Errorf("failed to deliver to dead.test: %w",
		fmt.Errorf("dial failed: %w", &net.OpError{Op: "dial", Net: "tcp",
			Err: &timeoutErr{msg: "connect: connection timed out"}}))
	h := newHarness("someone@dead.test", func(string) error { return dialErr })

	for attempt := 1; attempt <= maxRetries; attempt++ {
		_ = h.task.Execute(context.Background())
	}
	if h.outbox.status != "failed" {
		t.Fatalf("outbox %+v: want failed after %d attempts", h.outbox, maxRetries)
	}
	if !strings.Contains(h.outbox.lastError, "connection timed out") {
		t.Fatalf("last_error %q: the raw error belongs here, for the operator", h.outbox.lastError)
	}

	d := h.tracker.last()
	if d.Status != outbox.StatusFailed || d.Reason != outbox.ReasonUnreachable || strings.Join(d.Failed, ",") != "someone@dead.test" {
		t.Fatalf("delivery %+v: want failed / unreachable / someone@dead.test", d)
	}

	if len(h.local.dsns) != 1 {
		t.Fatalf("got %d DSNs, want 1", len(h.local.dsns))
	}
	raw := h.local.dsnRaw[0]
	for _, want := range []string{
		"From: Mail Delivery System <MAILER-DAEMON@mx.ours.test>",
		"In-Reply-To: <out1@ours.test>",
		"References: <out1@ours.test>",
		"Status: 4.4.7",
		"Diagnostic-Code: X-Local; The recipient's mail server does not respond.",
		"Почтовый сервер получателя не отвечает.",
		"The recipient's mail server does not respond.",
		"Final-Recipient: rfc822; someone@dead.test",
		"Message-ID: <out1@ours.test>", // the original's headers
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("DSN lacks %q:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "dial") || strings.Contains(raw, "timed out") {
		t.Errorf("DSN carries the raw socket error:\n%s", raw)
	}
	if dsn := h.local.dsns[0]; dsn.InReplyTo == "" {
		t.Errorf("parsed DSN has no In-Reply-To")
	}
}

// A row accepted by a server version that put the copy into Sent only after
// delivery (id 1463 on 2026-11-13) gets its copy on the first attempt after
// the upgrade — and only then, not on every retry.
func TestDirectSendLegacyRowGetsSentCopyOnce(t *testing.T) {
	h := newHarness("ext@remote.test", func(string) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	})
	h.tracker.fresh = true
	_ = h.task.Execute(context.Background())
	_ = h.task.Execute(context.Background())
	if h.tracker.copies != 1 {
		t.Fatalf("copies in Sent: %d, want 1", h.tracker.copies)
	}
}

func TestReasonFor(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"null MX", fmt.Errorf("x: %w", errNullMX), outbox.ReasonNoMailDomain},
		{"nxdomain", &net.DNSError{Err: "no such host", Name: "x.test", IsNotFound: true}, outbox.ReasonDomainNotFound},
		{"dns servfail", &net.DNSError{Err: "server misbehaving", IsTemporary: true}, outbox.ReasonUnreachable},
		{"dial timeout", &net.OpError{Op: "dial", Err: &timeoutErr{msg: "i/o timeout"}}, outbox.ReasonUnreachable},
		{"refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, outbox.ReasonUnreachable},
		{"rcpt 550", fmt.Errorf("RCPT TO s***@x.test failed: %w", &textproto.Error{Code: 550, Msg: "5.1.1 user unknown"}), outbox.ReasonMailboxUnavailable},
		{"rcpt 550 no enhanced", fmt.Errorf("RCPT TO s***@x.test failed: %w", &textproto.Error{Code: 550, Msg: "no mailbox"}), outbox.ReasonMailboxUnavailable},
		{"relay rcpt 550", fmt.Errorf("failed to set recipient s***@x.test: %w", &textproto.Error{Code: 550, Msg: "no"}), outbox.ReasonMailboxUnavailable},
		{"quota", fmt.Errorf("RCPT TO s failed: %w", &textproto.Error{Code: 552, Msg: "5.2.2 over quota"}), outbox.ReasonMailboxFull},
		{"data 554", fmt.Errorf("data close failed: %w", &textproto.Error{Code: 554, Msg: "5.7.1 spam"}), outbox.ReasonRejected},
		{"4xx", &textproto.Error{Code: 451, Msg: "4.7.1 greylisted"}, outbox.ReasonTemporary},
		{"local reject", &localdelivery.RejectError{Code: 550, Enhanced: [3]int{5, 1, 1}, Message: "No such user"}, outbox.ReasonMailboxUnavailable},
		{"local disabled domain", &localdelivery.RejectError{Code: 550, Enhanced: [3]int{5, 7, 1}, Message: "disabled"}, outbox.ReasonRejected},
		{"other", errors.New("something odd"), outbox.ReasonOther},
	}
	for _, c := range cases {
		if got := reasonFor(c.err); got != c.want {
			t.Errorf("%s: reasonFor = %q, want %q", c.name, got, c.want)
		}
	}
}

// A host that accepts the connection and never says anything used to hold
// the worker indefinitely: net/smtp has no timeouts of its own.
func TestDirectAttemptSilentServerTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open, say nothing.
			t.Cleanup(func() { _ = c.Close() })
		}
	}()

	start := time.Now()
	err = sendDirectAttempt(timeouts{dial: time.Second, session: 300 * time.Millisecond},
		ln.Addr().String(), "mx.ours.test", "a@ours.test", []string{"b@remote.test"}, []byte("x"), true)
	if err == nil {
		t.Fatal("silent server: want an error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("took %v: the session deadline did not fire", elapsed)
	}
	if IsPermanent(err) || reasonFor(err) != outbox.ReasonUnreachable {
		t.Fatalf("err %v: want a transient «unreachable»", err)
	}
}

// The relay client is bounded the same way.
func TestRelaySilentServerTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	c := newRelayClient(t, port)
	c.timeouts = timeouts{dial: time.Second, session: 300 * time.Millisecond}
	start := time.Now()
	if err := c.Send("a@remote.test", []string{"b@remote.test"}, []byte("x")); err == nil {
		t.Fatal("silent relay: want an error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("took %v: the session deadline did not fire", elapsed)
	}
}

func newRelayClient(t *testing.T, port int) *Client {
	t.Helper()
	return New(&models.Account{
		Email: "a@remote.test", SMTPHost: "127.0.0.1", SMTPPort: port,
		SMTPUsername: "a", SMTPPassword: "x", SMTPTLS: false,
	})
}

type timeoutErr struct{ msg string }

func (e *timeoutErr) Error() string   { return e.msg }
func (e *timeoutErr) Timeout() bool   { return true }
func (e *timeoutErr) Temporary() bool { return true }
