package parser

import (
	"strings"
	"testing"
)

const noIDMsg = "Date: Wed, 24 Sep 2026 21:35:02 +0300\r\n" +
	"From: avia@example.com\r\n" +
	"To: dd@example.org\r\n" +
	"Subject: =?UTF-8?B?0J/QvtGB0LDQtNC+0YfQvdGL0Lk=?=\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"boarding pass\r\n"

func TestDeriveMessageIDStable(t *testing.T) {
	a := DeriveMessageID([]byte(noIDMsg))
	b := DeriveMessageID([]byte(noIDMsg))
	if a == "" || a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "<noid.") || !strings.HasSuffix(a, "@ddmail.invalid>") {
		t.Fatalf("unexpected format %q", a)
	}
	if !IsSyntheticMessageID(a) {
		t.Fatalf("IsSyntheticMessageID(%q) = false", a)
	}
}

// The same message fetched from two upstream accounts carries different
// transport headers and may differ in line endings; it must keep one identity.
func TestDeriveMessageIDIgnoresTransport(t *testing.T) {
	base := DeriveMessageID([]byte(noIDMsg))
	relayed := "Received: from mx1.example.net by mx2.example.net\r\n" +
		"Delivered-To: other@example.org\r\n" +
		"X-Spam-Score: 0.1\r\n" +
		strings.ReplaceAll(noIDMsg, "\r\n", "\n") + "\n"
	if got := DeriveMessageID([]byte(relayed)); got != base {
		t.Fatalf("transport headers changed identity: %q vs %q", got, base)
	}
	folded := strings.Replace(noIDMsg, "To: dd@example.org", "To:\r\n dd@example.org", 1)
	if got := DeriveMessageID([]byte(folded)); got != base {
		t.Fatalf("header folding changed identity: %q vs %q", got, base)
	}
}

// Repeated notifications (same subject/sender) are distinct messages.
func TestDeriveMessageIDDistinct(t *testing.T) {
	base := DeriveMessageID([]byte(noIDMsg))
	cases := map[string]string{
		"date": strings.Replace(noIDMsg, "21:35:02", "21:35:03", 1),
		"body": strings.Replace(noIDMsg, "boarding pass", "boarding pass 2", 1),
		"to":   strings.Replace(noIDMsg, "dd@example.org", "ee@example.org", 1),
	}
	for name, m := range cases {
		if DeriveMessageID([]byte(m)) == base {
			t.Errorf("%s change did not change identity", name)
		}
	}
}

// Without a Date the identity headers are too weak; the whole raw message is
// hashed, so transport headers do count there.
func TestDeriveMessageIDNoDateFallsBackToRaw(t *testing.T) {
	noDate := strings.Replace(noIDMsg, "Date: Wed, 24 Sep 2026 21:35:02 +0300\r\n", "", 1)
	a := DeriveMessageID([]byte(noDate))
	b := DeriveMessageID([]byte("Received: x\r\n" + noDate))
	if a == "" || a == b {
		t.Fatalf("expected raw-hash fallback to differ: %q vs %q", a, b)
	}
	if DeriveMessageID(nil) != "" {
		t.Fatal("empty input must yield empty id")
	}
}

func TestStripSyntheticMessageIDs(t *testing.T) {
	syn := DeriveMessageID([]byte(noIDMsg))
	in := "<a@example.com> " + syn + " <b@example.com>"
	if got := StripSyntheticMessageIDs(in); got != "<a@example.com> <b@example.com>" {
		t.Fatalf("got %q", got)
	}
	if got := StripSyntheticMessageIDs(syn); got != "" {
		t.Fatalf("got %q", got)
	}
	if IsSyntheticMessageID("<abc@example.com>") {
		t.Fatal("real id reported synthetic")
	}
}
