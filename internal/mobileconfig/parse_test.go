package mobileconfig

import (
	"errors"
	"strings"
	"testing"
)

// sogoProfile reproduces the shape of a real SOGo-generated profile: three
// payloads for one identity, a principal URL rather than a collection URL, and
// CalDAVPort as <real> next to CardDAVPort as <integer>.
const sogoProfile = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
  <dict>
    <key>PayloadContent</key>
    <array>
      <dict>
        <key>EmailAccountType</key><string>EmailTypeIMAP</string>
        <key>EmailAccountName</key><string>Тестовый Пользователь</string>
        <key>EmailAddress</key><string>user@example.org</string>
        <key>IncomingMailServerHostName</key><string>mail.example.org</string>
        <key>IncomingMailServerPortNumber</key><integer>993</integer>
        <key>IncomingMailServerUseSSL</key><true/>
        <key>IncomingMailServerUsername</key><string>user@example.org</string>
        <key>IncomingPassword</key><string>s3cr3t</string>
        <key>OutgoingMailServerHostName</key><string>mail.example.org</string>
        <key>OutgoingMailServerPortNumber</key><integer>465</integer>
        <key>OutgoingMailServerUseSSL</key><true/>
        <key>OutgoingPasswordSameAsIncomingPassword</key><true/>
        <key>PayloadType</key><string>com.apple.mail.managed</string>
      </dict>
      <dict>
        <key>CalDAVHostName</key><string>mail.example.org</string>
        <key>CalDAVPort</key><real>443</real>
        <key>CalDAVPrincipalURL</key><string>/SOGo/dav/user@example.org</string>
        <key>CalDAVUseSSL</key><true/>
        <key>CalDAVUsername</key><string>user@example.org</string>
        <key>CalDAVPassword</key><string>s3cr3t</string>
        <key>PayloadType</key><string>com.apple.caldav.account</string>
      </dict>
      <dict>
        <key>CardDAVHostName</key><string>mail.example.org</string>
        <key>CardDAVPort</key><integer>443</integer>
        <key>CardDAVPrincipalURL</key><string>/SOGo/dav/user@example.org</string>
        <key>CardDAVUseSSL</key><true/>
        <key>CardDAVUsername</key><string>user@example.org</string>
        <key>CardDAVPassword</key><string>s3cr3t</string>
        <key>PayloadType</key><string>com.apple.carddav.account</string>
      </dict>
    </array>
    <key>PayloadDisplayName</key><string>user@example.org</string>
    <key>PayloadOrganization</key><string>Example</string>
    <key>PayloadType</key><string>Configuration</string>
  </dict>
</plist>`

func TestParseSOGoProfile(t *testing.T) {
	p, err := Parse([]byte(sogoProfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if p.Mail == nil || p.CalDAV == nil || p.CardDAV == nil {
		t.Fatalf("expected all three payloads, got mail=%v caldav=%v carddav=%v",
			p.Mail != nil, p.CalDAV != nil, p.CardDAV != nil)
	}

	if p.Mail.EmailAddress != "user@example.org" {
		t.Errorf("email = %q", p.Mail.EmailAddress)
	}
	if p.Mail.IMAPPort != 993 || p.Mail.SMTPPort != 465 {
		t.Errorf("ports = %d/%d, want 993/465", p.Mail.IMAPPort, p.Mail.SMTPPort)
	}
	if p.Mail.AccountName != "Тестовый Пользователь" {
		t.Errorf("account name = %q", p.Mail.AccountName)
	}
}

// TestParsePropagatesSharedPassword: the flag exists so the profile need not
// repeat the secret, and an import that ignored it would create an account
// that receives mail but cannot send any.
func TestParsePropagatesSharedPassword(t *testing.T) {
	p, err := Parse([]byte(sogoProfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if p.Mail.SMTPPassword != "s3cr3t" {
		t.Errorf("SMTP password = %q, want the incoming one propagated", p.Mail.SMTPPassword)
	}
	if p.Mail.SMTPUsername != "user@example.org" {
		t.Errorf("SMTP username = %q, want the incoming one propagated", p.Mail.SMTPUsername)
	}
}

// TestDAVURLAssembly: the profile gives host and path separately, and the port
// must be elided when it is the scheme default — "https://h:443/x" is legal but
// noise, and some servers compare the Host header literally.
func TestDAVURLAssembly(t *testing.T) {
	cases := []struct {
		name string
		dav  DAVAccount
		want string
	}{
		{
			name: "https on default port",
			dav:  DAVAccount{Hostname: "mail.example.org", Port: 443, PrincipalURL: "/SOGo/dav/user", UseSSL: true},
			want: "https://mail.example.org/SOGo/dav/user",
		},
		{
			name: "https on a custom port",
			dav:  DAVAccount{Hostname: "mail.example.org", Port: 8443, PrincipalURL: "/dav/", UseSSL: true},
			want: "https://mail.example.org:8443/dav/",
		},
		{
			name: "plain http",
			dav:  DAVAccount{Hostname: "dav.local", Port: 80, PrincipalURL: "/dav", UseSSL: false},
			want: "http://dav.local/dav",
		},
		{
			name: "path without a leading slash",
			dav:  DAVAccount{Hostname: "h", Port: 443, PrincipalURL: "dav/user", UseSSL: true},
			want: "https://h/dav/user",
		},
		{
			name: "no path at all",
			dav:  DAVAccount{Hostname: "h", Port: 443, UseSSL: true},
			want: "https://h/",
		},
	}

	for _, c := range cases {
		if got := c.dav.URL(); got != c.want {
			t.Errorf("%s: URL() = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestParseRejectsPOP: a POP account would import cleanly and then never sync,
// because this server speaks IMAP. Failing at import is the honest outcome.
func TestParseRejectsPOP(t *testing.T) {
	doc := `<plist version="1.0"><dict><key>PayloadContent</key><array><dict>
	  <key>PayloadType</key><string>com.apple.mail.managed</string>
	  <key>EmailAccountType</key><string>EmailTypePOP</string>
	  <key>IncomingMailServerHostName</key><string>pop.example.org</string>
	</dict></array></dict></plist>`

	_, err := Parse([]byte(doc))
	if err == nil {
		t.Fatal("expected POP to be rejected")
	}
	if !strings.Contains(err.Error(), "POP") {
		t.Errorf("error should name POP: %v", err)
	}
}

// TestParseReportsIgnoredPayloads: a profile that also configures WiFi should
// import its mail and say what it skipped, not silently drop half the file.
func TestParseReportsIgnoredPayloads(t *testing.T) {
	doc := `<plist version="1.0"><dict><key>PayloadContent</key><array>
	  <dict>
	    <key>PayloadType</key><string>com.apple.mail.managed</string>
	    <key>EmailAddress</key><string>user@example.org</string>
	    <key>IncomingMailServerHostName</key><string>mail.example.org</string>
	  </dict>
	  <dict><key>PayloadType</key><string>com.apple.wifi.managed</string></dict>
	  <dict><key>PayloadType</key><string>com.apple.vpn.managed</string></dict>
	</array></dict></plist>`

	p, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Mail == nil {
		t.Fatal("mail payload should have been imported")
	}
	if len(p.Ignored) != 2 {
		t.Fatalf("Ignored = %v, want the two unsupported payloads", p.Ignored)
	}
}

// TestParseNoUsablePayload — a WiFi-only profile parses fine and has nothing to
// import; the user needs to be told that specifically.
func TestParseNoUsablePayload(t *testing.T) {
	doc := `<plist version="1.0"><dict><key>PayloadContent</key><array>
	  <dict><key>PayloadType</key><string>com.apple.wifi.managed</string></dict>
	</array></dict></plist>`

	_, err := Parse([]byte(doc))
	if !errors.Is(err, ErrNoUsablePayload) {
		t.Fatalf("got %v, want ErrNoUsablePayload", err)
	}
}

// TestResolvedEmailFallbacks: the address is what every imported source is
// attributed to, so it is worth digging for before asking the user.
func TestResolvedEmailFallbacks(t *testing.T) {
	cases := []struct {
		name   string
		parsed Parsed
		want   string
	}{
		{
			name:   "explicit address",
			parsed: Parsed{Mail: &MailAccount{EmailAddress: "a@example.org", IMAPUsername: "other@example.org"}},
			want:   "a@example.org",
		},
		{
			name:   "address-shaped IMAP username",
			parsed: Parsed{Mail: &MailAccount{IMAPUsername: "b@example.org"}},
			want:   "b@example.org",
		},
		{
			name:   "from the DAV username",
			parsed: Parsed{CalDAV: &DAVAccount{Username: "c@example.org"}},
			want:   "c@example.org",
		},
		{
			name:   "from the profile display name",
			parsed: Parsed{DisplayName: "d@example.org"},
			want:   "d@example.org",
		},
		{
			name:   "nothing to go on",
			parsed: Parsed{Mail: &MailAccount{IMAPUsername: "plainlogin"}},
			want:   "",
		},
	}

	for _, c := range cases {
		if got := c.parsed.ResolvedEmail(); got != c.want {
			t.Errorf("%s: ResolvedEmail() = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestSuggestedNamePrefersHumanName — a label that just repeats the address
// tells the user nothing they cannot already see.
func TestSuggestedNamePrefersHumanName(t *testing.T) {
	p := Parsed{
		Mail:        &MailAccount{AccountName: "Денис Данилин"},
		DisplayName: "user@example.org",
	}
	if got := p.SuggestedName("fallback"); got != "Денис Данилин" {
		t.Errorf("SuggestedName() = %q", got)
	}

	onlyAddresses := Parsed{DisplayName: "user@example.org"}
	if got := onlyAddresses.SuggestedName("fallback"); got != "fallback" {
		t.Errorf("SuggestedName() = %q, want the fallback rather than the address", got)
	}
}

// TestParseDefaultPortsWhenAbsent: profiles routinely omit ports.
func TestParseDefaultPortsWhenAbsent(t *testing.T) {
	doc := `<plist version="1.0"><dict><key>PayloadContent</key><array><dict>
	  <key>PayloadType</key><string>com.apple.mail.managed</string>
	  <key>EmailAddress</key><string>user@example.org</string>
	  <key>IncomingMailServerHostName</key><string>mail.example.org</string>
	  <key>IncomingMailServerUseSSL</key><true/>
	  <key>OutgoingMailServerUseSSL</key><false/>
	</dict></array></dict></plist>`

	p, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Mail.IMAPPort != 993 {
		t.Errorf("IMAP port = %d, want 993 for TLS", p.Mail.IMAPPort)
	}
	if p.Mail.SMTPPort != 587 {
		t.Errorf("SMTP port = %d, want 587 for non-TLS", p.Mail.SMTPPort)
	}
	if p.Mail.SMTPHost != "mail.example.org" {
		t.Errorf("SMTP host = %q, want the incoming host", p.Mail.SMTPHost)
	}
}
