package mobileconfig

import (
	"fmt"
	"strings"
)

// Parsed is a profile reduced to what this server can act on. It is a
// description, not a change: nothing here has touched the database, so a
// caller can show it to the user, collect decisions, and only then apply it.
type Parsed struct {
	DisplayName  string
	Organization string
	Identifier   string

	Mail    *MailAccount
	CalDAV  *DAVAccount
	CardDAV *DAVAccount

	// Ignored lists payload types present in the file that this server has no
	// use for — VPN, WiFi, restrictions. Surfaced so the user is told what was
	// skipped rather than left assuming the whole file was applied.
	Ignored []string
}

// MailAccount is a com.apple.mail.managed payload.
type MailAccount struct {
	AccountDescription string
	AccountName        string
	EmailAddress       string

	IMAPHost     string
	IMAPPort     int
	IMAPUsername string
	IMAPPassword string
	IMAPTLS      bool

	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	SMTPTLS      bool
}

// DAVAccount is a com.apple.caldav.account or com.apple.carddav.account payload.
type DAVAccount struct {
	AccountDescription string
	Hostname           string
	Port               int
	PrincipalURL       string
	Username           string
	Password           string
	UseSSL             bool
}

// URL assembles the absolute URL a DAV client should start from. The profile
// gives a hostname and a path separately, and the path is a *principal* URL —
// for SOGo that is /SOGo/dav/<user>, which is not a collection. Discovery still
// has to run from here; this only produces the starting point.
func (d *DAVAccount) URL() string {
	scheme := "http"
	defaultPort := 80
	if d.UseSSL {
		scheme = "https"
		defaultPort = 443
	}

	host := d.Hostname
	if d.Port != 0 && d.Port != defaultPort {
		host = fmt.Sprintf("%s:%d", host, d.Port)
	}

	path := d.PrincipalURL
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	return scheme + "://" + host + path
}

// ErrNoUsablePayload is returned for a profile this server cannot act on — a
// WiFi-only or VPN-only profile, say. It parsed fine; there is simply nothing
// in it to import.
var ErrNoUsablePayload = fmt.Errorf("profile contains no mail, calendar or contact account")

// Parse reads a .mobileconfig and extracts the accounts it describes.
//
// POP payloads are rejected rather than quietly downgraded: this server syncs
// over IMAP, and an account configured as POP would import cleanly and then
// never fetch anything.
func Parse(data []byte) (*Parsed, error) {
	root, err := ParsePlist(data)
	if err != nil {
		return nil, err
	}
	if root.Kind != KindDict {
		return nil, fmt.Errorf("profile root is not a dictionary")
	}

	out := &Parsed{
		DisplayName:  root.StringOr("PayloadDisplayName", ""),
		Organization: root.StringOr("PayloadOrganization", ""),
		Identifier:   root.StringOr("PayloadIdentifier", ""),
	}

	payloads := root.Get("PayloadContent").Children()
	if len(payloads) == 0 {
		return nil, fmt.Errorf("profile has no PayloadContent")
	}

	for _, p := range payloads {
		switch p.StringOr("PayloadType", "") {
		case "com.apple.mail.managed":
			mail, err := parseMailPayload(p)
			if err != nil {
				return nil, err
			}
			// Only the first of each kind is taken. A profile carrying two mail
			// accounts is legal but describes two identities, and this import
			// resolves conflicts against one — importing both silently would
			// make "replace or rename?" ambiguous.
			if out.Mail == nil {
				out.Mail = mail
			} else {
				out.Ignored = append(out.Ignored, "com.apple.mail.managed (additional)")
			}

		case "com.apple.caldav.account":
			dav := parseDAVPayload(p, "CalDAV")
			if out.CalDAV == nil {
				out.CalDAV = dav
			} else {
				out.Ignored = append(out.Ignored, "com.apple.caldav.account (additional)")
			}

		case "com.apple.carddav.account":
			dav := parseDAVPayload(p, "CardDAV")
			if out.CardDAV == nil {
				out.CardDAV = dav
			} else {
				out.Ignored = append(out.Ignored, "com.apple.carddav.account (additional)")
			}

		case "":
			// A payload with no type is not something to guess at.
			out.Ignored = append(out.Ignored, "(payload with no PayloadType)")

		default:
			out.Ignored = append(out.Ignored, p.StringOr("PayloadType", ""))
		}
	}

	if out.Mail == nil && out.CalDAV == nil && out.CardDAV == nil {
		return nil, ErrNoUsablePayload
	}

	return out, nil
}

func parseMailPayload(p *Value) (*MailAccount, error) {
	accountType := p.StringOr("EmailAccountType", "")
	if strings.EqualFold(accountType, "EmailTypePOP") {
		return nil, fmt.Errorf("POP accounts are not supported; this server syncs over IMAP")
	}

	mail := &MailAccount{
		AccountDescription: p.StringOr("EmailAccountDescription", ""),
		AccountName:        p.StringOr("EmailAccountName", ""),
		EmailAddress:       strings.TrimSpace(p.StringOr("EmailAddress", "")),

		IMAPHost:     strings.TrimSpace(p.StringOr("IncomingMailServerHostName", "")),
		IMAPUsername: strings.TrimSpace(p.StringOr("IncomingMailServerUsername", "")),
		IMAPPassword: p.StringOr("IncomingPassword", ""),
		IMAPTLS:      p.BoolOr("IncomingMailServerUseSSL", true),

		SMTPHost:     strings.TrimSpace(p.StringOr("OutgoingMailServerHostName", "")),
		SMTPUsername: strings.TrimSpace(p.StringOr("OutgoingMailServerUsername", "")),
		SMTPPassword: p.StringOr("OutgoingPassword", ""),
		SMTPTLS:      p.BoolOr("OutgoingMailServerUseSSL", true),
	}

	mail.IMAPPort = p.IntOr("IncomingMailServerPortNumber", defaultPort(mail.IMAPTLS, 993, 143))
	mail.SMTPPort = p.IntOr("OutgoingMailServerPortNumber", defaultPort(mail.SMTPTLS, 465, 587))

	// The flag exists precisely so the profile need not repeat the secret.
	if p.BoolOr("OutgoingPasswordSameAsIncomingPassword", false) && mail.SMTPPassword == "" {
		mail.SMTPPassword = mail.IMAPPassword
	}
	// Same for the login: an outgoing username is often simply omitted.
	if mail.SMTPUsername == "" {
		mail.SMTPUsername = mail.IMAPUsername
	}
	// And a missing outgoing host means "same server", which is the usual
	// arrangement for anything that is not a hosted relay.
	if mail.SMTPHost == "" {
		mail.SMTPHost = mail.IMAPHost
	}

	if mail.IMAPHost == "" {
		return nil, fmt.Errorf("mail payload has no incoming server hostname")
	}

	return mail, nil
}

func parseDAVPayload(p *Value, prefix string) *DAVAccount {
	useSSL := p.BoolOr(prefix+"UseSSL", true)

	return &DAVAccount{
		AccountDescription: p.StringOr(prefix+"AccountDescription", ""),
		Hostname:           strings.TrimSpace(p.StringOr(prefix+"HostName", "")),
		Port:               p.IntOr(prefix+"Port", defaultPort(useSSL, 443, 80)),
		PrincipalURL:       strings.TrimSpace(p.StringOr(prefix+"PrincipalURL", "")),
		Username:           strings.TrimSpace(p.StringOr(prefix+"Username", "")),
		Password:           p.StringOr(prefix+"Password", ""),
		UseSSL:             useSSL,
	}
}

func defaultPort(secure bool, securePort, plainPort int) int {
	if secure {
		return securePort
	}
	return plainPort
}

// ResolvedEmail returns the address this profile is about, preferring the
// explicit EmailAddress and falling back to anything username-shaped that
// looks like an address.
//
// Returns "" when the profile carries no address at all — the caller has to ask
// the user, since every calendar and contact source on this server must belong
// to a concrete identity.
func (p *Parsed) ResolvedEmail() string {
	if p.Mail != nil {
		if p.Mail.EmailAddress != "" {
			return p.Mail.EmailAddress
		}
		if strings.Contains(p.Mail.IMAPUsername, "@") {
			return p.Mail.IMAPUsername
		}
	}

	for _, dav := range []*DAVAccount{p.CalDAV, p.CardDAV} {
		if dav == nil {
			continue
		}
		if strings.Contains(dav.Username, "@") {
			return dav.Username
		}
		if strings.Contains(dav.AccountDescription, "@") {
			return strings.TrimSpace(dav.AccountDescription)
		}
	}

	if strings.Contains(p.DisplayName, "@") {
		return strings.TrimSpace(p.DisplayName)
	}

	return ""
}

// SuggestedName is the label to give the imported account.
func (p *Parsed) SuggestedName(fallback string) string {
	for _, candidate := range []string{
		mailAccountName(p.Mail),
		p.DisplayName,
		p.Organization,
	} {
		candidate = strings.TrimSpace(candidate)
		// A name that is just the address adds nothing over the address itself.
		if candidate != "" && !strings.Contains(candidate, "@") {
			return candidate
		}
	}
	return fallback
}

func mailAccountName(m *MailAccount) string {
	if m == nil {
		return ""
	}
	return m.AccountName
}
