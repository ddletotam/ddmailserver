package client

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/authfail"
	"github.com/ddletotam/ddmailserver/internal/logmask"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/tlsverify"
)

// oauthBearerAuth implements smtp.Auth for OAUTHBEARER (RFC 7628)
type oauthBearerAuth struct {
	username string
	token    string
	host     string
	port     int
}

// Start begins OAUTHBEARER authentication
func (a *oauthBearerAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	// OAUTHBEARER initial response format (RFC 7628):
	// n,a=<authzid>,^Ahost=<host>^Aport=<port>^Aauth=Bearer <token>^A^A
	// where ^A is ASCII 0x01
	response := fmt.Sprintf("n,a=%s,\x01host=%s\x01port=%d\x01auth=Bearer %s\x01\x01",
		a.username, a.host, a.port, a.token)
	return "OAUTHBEARER", []byte(response), nil
}

// Next handles server challenges (not expected for OAUTHBEARER)
func (a *oauthBearerAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		// Server sent an error response
		return nil, authfail.Mark(fmt.Errorf("OAUTHBEARER error: %s", string(fromServer)))
	}
	return nil, nil
}

// xoauth2Auth implements smtp.Auth for XOAUTH2 (Gmail-specific)
type xoauth2Auth struct {
	username string
	token    string
}

// Start begins XOAUTH2 authentication
func (a *xoauth2Auth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	// XOAUTH2 format: base64("user=" + email + "\x01auth=Bearer " + token + "\x01\x01")
	authString := fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", a.username, a.token)
	return "XOAUTH2", []byte(authString), nil
}

// Next handles server challenges
func (a *xoauth2Auth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		// Server sent an error response (base64 encoded JSON)
		decoded, err := base64.StdEncoding.DecodeString(string(fromServer))
		if err != nil {
			return nil, authfail.Mark(fmt.Errorf("XOAUTH2 error: %s", string(fromServer)))
		}
		return nil, authfail.Mark(fmt.Errorf("XOAUTH2 error: %s", string(decoded)))
	}
	return nil, nil
}

// Client wraps the SMTP client for external mail servers
type Client struct {
	account *models.Account
	// authPassed: the last Send got past SMTP AUTH.
	authPassed bool
	// timeouts bound the SMTP session (see DialTimeout).
	timeouts timeouts
}

// AuthPassed reports whether the last Send was accepted at the AUTH step,
// even if it failed later (a refused recipient proves the password works).
func (c *Client) AuthPassed() bool { return c.authPassed }

// New creates a new SMTP client for an account
func New(account *models.Account) *Client {
	return &Client{
		account:  account,
		timeouts: defaultTimeouts(),
	}
}

// envelopeAddress reduces one address to the bare addr-spec the SMTP envelope
// takes. The envelope is not a header: `MAIL FROM:<Имя <addr>>` is a syntax
// error, and receivers say so with 555 5.5.2.
func envelopeAddress(addr string) string {
	if extracted := extractEmailAddr(addr); extracted != "" {
		return extracted
	}
	return strings.TrimSpace(addr)
}

// envelopeAddresses is envelopeAddress over a list, dropping anything that
// reduces to nothing.
func envelopeAddresses(addrs []string) []string {
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if normalised := envelopeAddress(addr); normalised != "" {
			out = append(out, normalised)
		}
	}
	return out
}

// Send sends an email through the external SMTP server
func (c *Client) Send(from string, to []string, message []byte) error {
	c.authPassed = false
	// Normalised here rather than in the callers so no path out of this
	// function can put a display name on the wire: an identity configured as
	// "АппСек <user@example.org>" used to fail every send with 555 5.5.2 on
	// MAIL FROM.
	from = envelopeAddress(from)
	to = envelopeAddresses(to)

	addr := fmt.Sprintf("%s:%d", c.account.SMTPHost, c.account.SMTPPort)

	log.Printf("Sending email via SMTP %s", addr)

	// Create authentication based on auth type
	var auth smtp.Auth
	if c.account.IsOAuth() {
		// Use XOAUTH2 for Gmail (more widely supported than OAUTHBEARER)
		log.Printf("Using XOAUTH2 authentication for %s", c.account.SMTPUsername)
		auth = &xoauth2Auth{
			username: c.account.SMTPUsername,
			token:    c.account.OAuthAccessToken,
		}
	} else {
		// Use PLAIN auth for password-based authentication
		auth = smtp.PlainAuth(
			"",
			c.account.SMTPUsername,
			c.account.SMTPPassword,
			c.account.SMTPHost,
		)
	}

	var err error
	if c.account.SMTPTLS {
		// Use TLS (port 465 or 587 with STARTTLS)
		err = c.sendTLS(addr, auth, from, to, message)
	} else {
		// Plain SMTP (usually port 25, not recommended)
		err = c.sendPlain(addr, auth, from, to, message)
	}

	if err != nil {
		return fmt.Errorf("failed to send email: %w", err)
	}

	log.Printf("Email sent successfully from %s to %s", logmask.Addr(from), logmask.AddrSlice(to))
	return nil
}

// usesImplicitTLS reports whether the connection must be wrapped in TLS before
// the first byte, rather than upgraded with STARTTLS afterwards.
//
// The two are not interchangeable. On an implicit-TLS port the server waits for
// a ClientHello and says nothing; a plaintext dial there gets no greeting and
// hangs until the timeout. 465 is the assigned port for implicit TLS
// (RFC 8314) and is what device configuration profiles overwhelmingly specify,
// so this is not an exotic case — it is most of them.
func usesImplicitTLS(port int) bool {
	return port == 465
}

// Timeouts of one SMTP session, both directions (relay and direct MX).
//
// net/smtp has none of its own: smtp.Dial and smtp.SendMail dial without a
// timeout and never set a deadline, so a host that accepts the connection and
// then says nothing — or a SYN silently dropped and left to the kernel — held
// a worker for as long as the operating system cared to wait. Now the
// connection (TCP and, for implicit TLS, the handshake) must be up within
// DialTimeout, and the whole session — greeting to QUIT, DATA included —
// must finish within SessionTimeout. RFC 5321 §4.5.3.2 suggests 5 minutes per
// command and 10 for the end of DATA; one deadline for the session keeps the
// worst case bounded and is generous for messages within our 10 MB limit.
const (
	DialTimeout    = 30 * time.Second
	SessionTimeout = 10 * time.Minute
)

// timeouts are the limits of one SMTP session (shorter in tests).
type timeouts struct {
	dial    time.Duration
	session time.Duration
}

func defaultTimeouts() timeouts {
	return timeouts{dial: DialTimeout, session: SessionTimeout}
}

// dialSMTP connects to addr — over TLS from the first byte when implicitTLS
// is set — reads the greeting and returns the client. The session deadline is
// set on the connection before anything is read, so every later command is
// bounded by it too (a STARTTLS upgrade wraps the same connection).
func dialSMTP(addr, serverName string, implicitTLS *tls.Config, t timeouts) (*smtp.Client, error) {
	dialer := &net.Dialer{Timeout: t.dial}
	var conn net.Conn
	var err error
	if implicitTLS != nil {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, implicitTLS)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(t.session)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set session deadline: %w", err)
	}
	client, err := smtp.NewClient(conn, serverName)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("no SMTP greeting: %w", err)
	}
	return client, nil
}

// sendPlain is smtp.SendMail with timeouts: STARTTLS when the server offers
// it, then AUTH, then the transaction.
func (c *Client) sendPlain(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	client, err := dialSMTP(addr, c.account.SMTPHost, nil, c.timeouts)
	if err != nil {
		return fmt.Errorf("failed to dial: %w", err)
	}
	defer client.Close()

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(tlsverify.Config(c.account.SMTPHost)); err != nil {
			return fmt.Errorf("failed to start TLS: %w", err)
		}
	}
	if auth != nil {
		if ok, _ := client.Extension("AUTH"); !ok {
			return errors.New("smtp: server doesn't support AUTH")
		}
	}
	return c.transact(client, auth, from, to, msg)
}

// sendTLS sends email over TLS, choosing implicit TLS or STARTTLS by port.
func (c *Client) sendTLS(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	tlsConfig := tlsverify.Config(c.account.SMTPHost)

	if usesImplicitTLS(c.account.SMTPPort) {
		// Handshake first, then speak SMTP inside it.
		client, err := dialSMTP(addr, c.account.SMTPHost, tlsConfig, c.timeouts)
		if err != nil {
			return fmt.Errorf("failed to dial with implicit TLS: %w", err)
		}
		defer client.Close()
		return c.transact(client, auth, from, to, msg)
	}

	client, err := dialSMTP(addr, c.account.SMTPHost, nil, c.timeouts)
	if err != nil {
		return fmt.Errorf("failed to dial: %w", err)
	}
	defer client.Close()

	if err := client.StartTLS(tlsConfig); err != nil {
		return fmt.Errorf("failed to start TLS: %w", err)
	}
	return c.transact(client, auth, from, to, msg)
}

// transact authenticates and runs one mail transaction on a connected client.
func (c *Client) transact(client *smtp.Client, auth smtp.Auth, from string, to []string, msg []byte) error {
	var err error

	// Authenticate
	if auth != nil {
		if err = client.Auth(auth); err != nil {
			return fmt.Errorf("failed to authenticate: %w", err)
		}
		c.authPassed = true
	}

	// Set sender
	if err = client.Mail(from); err != nil {
		return fmt.Errorf("failed to set sender: %w", err)
	}

	// Set recipients
	for _, recipient := range to {
		if err = client.Rcpt(recipient); err != nil {
			// Masked: this error lands in logs and in outbox last_error, and
			// the recipient address is personal data — same as in SendDirect.
			return fmt.Errorf("failed to set recipient %s: %w", logmask.Addr(recipient), err)
		}
	}

	// Send message body
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("failed to get data writer: %w", err)
	}

	_, err = w.Write(msg)
	if err != nil {
		return fmt.Errorf("failed to write message: %w", err)
	}

	err = w.Close()
	if err != nil {
		return fmt.Errorf("failed to close data writer: %w", err)
	}

	// Quit
	return client.Quit()
}

// SendDirect sends email directly via MX lookup (for local domain senders).
// It returns the first failure; SendDirectEach says which recipient got what.
func SendDirect(from string, to []string, message []byte, hostname string) error {
	results := SendDirectEach(from, to, message, hostname)
	for _, rcpt := range envelopeAddresses(to) {
		if err := results[rcpt]; err != nil {
			return err
		}
	}
	return nil
}

// lookupMX is net.LookupMX, replaceable in tests.
var lookupMX = net.LookupMX

// SendDirectEach delivers to every recipient domain via its MX and returns the
// outcome per recipient addr-spec (nil = accepted). Every domain is attempted,
// even after one fails, so one unreachable domain does not hold back the rest
// and the caller can bounce exactly the recipients that failed.
func SendDirectEach(from string, to []string, message []byte, hostname string) map[string]error {
	return sendDirectEach(defaultTimeouts(), from, to, message, hostname)
}

// sendDirectEach is SendDirectEach with explicit session limits.
func sendDirectEach(t timeouts, from string, to []string, message []byte, hostname string) map[string]error {
	// As in Send: the envelope takes addr-specs only. Here a stray display name
	// also breaks the domain grouping below, since splitting "Имя <u@host>" on
	// "@" yields "host>" and the MX lookup goes looking for that.
	from = envelopeAddress(from)
	to = envelopeAddresses(to)

	results := make(map[string]error, len(to))

	// Group recipients by domain
	byDomain := make(map[string][]string)
	for _, rcpt := range to {
		parts := strings.SplitN(rcpt, "@", 2)
		if len(parts) != 2 || parts[1] == "" {
			results[rcpt] = fmt.Errorf("invalid recipient address %s", logmask.Addr(rcpt))
			continue
		}
		byDomain[parts[1]] = append(byDomain[parts[1]], rcpt)
	}

	for domain, rcpts := range byDomain {
		err := sendDirectDomain(t, domain, from, rcpts, message, hostname)
		for _, rcpt := range rcpts {
			results[rcpt] = err
		}
	}

	return results
}

// sendDirectDomain delivers to the recipients of one domain, trying its MX
// hosts in priority order.
func sendDirectDomain(t timeouts, domain, from string, rcpts []string, message []byte, hostname string) error {
	mxRecords, err := lookupMX(domain)
	if err != nil || len(mxRecords) == 0 {
		// Fallback to A record (RFC 5321 §5.1, implicit MX)
		mxRecords = []*net.MX{{Host: domain, Pref: 10}}
	}
	if len(mxRecords) == 1 && strings.TrimSuffix(mxRecords[0].Host, ".") == "" {
		return fmt.Errorf("failed to deliver to %s: %w", domain, errNullMX)
	}

	var lastErr error
	for _, mx := range mxRecords {
		host := strings.TrimSuffix(mx.Host, ".")
		if host == "" {
			continue
		}
		addr := fmt.Sprintf("%s:25", host)
		log.Printf("Direct delivery to %s via MX %s", domain, addr)

		lastErr = sendDirectToHost(t, addr, hostname, from, rcpts, message)
		if lastErr == nil {
			log.Printf("Direct delivery to %s successful via %s", domain, host)
			return nil
		}
		log.Printf("Direct delivery to %s via %s failed: %v", domain, host, lastErr)
		if IsPermanent(lastErr) {
			// A 5xx from an MX of the domain is the domain's answer; the
			// next MX would say the same.
			break
		}
	}
	return fmt.Errorf("failed to deliver to %s: %w", domain, lastErr)
}

// errStartTLS marks a failed STARTTLS handshake. The TCP session is dead
// after a handshake failure — "continuing" on the same connection just
// surfaces the TLS error on the next command. Opportunistic TLS therefore
// means: redial from scratch and skip STARTTLS entirely.
var errStartTLS = errors.New("starttls handshake failed")

func sendDirectToHost(t timeouts, addr, hostname, from string, to []string, msg []byte) error {
	err := sendDirectAttempt(t, addr, hostname, from, to, msg, true)
	if errors.Is(err, errStartTLS) {
		log.Printf("STARTTLS with %s failed, redialing without TLS", addr)
		err = sendDirectAttempt(t, addr, hostname, from, to, msg, false)
	}
	return err
}

func sendDirectAttempt(t timeouts, addr, hostname, from string, to []string, msg []byte, tryTLS bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bad MX address %q: %w", addr, err)
	}
	client, err := dialSMTP(addr, host, nil, t)
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}
	defer client.Close()

	if err := client.Hello(hostname); err != nil {
		return fmt.Errorf("EHLO failed: %w", err)
	}

	// Opportunistic STARTTLS; a failed handshake aborts this attempt (the
	// connection is unusable) and the caller retries in plaintext.
	if tryTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			// Verification here is opportunistic in effect: a failed handshake
			// makes the caller retry in plaintext. Completing the chain via AIA
			// therefore only ever upgrades an MX that would have fallen back —
			// it cannot make delivery less secure than it already was.
			tlsConfig := tlsverify.Config(host)
			if err := client.StartTLS(tlsConfig); err != nil {
				return fmt.Errorf("%w: %v", errStartTLS, err)
			}
		}
	}

	if err := client.Mail(from); err != nil {
		return fmt.Errorf("MAIL FROM failed: %w", err)
	}

	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			// Masked: this error lands in logs and outbox last_error, and
			// the recipient address is personal data.
			return fmt.Errorf("RCPT TO %s failed: %w", logmask.Addr(rcpt), err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA failed: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("write failed: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("data close failed: %w", err)
	}

	return client.Quit()
}
