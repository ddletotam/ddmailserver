package client

import (
	"errors"
	"net"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/outbox"
	"github.com/ddletotam/ddmailserver/internal/smtp/localdelivery"
)

// errNullMX: the domain publishes a "null MX" (RFC 7505, `MX 0 .`) — it
// declares that it accepts no mail at all. Dialling the "." host used to
// produce `dial tcp :25` and six pointless attempts over 17 hours.
var errNullMX = errors.New("domain does not accept mail (null MX)")

// IsPermanent reports whether a delivery error is one that retrying cannot
// fix, so the message should fail (and bounce) now instead of spending the
// whole retry schedule:
//
//   - a 5xx reply from the receiving server (RFC 5321 §4.2.1: permanent);
//   - the recipient domain does not exist (NXDOMAIN, no MX and no A);
//   - the domain refuses all mail with a null MX.
//
// Everything else — refused or timed-out connections, 4xx, DNS SERVFAIL —
// is transient and goes through the backoff schedule.
func IsPermanent(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errNullMX) {
		return true
	}
	var tp *textproto.Error
	if errors.As(err, &tp) {
		return tp.Code >= 500 && tp.Code < 600
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsNotFound
	}
	return false
}

var enhancedStatusRe = regexp.MustCompile(`^([245])\.(\d{1,3})\.(\d{1,3})\b`)

// failedRecipient describes a recipient the message could not be delivered
// to because of err: the RFC 3464 Status, the receiving server's reply for
// Diagnostic-Code (none when it never replied), and the reason code the
// sender is shown (outbox.Reason*). The raw error goes to Detail — logs and
// outbox last_error only.
//
// An error without an SMTP reply (timeout, refused connection) gets 4.4.7
// «delivery time expired»: by the time it is reported, retries ran out.
func failedRecipient(addr string, err error) FailedRecipient {
	f := FailedRecipient{Address: addr, Reason: reasonFor(err), Detail: err.Error()}
	var tp *textproto.Error
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, errNullMX):
		f.Status = "5.1.10"
	case errors.As(err, &tp):
		msg := strings.TrimSpace(tp.Msg)
		f.Status = "5.0.0"
		if tp.Code < 500 {
			f.Status = "4.0.0"
		}
		if m := enhancedStatusRe.FindString(msg); m != "" {
			f.Status = m
		}
		f.Diagnostic = "smtp; " + strings.TrimSpace(strconv.Itoa(tp.Code)+" "+msg)
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		f.Status = "5.1.2"
	default:
		f.Status = "4.4.7"
	}
	return f
}

// reasonFor maps a delivery error to the reason code shown to the sender.
func reasonFor(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errNullMX) {
		return outbox.ReasonNoMailDomain
	}

	var reject *localdelivery.RejectError
	if errors.As(err, &reject) {
		return reasonForStatus(reject.Code, reject.Status(), true)
	}

	var tp *textproto.Error
	if errors.As(err, &tp) {
		status := enhancedStatusRe.FindString(strings.TrimSpace(tp.Msg))
		// A refused RCPT is about the address; a refusal elsewhere (MAIL,
		// DATA) is about the message or the sender.
		atRcpt := strings.Contains(err.Error(), "RCPT TO") || strings.Contains(err.Error(), "failed to set recipient")
		return reasonForStatus(tp.Code, status, atRcpt)
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return outbox.ReasonDomainNotFound
		}
		return outbox.ReasonUnreachable
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		// Connect / read timeouts, refused and reset connections, no route:
		// the receiving server could not be talked to.
		return outbox.ReasonUnreachable
	}
	return outbox.ReasonOther
}

// reasonForStatus maps an SMTP reply code and its enhanced status to a reason.
func reasonForStatus(code int, status string, atRcpt bool) string {
	switch {
	case code >= 400 && code < 500:
		return outbox.ReasonTemporary
	case status == "5.2.2":
		return outbox.ReasonMailboxFull
	case strings.HasPrefix(status, "5.1.") || status == "5.2.1":
		return outbox.ReasonMailboxUnavailable
	case status != "":
		// The enhanced status says what it is (5.7.1 policy, 5.3.4 size…).
		return outbox.ReasonRejected
	case atRcpt && code == 552:
		return outbox.ReasonMailboxFull
	case atRcpt && (code == 550 || code == 551 || code == 553):
		return outbox.ReasonMailboxUnavailable
	default:
		return outbox.ReasonRejected
	}
}
