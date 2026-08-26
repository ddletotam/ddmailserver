package mobileconfig

import (
	"errors"
	"strings"
	"testing"
)

const sampleProfile = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>PayloadContent</key>
    <array>
      <dict>
        <key>EmailAccountType</key>
        <string>EmailTypeIMAP</string>
        <key>IncomingMailServerHostName</key>
        <string>mail.example.org</string>
        <key>IncomingMailServerPortNumber</key>
        <integer>993</integer>
        <key>IncomingMailServerUseSSL</key>
        <true/>
        <key>PreventMove</key>
        <false/>
        <key>PayloadType</key>
        <string>com.apple.mail.managed</string>
      </dict>
      <dict>
        <key>CalDAVPort</key>
        <real>443</real>
        <key>CalDAVPrincipalURL</key>
        <string>/SOGo/dav/user@example.org</string>
        <key>PayloadType</key>
        <string>com.apple.caldav.account</string>
      </dict>
    </array>
    <key>PayloadDisplayName</key>
    <string>user@example.org</string>
    <key>PayloadType</key>
    <string>Configuration</string>
  </dict>
</plist>
`

func TestParsePlistStructure(t *testing.T) {
	root, err := ParsePlist([]byte(sampleProfile))
	if err != nil {
		t.Fatalf("ParsePlist: %v", err)
	}

	if root.Kind != KindDict {
		t.Fatalf("root kind = %v, want dict", root.Kind)
	}

	if got := root.StringOr("PayloadDisplayName", ""); got != "user@example.org" {
		t.Errorf("PayloadDisplayName = %q", got)
	}

	content := root.Get("PayloadContent")
	payloads := content.Children()
	if len(payloads) != 2 {
		t.Fatalf("got %d payloads, want 2", len(payloads))
	}

	mail := payloads[0]
	if got := mail.StringOr("PayloadType", ""); got != "com.apple.mail.managed" {
		t.Errorf("payload 0 type = %q", got)
	}
	if got := mail.IntOr("IncomingMailServerPortNumber", 0); got != 993 {
		t.Errorf("IMAP port = %d, want 993", got)
	}
	if got := mail.BoolOr("IncomingMailServerUseSSL", false); !got {
		t.Error("IncomingMailServerUseSSL should be true")
	}
	if got := mail.BoolOr("PreventMove", true); got {
		t.Error("PreventMove should be false")
	}
}

// TestParsePlistPortTypesInterchangeable is the concrete bug this parser was
// written to survive: a real SOGo profile carries CalDAVPort as <real>443</real>
// and CardDAVPort as <integer>443</integer> in the same document. A reader that
// only accepts <integer> silently loses half the ports it is handed.
func TestParsePlistPortTypesInterchangeable(t *testing.T) {
	doc := `<plist version="1.0"><dict>
	  <key>AsReal</key><real>443</real>
	  <key>AsInteger</key><integer>443</integer>
	  <key>AsString</key><string>443</string>
	</dict></plist>`

	root, err := ParsePlist([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePlist: %v", err)
	}

	for _, key := range []string{"AsReal", "AsInteger", "AsString"} {
		got, ok := root.Int(key)
		if !ok {
			t.Errorf("%s: not readable as an int", key)
			continue
		}
		if got != 443 {
			t.Errorf("%s = %d, want 443", key, got)
		}
	}
}

// TestParsePlistBoolSpellings: profiles carry booleans as <true/>, as 1, and as
// the strings "true"/"YES" depending on who generated them.
func TestParsePlistBoolSpellings(t *testing.T) {
	doc := `<plist version="1.0"><dict>
	  <key>Canonical</key><true/>
	  <key>Numeric</key><integer>1</integer>
	  <key>Textual</key><string>YES</string>
	  <key>TextualFalse</key><string>no</string>
	  <key>NumericFalse</key><integer>0</integer>
	</dict></plist>`

	root, err := ParsePlist([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePlist: %v", err)
	}

	for _, key := range []string{"Canonical", "Numeric", "Textual"} {
		if got, ok := root.Bool(key); !ok || !got {
			t.Errorf("%s = (%v, %v), want (true, true)", key, got, ok)
		}
	}
	for _, key := range []string{"TextualFalse", "NumericFalse"} {
		if got, ok := root.Bool(key); !ok || got {
			t.Errorf("%s = (%v, %v), want (false, true)", key, got, ok)
		}
	}
}

// TestParsePlistRejectsSignedProfile: a CMS/PKCS#7 profile is a normal thing to
// be handed and cannot be parsed as XML. The user needs to be told that, not
// shown a syntax error at byte 1.
func TestParsePlistRejectsSignedProfile(t *testing.T) {
	// DER SEQUENCE header — how a signed profile starts.
	der := []byte{0x30, 0x82, 0x0a, 0x1f, 0x06, 0x09, 0x2a}

	_, err := ParsePlist(der)
	if !errors.Is(err, ErrSignedProfile) {
		t.Fatalf("got %v, want ErrSignedProfile", err)
	}
	if !strings.Contains(err.Error(), "signed") {
		t.Errorf("error should mention signing: %v", err)
	}
}

// TestParsePlistRejectsBinaryPlist — same reasoning as the signed case.
func TestParsePlistRejectsBinaryPlist(t *testing.T) {
	_, err := ParsePlist([]byte("bplist00\xd1\x01\x02"))
	if !errors.Is(err, ErrSignedProfile) {
		t.Fatalf("got %v, want ErrSignedProfile", err)
	}
}

// TestParsePlistUnknownElementsSkipped: vendor extensions must not sink the
// whole file.
func TestParsePlistUnknownElementsSkipped(t *testing.T) {
	doc := `<plist version="1.0"><dict>
	  <key>Known</key><string>value</string>
	  <key>Weird</key><vendor-thing><nested>x</nested></vendor-thing>
	  <key>After</key><string>still here</string>
	</dict></plist>`

	root, err := ParsePlist([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePlist: %v", err)
	}

	if got := root.StringOr("Known", ""); got != "value" {
		t.Errorf("Known = %q", got)
	}
	if got := root.StringOr("After", ""); got != "still here" {
		t.Errorf("After = %q — parsing did not recover past the unknown element", got)
	}
}

// TestParsePlistMalformed: truncated input must error, not hang or panic.
func TestParsePlistMalformed(t *testing.T) {
	cases := map[string]string{
		"truncated dict":  `<plist version="1.0"><dict><key>A</key><string>x</string>`,
		"truncated array": `<plist version="1.0"><array><string>x</string>`,
		"empty plist":     `<plist version="1.0"></plist>`,
		"not a plist":     `<html><body>login page</body></html>`,
		"empty input":     ``,
	}

	for name, doc := range cases {
		if _, err := ParsePlist([]byte(doc)); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

// TestParsePlistKeysInDocumentOrder — payload order is meaningful when
// reporting what a profile contains back to the user.
func TestParsePlistKeysInDocumentOrder(t *testing.T) {
	doc := `<plist version="1.0"><dict>
	  <key>Zebra</key><string>1</string>
	  <key>Alpha</key><string>2</string>
	  <key>Mike</key><string>3</string>
	</dict></plist>`

	root, err := ParsePlist([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePlist: %v", err)
	}

	want := []string{"Zebra", "Alpha", "Mike"}
	got := root.Keys()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestRoundTrip: what Generate writes, ParsePlist must read. The two halves of
// this feature have to agree, and nothing else checks that they do.
func TestRoundTrip(t *testing.T) {
	out, err := Generate(testProfile())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	root, err := ParsePlist(out)
	if err != nil {
		t.Fatalf("ParsePlist on our own output: %v", err)
	}

	payloads := root.Get("PayloadContent").Children()
	if len(payloads) != 3 {
		t.Fatalf("got %d payloads, want 3", len(payloads))
	}

	byType := make(map[string]*Value)
	for _, p := range payloads {
		byType[p.StringOr("PayloadType", "")] = p
	}

	mail := byType["com.apple.mail.managed"]
	if mail == nil {
		t.Fatal("no mail payload")
	}
	if got := mail.IntOr("IncomingMailServerPortNumber", 0); got != 993 {
		t.Errorf("IMAP port = %d, want 993", got)
	}
	if got := mail.StringOr("IncomingPassword", ""); got != "abcd-efgh-ijkl-mnop" {
		t.Errorf("password = %q", got)
	}
	if !mail.BoolOr("OutgoingPasswordSameAsIncomingPassword", false) {
		t.Error("OutgoingPasswordSameAsIncomingPassword should be true")
	}

	caldav := byType["com.apple.caldav.account"]
	if caldav == nil {
		t.Fatal("no CalDAV payload")
	}
	if got := caldav.StringOr("CalDAVPrincipalURL", ""); got != "/caldav/" {
		t.Errorf("CalDAV principal = %q", got)
	}
	if got := caldav.IntOr("CalDAVPort", 0); got != 443 {
		t.Errorf("CalDAV port = %d", got)
	}
}
