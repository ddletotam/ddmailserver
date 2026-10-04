package authfail

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"testing"
)

// TestIs: what counts as "the provider rejected the credentials". The cost of
// a false positive is an account paused for an hour over a network blip; of a
// false negative, the per-minute login storm this package exists to stop.
func TestIs(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"marked", Mark(errors.New("whatever")), true},
		{"marked and wrapped", fmt.Errorf("failed to connect: %w", Mark(errors.New("x"))), true},
		{"SMTP 535", fmt.Errorf("failed to send email: %w", &textproto.Error{Code: 535, Msg: "5.7.8 Error: authentication failed"}), true},
		{"SMTP 534 app password required", &textproto.Error{Code: 534, Msg: "5.7.9 Application-specific password required"}, true},
		{"SMTP 454 temporary auth failure", &textproto.Error{Code: 454, Msg: "4.7.0 Temporary authentication failure"}, false},
		{"SMTP 550 mailbox", &textproto.Error{Code: 550, Msg: "5.1.1 No such user"}, false},
		{"OAuth invalid_grant", errors.New("token refresh failed: map[error:invalid_grant error_description:Token has been expired or revoked.]"), true},
		{"go-webdav 401", errors.New("401 Unauthorized: unauthorized"), true},
		{"our REPORT 401", errors.New("REPORT returned 401"), true},
		{"our PUT 401", errors.New("PUT /x.ics failed with status 401: "), true},
		{"REPORT 404", errors.New("REPORT returned 404"), false},
		{"401 inside a number", errors.New("REPORT returned 4010"), false},
		// The old isAuthError matched "auth" anywhere and so paused accounts
		// over certificate errors.
		{"x509 unknown authority", fmt.Errorf("failed to connect: %w", x509.UnknownAuthorityError{}), false},
		{"text with authority", errors.New("x509: certificate signed by unknown authority"), false},
		{"connection refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, false},
	}
	for _, c := range cases {
		if got := Is(c.err); got != c.want {
			t.Errorf("%s: Is(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}

// TestMarkLoginRefusal: an IMAP LOGIN/AUTHENTICATE error is the server's
// verdict unless the connection failed or the server named a temporary cause.
func TestMarkLoginRefusal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// The production line that started this.
		{"Yandex revoked app password", errors.New("LOGIN invalid credentials or IMAP is disabled"), true},
		{"Gmail", errors.New("[AUTHENTICATIONFAILED] Invalid credentials (Failure)"), true},
		{"Microsoft", errors.New("AUTHENTICATE failed."), true},
		{"unavailable", errors.New("[UNAVAILABLE] Internal error, try again later"), false},
		{"too many connections", errors.New("[LIMIT] Too many simultaneous connections"), false},
		{"in use", errors.New("[INUSE] Mailbox in use"), false},
		{"connection closed mid-command", errors.New("imap: connection closed during command execution"), false},
		{"EOF", io.EOF, false},
		{"wrapped EOF", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), false},
		{"net error", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, false},
		{"timeout text", errors.New("read tcp 1.2.3.4:993: i/o timeout"), false},
	}
	for _, c := range cases {
		got := Is(MarkLoginRefusal(c.err))
		if got != c.want {
			t.Errorf("%s: Is(MarkLoginRefusal(%q)) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
	if MarkLoginRefusal(nil) != nil {
		t.Error("MarkLoginRefusal(nil) != nil")
	}
}

// TestMark_KeepsMessageAndChain: marking must not change what the user reads
// or hide the cause from errors.Is/As.
func TestMark_KeepsMessageAndChain(t *testing.T) {
	cause := io.ErrUnexpectedEOF
	m := Mark(fmt.Errorf("login: %w", cause))
	if m.Error() != "login: unexpected EOF" {
		t.Errorf("message changed: %q", m.Error())
	}
	if !errors.Is(m, cause) {
		t.Error("cause lost from the chain")
	}
	if Mark(nil) != nil {
		t.Error("Mark(nil) != nil")
	}
	if Mark(m) != m {
		t.Error("double marking wraps again")
	}
}
