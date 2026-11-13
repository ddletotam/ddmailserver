package client

import (
	"net/mail"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/models"
)

func parseRecipientsFromOutbox(msg *models.OutboxMessage) []string {
	var recipients []string
	for _, addr := range splitAddresses(msg.To) {
		if addr != "" {
			recipients = append(recipients, addr)
		}
	}
	for _, addr := range splitAddresses(msg.Cc) {
		if addr != "" {
			recipients = append(recipients, addr)
		}
	}
	for _, addr := range splitAddresses(msg.Bcc) {
		if addr != "" {
			recipients = append(recipients, addr)
		}
	}
	return recipients
}

// splitAddresses reduces a header-style address list to the bare addr-specs the
// SMTP envelope takes. Shared by both send paths — the relay (SendTask) and
// direct MX (DirectSendTask).
//
// RCPT TO carries an address and nothing else. net/smtp writes what it is given
// straight into `RCPT TO:<%s>`, so a display name surviving this far produces
// nested angle brackets, and with a non-ASCII name a non-ASCII envelope on top
// of that; receivers answer 555 5.5.2 Syntax error, as they should.
//
// mail.ParseAddressList goes first because it is the only thing here that gets
// quoting right: a display name may legitimately contain a comma, and splitting
// on commas alone turns `"Doe, John" <j@d>` into two recipients that do not
// exist. It is all-or-nothing, though, so one malformed entry falls back to the
// naive split rather than costing every recipient in the list.
func splitAddresses(s string) []string {
	if s == "" {
		return nil
	}

	if parsed, err := mail.ParseAddressList(s); err == nil {
		addrs := make([]string, 0, len(parsed))
		for _, a := range parsed {
			if a.Address != "" {
				addrs = append(addrs, a.Address)
			}
		}
		if len(addrs) > 0 {
			return addrs
		}
	}

	var addrs []string
	for _, part := range splitComma(s) {
		email := extractEmailAddr(part)
		if email != "" {
			addrs = append(addrs, email)
		}
	}
	return addrs
}

func splitComma(s string) []string {
	var parts []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

func extractEmailAddr(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "<"); idx != -1 {
		// Search for the closing bracket after the opening one, not from the
		// start of the string.
		if end := strings.Index(s[idx:], ">"); end > 0 {
			return strings.TrimSpace(s[idx+1 : idx+end])
		}
	}
	if strings.Contains(s, "@") {
		return s
	}
	return ""
}
