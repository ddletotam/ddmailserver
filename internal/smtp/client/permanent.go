package client

import (
	"errors"
	"net"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
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

// deliveryStatus derives the RFC 3464 Status and Diagnostic-Code for a failed
// recipient from the delivery error.
func deliveryStatus(err error) (status, diagnostic string) {
	var tp *textproto.Error
	switch {
	case errors.Is(err, errNullMX):
		return "5.1.10", "Recipient domain does not accept mail (null MX)"
	case errors.As(err, &tp):
		msg := strings.TrimSpace(tp.Msg)
		status = "5.0.0"
		if tp.Code < 500 {
			status = "4.0.0"
		}
		if m := enhancedStatusRe.FindString(msg); m != "" {
			status = m
		}
		return status, "smtp; " + strings.TrimSpace(strconv.Itoa(tp.Code)+" "+msg)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return "5.1.2", "Host or domain name not found: " + dnsErr.Name
	}
	return "4.4.7", "Delivery time expired: " + err.Error()
}
