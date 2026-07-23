package parser

import (
	"mime"
	"strings"
)

// EncodeHeaderWord RFC 2047-encodes a header value when it contains non-ASCII
// octets; pure-ASCII input is returned unchanged (so plain headers stay
// human-readable on the wire). Used for Subject and other free-text headers.
//
// Why this matters: a raw UTF-8 Subject (e.g. Cyrillic) makes 8-bit octets
// appear in the header section. Relays like iCloud then advertise/require the
// SMTPUTF8 extension (RFC 6531) when forwarding, and older receiving MTAs that
// don't offer SMTPUTF8 reject the message ("SMTPUTF8 is required, but was not
// offered by host …"). Encoding the header to 7-bit ASCII removes that
// requirement, so the mail delivers to legacy servers.
func EncodeHeaderWord(s string) string {
	return mime.QEncoding.Encode("utf-8", s)
}

// EncodeAddressHeader encodes only the display-name part of each address in a
// comma-separated From/To/Cc list, leaving the addr-spec (<user@domain> or a
// bare address) untouched. "Имя <a@b>" → "=?utf-8?q?...?= <a@b>"; a plain
// "a@b" passes through. Keeps address headers 7-bit for the same SMTPUTF8
// reason as EncodeHeaderWord.
func EncodeAddressHeader(list string) string {
	parts := strings.Split(list, ",")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			parts[i] = p
			continue
		}
		if lt := strings.LastIndex(p, "<"); lt > 0 {
			name := strings.TrimSpace(p[:lt])
			addr := p[lt:]
			parts[i] = strings.TrimSpace(EncodeHeaderWord(name) + " " + addr)
		} else {
			parts[i] = p
		}
	}
	return strings.Join(parts, ", ")
}
