package client

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/outbox"
	"github.com/ddletotam/ddmailserver/internal/parser"
	"github.com/ddletotam/ddmailserver/internal/web/locales"
)

// FailedRecipient is one recipient a message could not be delivered to.
type FailedRecipient struct {
	Address string // addr-spec
	Status  string // RFC 3463 enhanced status, e.g. "5.1.1"
	Reason  string // outbox.Reason* — what the sender is told
	// Diagnostic is the receiving server's reply ("smtp; 550 5.1.1 …"); empty
	// when there was none (timeout, refused connection, DNS).
	Diagnostic string
	// Detail is the raw error, for the log and outbox last_error only: a
	// «dial tcp …: connection timed out» means nothing to the sender.
	Detail string
}

// dsnLocales are the languages of the human-readable part of a DSN, in
// order. The server does not know which one the sender reads — the DSN is a
// letter, read in any client — so it says it in both, like the MTAs do.
func dsnLocales() []string { return []string{"ru", "en"} }

// dsnText is the translations a DSN is written with.
type dsnText map[string]map[string]string

func loadDSNText() dsnText {
	t := make(dsnText, len(dsnLocales()))
	for _, l := range dsnLocales() {
		m, err := locales.Load(l)
		if err != nil {
			log.Printf("DSN: %v", err)
			m = map[string]string{}
		}
		t[l] = m
	}
	return t
}

// get returns the text of key in locale, the English one if the locale has
// none, and the key itself as the last resort.
func (t dsnText) get(locale, key string) string {
	if s := t[locale][key]; s != "" {
		return s
	}
	if s := t["en"][key]; s != "" {
		return s
	}
	return key
}

// buildDSN composes a delivery status notification (RFC 3464,
// multipart/report) telling sender that original (Message-ID originalID,
// without angle brackets) could not be delivered to failed. Returns the
// message and its Message-ID.
//
// It is only ever put into the sender's own local mailbox, never sent out,
// so it cannot loop. Headers are kept 7-bit. In-Reply-To / References point
// at the original, so a client can show the bounce next to the message it is
// about. The human-readable part is UTF-8, Russian first, then English, with
// the reason in plain words; the receiving server's own reply, when there
// was one, follows it.
func buildDSN(hostname, sender string, original []byte, originalID string, failed []FailedRecipient, now time.Time) ([]byte, string) {
	if hostname == "" {
		hostname = "localhost"
	}
	text := loadDSNText()
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
	subject := text.get("ru", "dsn.subject")
	if origSubject != "" {
		subject += ": " + origSubject
	}

	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("From: %s <MAILER-DAEMON@%s>\r\n", parser.EncodeHeaderWord(text.get("en", "dsn.from_name")), hostname)
	w("To: <%s>\r\n", sender)
	w("Subject: %s\r\n", parser.EncodeHeaderWord(subject))
	w("Date: %s\r\n", now.Format(time.RFC1123Z))
	w("Message-ID: %s\r\n", messageID)
	if originalID != "" {
		w("In-Reply-To: <%s>\r\n", originalID)
		w("References: <%s>\r\n", originalID)
	}
	w("Auto-Submitted: auto-replied\r\n")
	w("MIME-Version: 1.0\r\n")
	w("Content-Type: multipart/report; report-type=delivery-status; boundary=\"%s\"\r\n", boundary)
	w("\r\n")

	// 1. Human-readable explanation, in every language.
	w("--%s\r\n", boundary)
	w("Content-Type: text/plain; charset=utf-8\r\n")
	w("Content-Transfer-Encoding: 8bit\r\n\r\n")
	for i, l := range dsnLocales() {
		if i > 0 {
			w("\r\n--\r\n\r\n")
		}
		w("%s\r\n\r\n", text.get(l, "dsn.intro"))
		for _, f := range failed {
			w("  %s\r\n    %s\r\n", f.Address, text.get(l, outbox.ReasonKey(reasonOrOther(f.Reason))))
			if reply := serverReply(f.Diagnostic); reply != "" {
				w("    %s: %s\r\n", text.get(l, "dsn.server_reply"), reply)
			}
		}
		w("\r\n%s\r\n", text.get(l, "dsn.headers_attached"))
	}
	w("\r\n")

	// 2. Machine-readable status (RFC 3464).
	w("--%s\r\n", boundary)
	w("Content-Type: message/delivery-status\r\n\r\n")
	w("Reporting-MTA: dns; %s\r\n", hostname)
	w("Arrival-Date: %s\r\n", now.Format(time.RFC1123Z))
	for _, f := range failed {
		w("\r\nFinal-Recipient: rfc822; %s\r\n", f.Address)
		w("Action: failed\r\n")
		w("Status: %s\r\n", f.Status)
		diag := f.Diagnostic
		if diag == "" {
			// No reply from the receiving side: say why in words, not
			// with the raw socket error.
			diag = "X-Local; " + text.get("en", outbox.ReasonKey(reasonOrOther(f.Reason)))
		}
		w("Diagnostic-Code: %s\r\n", diagnosticCode(diag))
	}
	w("\r\n")

	// 3. The original headers.
	w("--%s\r\n", boundary)
	w("Content-Type: text/rfc822-headers\r\n\r\n")
	b.Write(origHeaders)
	w("\r\n--%s--\r\n", boundary)

	return []byte(b.String()), messageID
}

func reasonOrOther(reason string) string {
	if reason == "" {
		return outbox.ReasonOther
	}
	return reason
}

// serverReply is the receiving server's reply without the "smtp;" type
// prefix, on one line.
func serverReply(diagnostic string) string {
	d := strings.NewReplacer("\r", " ", "\n", " ").Replace(diagnostic)
	if i := strings.Index(d, ";"); i >= 0 && strings.EqualFold(strings.TrimSpace(d[:i]), "smtp") {
		d = d[i+1:]
	}
	return strings.TrimSpace(d)
}

// diagnosticCode makes sure the Diagnostic-Code field has its type prefix
// and stays on one line.
func diagnosticCode(d string) string {
	d = strings.NewReplacer("\r", " ", "\n", " ").Replace(d)
	lower := strings.ToLower(d)
	if strings.HasPrefix(lower, "smtp;") || strings.HasPrefix(lower, "x-local;") {
		return d
	}
	return "X-Local; " + d
}
