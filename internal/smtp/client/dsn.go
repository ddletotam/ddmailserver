package client

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/parser"
)

// FailedRecipient is one recipient a message could not be delivered to.
type FailedRecipient struct {
	Address    string // addr-spec
	Status     string // RFC 3463 enhanced status, e.g. "5.1.1"
	Diagnostic string // the receiving side's reply or our reason
}

// buildDSN composes a delivery status notification (RFC 3464,
// multipart/report) telling sender that original could not be delivered to
// failed. Returns the message and its Message-ID.
//
// It is only ever put into the sender's own local mailbox, never sent out,
// so it cannot loop. Headers are kept 7-bit; the human-readable part is
// UTF-8, Russian first with an English line for non-Russian clients.
func buildDSN(hostname, sender string, original []byte, failed []FailedRecipient, now time.Time) ([]byte, string) {
	if hostname == "" {
		hostname = "localhost"
	}
	rnd := make([]byte, 8)
	_, _ = rand.Read(rnd)
	messageID := fmt.Sprintf("<dsn.%d.%s@%s>", now.UnixNano(), hex.EncodeToString(rnd), hostname)
	boundary := randomBoundary()

	origHeaders := original
	if i := bytes.Index(original, []byte("\r\n\r\n")); i >= 0 {
		origHeaders = original[:i+2]
	} else if i := bytes.Index(original, []byte("\n\n")); i >= 0 {
		origHeaders = original[:i+1]
	}

	origSubject := ""
	if parsed, err := parser.New().ParseBytes(original); err == nil {
		origSubject = parsed.Subject
	}
	subject := "Письмо не доставлено"
	if origSubject != "" {
		subject += ": " + origSubject
	}

	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("From: %s <MAILER-DAEMON@%s>\r\n", parser.EncodeHeaderWord("Mail Delivery System"), hostname)
	w("To: <%s>\r\n", sender)
	w("Subject: %s\r\n", parser.EncodeHeaderWord(subject))
	w("Date: %s\r\n", now.Format(time.RFC1123Z))
	w("Message-ID: %s\r\n", messageID)
	w("Auto-Submitted: auto-replied\r\n")
	w("MIME-Version: 1.0\r\n")
	w("Content-Type: multipart/report; report-type=delivery-status; boundary=\"%s\"\r\n", boundary)
	w("\r\n")

	// 1. Human-readable explanation.
	w("--%s\r\n", boundary)
	w("Content-Type: text/plain; charset=utf-8\r\n")
	w("Content-Transfer-Encoding: 8bit\r\n\r\n")
	w("Ваше письмо не удалось доставить следующим получателям:\r\n\r\n")
	for _, f := range failed {
		w("  %s\r\n    %s\r\n", f.Address, f.Diagnostic)
	}
	w("\r\nYour message could not be delivered to the recipients listed above.\r\n")
	w("Заголовки исходного письма приложены ниже.\r\n\r\n")

	// 2. Machine-readable status (RFC 3464).
	w("--%s\r\n", boundary)
	w("Content-Type: message/delivery-status\r\n\r\n")
	w("Reporting-MTA: dns; %s\r\n", hostname)
	w("Arrival-Date: %s\r\n", now.Format(time.RFC1123Z))
	for _, f := range failed {
		w("\r\nFinal-Recipient: rfc822; %s\r\n", f.Address)
		w("Action: failed\r\n")
		w("Status: %s\r\n", f.Status)
		if f.Diagnostic != "" {
			w("Diagnostic-Code: %s\r\n", diagnosticCode(f.Diagnostic))
		}
	}
	w("\r\n")

	// 3. The original headers.
	w("--%s\r\n", boundary)
	w("Content-Type: text/rfc822-headers\r\n\r\n")
	b.Write(origHeaders)
	w("\r\n--%s--\r\n", boundary)

	return []byte(b.String()), messageID
}

// diagnosticCode makes sure the Diagnostic-Code field has its type prefix
// and stays on one line.
func diagnosticCode(d string) string {
	d = strings.NewReplacer("\r", " ", "\n", " ").Replace(d)
	if strings.HasPrefix(strings.ToLower(d), "smtp;") {
		return d
	}
	return "X-Local; " + d
}
