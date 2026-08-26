package mobileconfig

import (
	"encoding/xml"
	"strings"
	"testing"
)

func testProfile() *Profile {
	return &Profile{
		DisplayName:  "Почта letotam",
		Organization: "letotam",
		AccountName:  "Денис Данилин",
		EmailAddress: "lucky@letotam.ru",
		Username:     "lucky",
		Secret:       "abcd-efgh-ijkl-mnop",
		Hostname:     "mail.letotam.ru",
		IMAPPort:     993,
		SMTPPort:     465,
		HTTPSPort:    443,
		CalDAVPath:   "/caldav/",
		CardDAVPath:  "/carddav/",
	}
}

// TestGenerateIsWellFormedXML is the floor: a device rejects the whole profile
// if the document does not parse, and nothing else in this file matters then.
func TestGenerateIsWellFormedXML(t *testing.T) {
	out, err := Generate(testProfile())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	decoder := xml.NewDecoder(strings.NewReader(string(out)))
	decoder.Strict = true
	for {
		_, err := decoder.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("malformed XML: %v\n%s", err, out)
		}
	}
}

// TestGenerateCarriesPublicPorts guards the trap this deployment sets: the
// server listens on 10993/10465 and the firewall redirects, so a profile built
// from the listen ports would send phones somewhere nothing answers.
func TestGenerateCarriesPublicPorts(t *testing.T) {
	out, err := Generate(testProfile())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := string(out)

	for _, want := range []string{
		"<key>IncomingMailServerPortNumber</key>\n        <integer>993</integer>",
		"<key>OutgoingMailServerPortNumber</key>\n        <integer>465</integer>",
		"<key>CalDAVPort</key>\n        <integer>443</integer>",
		"<key>CardDAVPort</key>\n        <integer>443</integer>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("profile is missing:\n%s", want)
		}
	}

	for _, unwanted := range []string{"10993", "10465", "1143", "1587"} {
		if strings.Contains(s, unwanted) {
			t.Errorf("profile leaked internal listen port %s", unwanted)
		}
	}
}

// TestGenerateAllThreePayloads checks that one export configures mail, calendar
// and contacts — the whole point of handing over a profile instead of three
// sets of instructions.
func TestGenerateAllThreePayloads(t *testing.T) {
	out, err := Generate(testProfile())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := string(out)

	for _, want := range []string{
		"com.apple.mail.managed",
		"com.apple.caldav.account",
		"com.apple.carddav.account",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing payload type %s", want)
		}
	}
}

// TestGenerateOmitsDAVPayloadsWhenUnset lets a caller emit a mail-only profile.
func TestGenerateOmitsDAVPayloadsWhenUnset(t *testing.T) {
	p := testProfile()
	p.CalDAVPath = ""
	p.CardDAVPath = ""

	out, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := string(out)

	if !strings.Contains(s, "com.apple.mail.managed") {
		t.Error("mail payload should always be present")
	}
	if strings.Contains(s, "caldav") || strings.Contains(s, "carddav") {
		t.Error("DAV payloads should be omitted when their paths are empty")
	}
}

// TestGenerateWithoutSecretOmitsPasswordKeys covers the "let the device ask"
// mode: no password key at all, rather than an empty one, which iOS would
// happily install as a blank password.
func TestGenerateWithoutSecretOmitsPasswordKeys(t *testing.T) {
	p := testProfile()
	p.Secret = ""

	out, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := string(out)

	// Exact tags: OutgoingPasswordSameAsIncomingPassword legitimately contains
	// the substring "IncomingPassword" and must survive — it tells the device
	// to reuse whatever the user types for incoming.
	for _, key := range []string{"IncomingPassword", "CalDAVPassword", "CardDAVPassword"} {
		if strings.Contains(s, "<key>"+key+"</key>") {
			t.Errorf("profile without a secret still carries %s", key)
		}
	}
	if !strings.Contains(s, "<key>OutgoingPasswordSameAsIncomingPassword</key>") {
		t.Error("device should still be told to reuse the incoming password")
	}
	if !strings.Contains(s, "<key>IncomingMailServerUsername</key>") {
		t.Error("username should still be configured")
	}
}

// TestGenerateIdentifiersAreStable is what makes re-export replace the profile
// on the device instead of stacking a second copy next to it.
func TestGenerateIdentifiersAreStable(t *testing.T) {
	first, err := Generate(testProfile())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// A rotated password must not change the profile's identity.
	p := testProfile()
	p.Secret = "zyxw-vuts-rqpo-nmlk"
	second, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	id := "ru.letotam.mail.profile.lucky"
	for _, out := range [][]byte{first, second} {
		if !strings.Contains(string(out), "<string>"+id+"</string>") {
			t.Fatalf("expected identifier %s in:\n%s", id, out)
		}
	}

	uuid1 := extractFirst(t, string(first), "PayloadUUID")
	uuid2 := extractFirst(t, string(second), "PayloadUUID")
	if uuid1 != uuid2 {
		t.Errorf("UUID changed between exports: %s vs %s", uuid1, uuid2)
	}
	if len(uuid1) != 36 {
		t.Errorf("UUID %q is not 36 characters", uuid1)
	}
}

// TestGenerateEscapesDisplayText pins that arbitrary account names cannot break
// the document. "Иванов & Co" is exactly the kind of name that does.
func TestGenerateEscapesDisplayText(t *testing.T) {
	p := testProfile()
	p.AccountName = `Иванов & Co <"boss">`

	out, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := string(out)

	if !strings.Contains(s, "&amp;") || !strings.Contains(s, "&lt;") || !strings.Contains(s, "&quot;") {
		t.Error("display text was not escaped")
	}

	decoder := xml.NewDecoder(strings.NewReader(s))
	for {
		_, err := decoder.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("escaping produced malformed XML: %v", err)
		}
	}
}

// TestGenerateRequiresIdentity: a profile with no address or no login is not a
// profile, and failing loudly beats emitting one a device silently rejects.
func TestGenerateRequiresIdentity(t *testing.T) {
	cases := map[string]func(*Profile){
		"no hostname": func(p *Profile) { p.Hostname = "" },
		"no email":    func(p *Profile) { p.EmailAddress = "" },
		"no username": func(p *Profile) { p.Username = "" },
	}

	for name, mutate := range cases {
		p := testProfile()
		mutate(p)
		if _, err := Generate(p); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

func extractFirst(t *testing.T, doc, key string) string {
	t.Helper()

	marker := "<key>" + key + "</key>"
	i := strings.Index(doc, marker)
	if i == -1 {
		t.Fatalf("key %s not found", key)
	}
	rest := doc[i+len(marker):]

	open := strings.Index(rest, "<string>")
	closed := strings.Index(rest, "</string>")
	if open == -1 || closed == -1 {
		t.Fatalf("no string value after %s", key)
	}
	return rest[open+len("<string>") : closed]
}
