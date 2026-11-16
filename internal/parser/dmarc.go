package parser

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"
)

// DMARC (RFC 7489): the domain in the visible From must be backed by an
// aligned SPF pass or an aligned DKIM signature. Without this check a message
// "from" a bank or from @mail.ru sent by any server passed as long as it
// carried its own valid DKIM signature or the envelope domain's SPF passed —
// SPF and DKIM were only checked separately (found 2026-11-16).
//
// The result feeds the spam score (category "dmarc"), it never rejects at
// SMTP: a false positive (a mailing list that breaks the signature) lands in
// the hidden Spam folder, where "Not spam" recovers it, instead of bouncing.

// DMARCOutcome is the evaluation of one message.
type DMARCOutcome struct {
	Result AuthResult   // pass, fail, none (no policy), temperror
	Policy dmarc.Policy // the policy that applies (sp for a subdomain), "" without a record
	Domain string       // the From domain
	Detail string
}

// DMARCChecker looks up DMARC records and evaluates alignment.
type DMARCChecker struct {
	resolver SPFResolver
	timeout  time.Duration

	mu    sync.Mutex
	cache map[string]dmarcCacheEntry // key: domain the record was looked up for
}

type dmarcCacheEntry struct {
	record  *dmarc.Record // nil: no policy
	err     error         // temporary failure (not cached for long)
	expires time.Time
}

// NewDMARCChecker uses the system resolver.
func NewDMARCChecker() *DMARCChecker {
	return NewDMARCCheckerWithResolver(net.DefaultResolver)
}

// NewDMARCCheckerWithResolver uses a custom resolver (tests).
func NewDMARCCheckerWithResolver(r SPFResolver) *DMARCChecker {
	return &DMARCChecker{resolver: r, timeout: 5 * time.Second, cache: map[string]dmarcCacheEntry{}}
}

// OrgDomain is the organizational domain (RFC 7489 §3.2): the registrable
// domain by the public suffix list. Unknown suffixes fall back to the name.
func OrgDomain(domain string) string {
	domain = strings.TrimSuffix(strings.ToLower(domain), ".")
	if org, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil {
		return org
	}
	return domain
}

func aligned(mode dmarc.AlignmentMode, authDomain, fromDomain string) bool {
	authDomain = strings.TrimSuffix(strings.ToLower(authDomain), ".")
	if authDomain == "" {
		return false
	}
	if mode == dmarc.AlignmentStrict {
		return authDomain == fromDomain
	}
	return OrgDomain(authDomain) == OrgDomain(fromDomain)
}

// Evaluate checks the From domain against its DMARC policy.
//   - spfResult/spfDomain: the SPF verdict and the domain it was checked for
//     (envelope MAIL FROM, or the From domain when the envelope was empty);
//   - dkimPass: the d= domains of signatures that verified.
func (c *DMARCChecker) Evaluate(fromDomain string, spfResult AuthResult, spfDomain string, dkimPass []string) DMARCOutcome {
	fromDomain = strings.TrimSuffix(strings.ToLower(fromDomain), ".")
	out := DMARCOutcome{Domain: fromDomain}
	if fromDomain == "" {
		out.Result, out.Detail = AuthResultNone, "no From domain"
		return out
	}

	rec, sub, err := c.policyFor(fromDomain)
	if err != nil {
		out.Result, out.Detail = AuthResultTempError, err.Error()
		return out
	}
	if rec == nil {
		out.Result, out.Detail = AuthResultNone, "no DMARC policy for "+fromDomain
		return out
	}
	out.Policy = rec.Policy
	if sub && rec.SubdomainPolicy != "" {
		out.Policy = rec.SubdomainPolicy
	}

	for _, d := range dkimPass {
		if aligned(rec.DKIMAlignment, d, fromDomain) {
			out.Result, out.Detail = AuthResultPass, "aligned DKIM d="+d
			return out
		}
	}
	if spfResult == AuthResultPass && aligned(rec.SPFAlignment, spfDomain, fromDomain) {
		out.Result, out.Detail = AuthResultPass, "aligned SPF "+spfDomain
		return out
	}

	out.Result = AuthResultFail
	out.Detail = "no aligned SPF or DKIM for " + fromDomain + " (p=" + string(out.Policy) + ")"
	return out
}

// policyFor finds the record: the From domain first, then its
// organizational domain (sub=true — the subdomain policy applies).
func (c *DMARCChecker) policyFor(domain string) (rec *dmarc.Record, sub bool, err error) {
	rec, err = c.lookup(domain)
	if err != nil || rec != nil {
		return rec, false, err
	}
	if org := OrgDomain(domain); org != domain {
		rec, err = c.lookup(org)
		return rec, rec != nil, err
	}
	return nil, false, nil
}

func (c *DMARCChecker) lookup(domain string) (*dmarc.Record, error) {
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.cache[domain]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.record, e.err
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	rec, err := dmarc.LookupWithOptions(domain, &dmarc.LookupOptions{
		LookupTXT: func(name string) ([]string, error) { return c.resolver.LookupTXT(ctx, name) },
	})

	entry := dmarcCacheEntry{record: rec, expires: now.Add(time.Hour)}
	switch {
	case err == nil:
	case errors.Is(err, dmarc.ErrNoPolicy):
		entry.record = nil
	case isNotFound(err):
		entry.record = nil
	default:
		// DNS trouble or a broken record: temporary, retry soon.
		entry.record, entry.err = nil, errors.New("DMARC lookup "+domain+": "+err.Error())
		entry.expires = now.Add(time.Minute)
	}
	c.mu.Lock()
	c.cache[domain] = entry
	c.mu.Unlock()
	return entry.record, entry.err
}

// isNotFound: NXDOMAIN / no TXT records — the domain has no policy.
// dmarc.LookupWithOptions only maps *net.DNSError itself; a wrapped one
// (custom resolver) reaches here as a generic error.
func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound ||
		strings.Contains(err.Error(), "no such host")
}
