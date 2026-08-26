// Package mobileconfig reads and writes Apple configuration profiles
// (.mobileconfig) — the plist format iOS and macOS use to set up mail,
// calendar and contact accounts without the user typing hostnames.
package mobileconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Profile is the input to Generate: one identity, described the way a device
// needs to hear it.
type Profile struct {
	// DisplayName heads the profile in iOS Settings; Organization is the grey
	// subtitle under it.
	DisplayName  string
	Organization string

	// AccountName is the human name outgoing mail is sent under; EmailAddress
	// is the address itself.
	AccountName  string
	EmailAddress string

	// Username and Secret authenticate every payload. Secret should be an
	// application password, never the account password: the profile carries it
	// in the clear because the format has no other option, so it has to be a
	// credential that can be revoked on its own. Leave Secret empty to emit a
	// profile that prompts on the device instead.
	Username string
	Secret   string

	Hostname  string
	IMAPPort  int
	SMTPPort  int
	HTTPSPort int

	// CalDAVPath and CardDAVPath are the principal URLs, e.g. "/caldav/" —
	// leave either empty to omit that payload.
	CalDAVPath  string
	CardDAVPath string
}

// payloadIdentifier is the reverse-DNS namespace every payload hangs off.
// Stable across regenerations on purpose: iOS replaces a profile whose
// identifier it already holds, and mints a second one when it changes. A user
// who re-exports after rotating a password should end up with one profile, not
// a pile of them.
func (p *Profile) payloadIdentifier() string {
	host := reverseHost(p.Hostname)
	local := p.EmailAddress
	if at := strings.IndexByte(local, '@'); at != -1 {
		local = local[:at]
	}
	return fmt.Sprintf("%s.profile.%s", host, sanitizeIdentifier(local))
}

// stableUUID derives a payload UUID from the identifier. The format demands a
// UUID and iOS keys off it the same way it keys off the identifier, so it has
// to be deterministic for the same identity — a random one on each export
// would defeat the replacement behaviour payloadIdentifier is arranging.
func stableUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	h := hex.EncodeToString(sum[:16])
	return strings.ToUpper(fmt.Sprintf("%s-%s-%s-%s-%s",
		h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]))
}

// reverseHost turns "mail.letotam.ru" into "ru.letotam.mail".
func reverseHost(host string) string {
	parts := strings.Split(host, ".")
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return sanitizeIdentifier(strings.Join(parts, "."))
}

// sanitizeIdentifier keeps a reverse-DNS identifier to characters Apple
// tolerates: letters, digits, dot and hyphen.
func sanitizeIdentifier(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteByte('-')
		}
	}
	out := sb.String()
	if out == "" {
		return "profile"
	}
	return out
}

// Generate renders the profile as a .mobileconfig document.
//
// The result is unsigned. iOS shows an unsigned profile as "Not Verified" and
// installs it anyway; signing needs a certificate whose chain the device
// already trusts, which is a deployment decision rather than something this
// function can arrange.
func Generate(p *Profile) ([]byte, error) {
	if p.Hostname == "" {
		return nil, fmt.Errorf("hostname is required")
	}
	if p.EmailAddress == "" {
		return nil, fmt.Errorf("email address is required")
	}
	if p.Username == "" {
		return nil, fmt.Errorf("username is required")
	}

	root := p.payloadIdentifier()

	var payloads []string
	payloads = append(payloads, p.mailPayload(root))
	if p.CalDAVPath != "" {
		payloads = append(payloads, p.calDAVPayload(root))
	}
	if p.CardDAVPath != "" {
		payloads = append(payloads, p.cardDAVPayload(root))
	}

	description := "IMAP"
	if p.CalDAVPath != "" {
		description += ", CalDAV"
	}
	if p.CardDAVPath != "" {
		description += ", CardDAV"
	}
	if p.Secret != "" {
		description += " with application password"
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("  <dict>\n")
	b.WriteString("    <key>PayloadContent</key>\n    <array>\n")
	for _, payload := range payloads {
		b.WriteString(payload)
	}
	b.WriteString("    </array>\n")
	writeString(&b, 4, "PayloadDescription", description)
	writeString(&b, 4, "PayloadDisplayName", defaultString(p.DisplayName, p.EmailAddress))
	writeString(&b, 4, "PayloadIdentifier", root)
	writeString(&b, 4, "PayloadOrganization", p.Organization)
	b.WriteString("    <key>PayloadRemovalDisallowed</key>\n    <false/>\n")
	writeString(&b, 4, "PayloadType", "Configuration")
	writeString(&b, 4, "PayloadUUID", stableUUID(root))
	writeInt(&b, 4, "PayloadVersion", 1)
	b.WriteString("  </dict>\n")
	b.WriteString("</plist>\n")

	return []byte(b.String()), nil
}

func (p *Profile) mailPayload(root string) string {
	id := root + ".email"

	var b strings.Builder
	b.WriteString("      <dict>\n")
	writeString(&b, 8, "EmailAccountDescription", p.EmailAddress)
	writeString(&b, 8, "EmailAccountName", defaultString(p.AccountName, p.EmailAddress))
	writeString(&b, 8, "EmailAccountType", "EmailTypeIMAP")
	writeString(&b, 8, "EmailAddress", p.EmailAddress)

	writeString(&b, 8, "IncomingMailServerAuthentication", "EmailAuthPassword")
	writeString(&b, 8, "IncomingMailServerHostName", p.Hostname)
	writeInt(&b, 8, "IncomingMailServerPortNumber", p.IMAPPort)
	writeBool(&b, 8, "IncomingMailServerUseSSL", true)
	writeString(&b, 8, "IncomingMailServerUsername", p.Username)
	if p.Secret != "" {
		writeString(&b, 8, "IncomingPassword", p.Secret)
	}

	writeString(&b, 8, "OutgoingMailServerAuthentication", "EmailAuthPassword")
	writeString(&b, 8, "OutgoingMailServerHostName", p.Hostname)
	writeInt(&b, 8, "OutgoingMailServerPortNumber", p.SMTPPort)
	writeBool(&b, 8, "OutgoingMailServerUseSSL", true)
	writeString(&b, 8, "OutgoingMailServerUsername", p.Username)
	// Submission and retrieval are the same account here, so let the device
	// reuse the one credential rather than prompt twice for it.
	writeBool(&b, 8, "OutgoingPasswordSameAsIncomingPassword", true)

	writeString(&b, 8, "PayloadDescription", "Configures email account.")
	writeString(&b, 8, "PayloadDisplayName", fmt.Sprintf("IMAP (%s)", p.EmailAddress))
	writeString(&b, 8, "PayloadIdentifier", id)
	writeString(&b, 8, "PayloadType", "com.apple.mail.managed")
	writeString(&b, 8, "PayloadUUID", stableUUID(id))
	writeInt(&b, 8, "PayloadVersion", 1)
	b.WriteString("      </dict>\n")

	return b.String()
}

func (p *Profile) calDAVPayload(root string) string {
	id := root + ".caldav"

	var b strings.Builder
	b.WriteString("      <dict>\n")
	writeString(&b, 8, "CalDAVAccountDescription", p.EmailAddress)
	writeString(&b, 8, "CalDAVHostName", p.Hostname)
	writeInt(&b, 8, "CalDAVPort", p.HTTPSPort)
	writeString(&b, 8, "CalDAVPrincipalURL", p.CalDAVPath)
	writeBool(&b, 8, "CalDAVUseSSL", true)
	writeString(&b, 8, "CalDAVUsername", p.Username)
	if p.Secret != "" {
		writeString(&b, 8, "CalDAVPassword", p.Secret)
	}
	writeString(&b, 8, "PayloadDescription", "Configures CalDAV account.")
	writeString(&b, 8, "PayloadDisplayName", fmt.Sprintf("CalDAV (%s)", p.EmailAddress))
	writeString(&b, 8, "PayloadIdentifier", id)
	writeString(&b, 8, "PayloadType", "com.apple.caldav.account")
	writeString(&b, 8, "PayloadUUID", stableUUID(id))
	writeInt(&b, 8, "PayloadVersion", 1)
	b.WriteString("      </dict>\n")

	return b.String()
}

func (p *Profile) cardDAVPayload(root string) string {
	id := root + ".carddav"

	var b strings.Builder
	b.WriteString("      <dict>\n")
	writeString(&b, 8, "CardDAVAccountDescription", p.EmailAddress)
	writeString(&b, 8, "CardDAVHostName", p.Hostname)
	writeInt(&b, 8, "CardDAVPort", p.HTTPSPort)
	writeString(&b, 8, "CardDAVPrincipalURL", p.CardDAVPath)
	writeBool(&b, 8, "CardDAVUseSSL", true)
	writeString(&b, 8, "CardDAVUsername", p.Username)
	if p.Secret != "" {
		writeString(&b, 8, "CardDAVPassword", p.Secret)
	}
	writeString(&b, 8, "PayloadDescription", "Configures CardDAV account.")
	writeString(&b, 8, "PayloadDisplayName", fmt.Sprintf("CardDAV (%s)", p.EmailAddress))
	writeString(&b, 8, "PayloadIdentifier", id)
	writeString(&b, 8, "PayloadType", "com.apple.carddav.account")
	writeString(&b, 8, "PayloadUUID", stableUUID(id))
	writeInt(&b, 8, "PayloadVersion", 1)
	b.WriteString("      </dict>\n")

	return b.String()
}

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func writeString(b *strings.Builder, indent int, key, value string) {
	pad := strings.Repeat(" ", indent)
	fmt.Fprintf(b, "%s<key>%s</key>\n%s<string>%s</string>\n", pad, key, pad, escapeXML(value))
}

func writeInt(b *strings.Builder, indent int, key string, value int) {
	pad := strings.Repeat(" ", indent)
	fmt.Fprintf(b, "%s<key>%s</key>\n%s<integer>%d</integer>\n", pad, key, pad, value)
}

func writeBool(b *strings.Builder, indent int, key string, value bool) {
	pad := strings.Repeat(" ", indent)
	tag := "false"
	if value {
		tag = "true"
	}
	fmt.Fprintf(b, "%s<key>%s</key>\n%s<%s/>\n", pad, key, pad, tag)
}

// escapeXML escapes the five XML metacharacters. Display names carry arbitrary
// user text — an account called "Иванов & Co" must not produce a document the
// device refuses to parse.
func escapeXML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}
