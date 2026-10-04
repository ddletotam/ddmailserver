// Package authfail tells "the provider rejected our credentials" apart from
// every other failure, and keeps the pause between login attempts while it
// lasts (table auth_backoff, migrations/052_auth_backoff.sql).
//
// Why it exists: a revoked app password at Yandex 360 made the server log in
// with it once a minute from four places at once — IMAP sync, IDLE, CalDAV and
// CardDAV — with nothing ever slowing down. To the provider that looks like
// password guessing, and providers lock accounts for it. A network error is
// worth retrying soon; a rejected password is not going to start working until
// somebody changes it, so it gets a growing pause and a message to the user.
package authfail

import (
	"errors"
	"io"
	"net"
	"net/textproto"
	"regexp"
	"strings"
	"syscall"
)

// ErrRejected matches (errors.Is) every error marked as a credentials
// rejection by Mark.
var ErrRejected = errors.New("credentials rejected by the provider")

type rejected struct{ err error }

func (r *rejected) Error() string        { return r.err.Error() }
func (r *rejected) Unwrap() error        { return r.err }
func (r *rejected) Is(target error) bool { return target == ErrRejected }

// Mark wraps err so that Is reports it as a credentials rejection. The message
// is unchanged. Mark(nil) is nil.
func Mark(err error) error {
	if err == nil || errors.Is(err, ErrRejected) {
		return err
	}
	return &rejected{err: err}
}

// Is reports whether err means the provider does not accept the credentials:
// wrong or revoked password, revoked OAuth grant, HTTP 401.
//
// Marked errors are the reliable source — the IMAP client marks LOGIN and
// AUTHENTICATE refusals, the DAV clients a 401 seen on the wire, the OAuth
// clients an invalid_grant. Besides that a few unambiguous shapes are
// recognised in errors nobody marked: SMTP 535/534 (authentication failed /
// application-specific password required) and the textual forms of 401 and
// invalid_grant produced by libraries we do not control.
//
// Deliberately NOT matched: "auth" as a substring. The predecessor of this
// function did that and so took "x509: certificate signed by unknown
// authority" for a wrong password.
func Is(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrRejected) {
		return true
	}
	var tp *textproto.Error
	if errors.As(err, &tp) && (tp.Code == 535 || tp.Code == 534) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "invalid_grant") || http401.MatchString(msg)
}

// http401: "401 Unauthorized" (go-webdav), "REPORT returned 401", "PUT … failed
// with status 401", "HTTP 401" — and not 4010 or 1401.
var http401 = regexp.MustCompile(`\b401 unauthorized\b|\b(returned|status|http) 401\b`)

// MarkLoginRefusal classifies the error of an IMAP LOGIN / AUTHENTICATE
// command. go-imap returns the server's NO/BAD text as a plain error, so what
// is not a transport failure and not a temporary condition the server named
// itself is the server refusing these credentials — "LOGIN invalid credentials
// or IMAP is disabled" (Yandex), "[AUTHENTICATIONFAILED] Invalid credentials"
// (Gmail) and so on. Those are marked; everything else is returned unchanged.
func MarkLoginRefusal(err error) error {
	if err == nil || isTransport(err) || looksTemporary(err.Error()) {
		return err
	}
	return Mark(err)
}

// isTransport: the connection failed, the server said nothing about us.
func isTransport(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"connection closed",
		"use of closed network connection",
		"connection reset",
		"broken pipe",
		"i/o timeout",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// looksTemporary: the server refused, but said it is its own problem or a
// limit, not the password (RFC 5530 response codes and common wording).
func looksTemporary(msg string) bool {
	msg = strings.ToLower(msg)
	for _, s := range []string{
		"[unavailable]",
		"[inuse]",
		"[limit]",
		"[serverbug]",
		"try again",
		"temporar",
		"too many",
		"internal error",
		"server error",
		"system error",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
