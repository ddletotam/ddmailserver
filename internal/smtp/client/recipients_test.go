package client

import (
	"strings"
	"testing"

	"github.com/yourusername/mailserver/internal/models"
)

// TestDisplayNameNeverReachesTheEnvelope is the regression. A recipient entered
// as "Имя Фамилия <addr>" used to be handed to net/smtp whole, which wrote
// `RCPT TO:<Имя Фамилия <addr>>` — nested brackets and a non-ASCII envelope —
// and the receiving server answered 555 5.5.2 Syntax error.
func TestDisplayNameNeverReachesTheEnvelope(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "cyrillic display name",
			in:   "Иван Петров <ivan@example.org>",
			want: []string{"ivan@example.org"},
		},
		{
			name: "ascii display name",
			in:   "John Doe <john@example.org>",
			want: []string{"john@example.org"},
		},
		{
			name: "bare address",
			in:   "plain@example.org",
			want: []string{"plain@example.org"},
		},
		{
			name: "rfc 2047 encoded display name",
			in:   "=?utf-8?B?0JjQstCw0L0=?= <ivan@example.org>",
			want: []string{"ivan@example.org"},
		},
		{
			name: "several recipients, mixed forms",
			in:   "Иван Петров <ivan@example.org>, plain@example.org, John Doe <john@example.org>",
			want: []string{"ivan@example.org", "plain@example.org", "john@example.org"},
		},
		{
			// The reason commas alone cannot be the delimiter.
			name: "comma inside a quoted display name",
			in:   `"Петров, Иван" <ivan@example.org>`,
			want: []string{"ivan@example.org"},
		},
		{
			name: "angle brackets with padding",
			in:   "  Иван Петров   < ivan@example.org >  ",
			want: []string{"ivan@example.org"},
		},
		{
			name: "empty list",
			in:   "",
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitAddresses(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("splitAddresses(%q) = %v, want %v", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("splitAddresses(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestEnvelopeAddressesAreCleanForAnyInput states the invariant directly: no
// matter how the address list was written, what goes into RCPT TO must be an
// addr-spec — no brackets, no spaces, no display name left over.
func TestEnvelopeAddressesAreCleanForAnyInput(t *testing.T) {
	inputs := []string{
		"Иван Петров <ivan@example.org>",
		`"Петров, Иван" <ivan@example.org>`,
		"=?utf-8?B?0JjQstCw0L0=?= <ivan@example.org>",
		"a@example.org, Имя <b@example.org>, c@example.org",
		"  spaced@example.org  ",
		"Имя> со странностями <weird@example.org>",
	}

	for _, in := range inputs {
		for _, addr := range splitAddresses(in) {
			if strings.ContainsAny(addr, "<> ") {
				t.Errorf("splitAddresses(%q) produced %q, which is not a bare addr-spec", in, addr)
			}
			if !strings.Contains(addr, "@") {
				t.Errorf("splitAddresses(%q) produced %q, which is not an address", in, addr)
			}
		}
	}
}

// TestMalformedEntryDoesNotCostTheOtherRecipients: mail.ParseAddressList is
// all-or-nothing, so a single unparseable entry must fall back to the naive
// split instead of silently dropping everyone in the list.
func TestMalformedEntryDoesNotCostTheOtherRecipients(t *testing.T) {
	got := splitAddresses("not-an-address, Иван Петров <ivan@example.org>")

	found := false
	for _, addr := range got {
		if addr == "ivan@example.org" {
			found = true
		}
	}
	if !found {
		t.Errorf("splitAddresses = %v, want the valid recipient to survive alongside a malformed one", got)
	}
}

// TestAllRecipientFieldsAreCollected: To, Cc and Bcc all belong in the envelope.
func TestAllRecipientFieldsAreCollected(t *testing.T) {
	msg := &models.OutboxMessage{
		To:  "Иван Петров <ivan@example.org>",
		Cc:  "copy@example.org",
		Bcc: "Скрытый <hidden@example.org>",
	}

	got := parseRecipientsFromOutbox(msg)
	want := []string{"ivan@example.org", "copy@example.org", "hidden@example.org"}

	if len(got) != len(want) {
		t.Fatalf("parseRecipientsFromOutbox = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("recipient %d = %q, want %q", i, got[i], want[i])
		}
	}
}
