package parser

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SPFResolver is the subset of DNS lookups SPF evaluation needs.
// *net.Resolver satisfies it; tests substitute a fake.
type SPFResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// SPFChecker performs SPF (Sender Policy Framework) verification per RFC 7208.
type SPFChecker struct {
	resolver SPFResolver
	timeout  time.Duration
	cache    map[string]spfCacheEntry // key: "ip|domain"
	cacheMu  sync.RWMutex
}

type spfCacheEntry struct {
	result  AuthResult
	detail  string
	expires time.Time
}

const (
	spfCacheTTL = 10 * time.Minute
	// spfCacheMax bounds the cache so a stream of distinct senders cannot
	// grow it without limit.
	spfCacheMax = 10000
	// spfMaxDNSLookups is the RFC 7208 §4.6.4 limit on terms that cause DNS
	// queries (include, a, mx, ptr, exists, redirect).
	spfMaxDNSLookups = 10
	// spfMaxVoidLookups is the RFC 7208 §4.6.4 limit on lookups returning
	// no records / NXDOMAIN.
	spfMaxVoidLookups = 2
	// spfMaxNames limits MX hosts and PTR names examined per term.
	spfMaxNames = 10
	// spfMaxDepth is a belt-and-braces bound on include/redirect nesting:
	// the top-level record plus at most spfMaxDNSLookups nested ones.
	spfMaxDepth = spfMaxDNSLookups + 1
	// spfDefaultTimeout bounds one whole SPF evaluation.
	spfDefaultTimeout = 20 * time.Second
)

// NewSPFChecker creates a new SPF checker using the system resolver.
func NewSPFChecker() *SPFChecker {
	return NewSPFCheckerWithResolver(net.DefaultResolver)
}

// NewSPFCheckerWithResolver creates an SPF checker with a custom resolver.
func NewSPFCheckerWithResolver(r SPFResolver) *SPFChecker {
	return &SPFChecker{
		resolver: r,
		timeout:  spfDefaultTimeout,
		cache:    make(map[string]spfCacheEntry),
	}
}

// spfError carries a permerror/temperror out of the evaluation.
type spfError struct {
	result AuthResult
	msg    string
}

func (e *spfError) Error() string { return string(e.result) + ": " + e.msg }

func permErr(format string, a ...any) error {
	return &spfError{result: AuthResultPermError, msg: fmt.Sprintf(format, a...)}
}

func tempErr(format string, a ...any) error {
	return &spfError{result: AuthResultTempError, msg: fmt.Sprintf(format, a...)}
}

// spfEval is the state shared across one check_host() evaluation, including
// all nested include/redirect evaluations.
type spfEval struct {
	ctx     context.Context
	ip      net.IP
	lookups int
	voids   int
	path    map[string]bool // domains on the current include/redirect chain
	depth   int
}

func (ev *spfEval) countLookup(term string) error {
	ev.lookups++
	if ev.lookups > spfMaxDNSLookups {
		return permErr("too many DNS lookups (limit %d) at %q", spfMaxDNSLookups, term)
	}
	return nil
}

func (ev *spfEval) countVoid(name string) error {
	ev.voids++
	if ev.voids > spfMaxVoidLookups {
		return permErr("too many void DNS lookups (limit %d) at %q", spfMaxVoidLookups, name)
	}
	return nil
}

// CheckSPF verifies if the sender IP is authorized to send mail for the domain.
// Returns AuthResult: pass, fail, softfail, neutral, none, permerror, temperror.
func (c *SPFChecker) CheckSPF(senderIP, fromDomain string) (AuthResult, string) {
	if senderIP == "" || fromDomain == "" {
		return AuthResultNone, "missing sender IP or domain"
	}
	domain := normalizeSPFDomain(fromDomain)

	cacheKey := senderIP + "|" + domain
	c.cacheMu.RLock()
	if e, ok := c.cache[cacheKey]; ok && time.Now().Before(e.expires) {
		c.cacheMu.RUnlock()
		return e.result, e.detail
	}
	c.cacheMu.RUnlock()

	ip := net.ParseIP(senderIP)
	if ip == nil {
		return AuthResultNone, "invalid sender IP"
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	ev := &spfEval{ctx: ctx, ip: ip, path: make(map[string]bool)}
	result, detail := c.checkHost(ev, domain)

	// Transient DNS failures are not cached: the next message retries.
	if result != AuthResultTempError {
		c.cacheStore(cacheKey, result, detail)
	}
	return result, detail
}

func (c *SPFChecker) cacheStore(key string, result AuthResult, detail string) {
	now := time.Now()
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if len(c.cache) >= spfCacheMax {
		for k, e := range c.cache {
			if now.After(e.expires) {
				delete(c.cache, k)
			}
		}
		if len(c.cache) >= spfCacheMax {
			c.cache = make(map[string]spfCacheEntry)
		}
	}
	c.cache[key] = spfCacheEntry{result: result, detail: detail, expires: now.Add(spfCacheTTL)}
}

// checkHost implements RFC 7208 check_host() and converts errors to results.
func (c *SPFChecker) checkHost(ev *spfEval, domain string) (AuthResult, string) {
	result, detail, err := c.evalDomain(ev, domain)
	if err != nil {
		var se *spfError
		if errors.As(err, &se) {
			return se.result, se.msg
		}
		return AuthResultTempError, err.Error()
	}
	return result, detail
}

// evalDomain fetches the SPF record of domain and evaluates it.
func (c *SPFChecker) evalDomain(ev *spfEval, domain string) (AuthResult, string, error) {
	if domain == "" || len(domain) > 253 || strings.ContainsAny(domain, " /%") {
		return "", "", permErr("invalid domain %q", domain)
	}
	if ev.depth >= spfMaxDepth {
		return "", "", permErr("include/redirect nesting too deep at %q", domain)
	}
	if ev.path[domain] {
		return "", "", permErr("include/redirect loop at %q", domain)
	}
	ev.path[domain] = true
	ev.depth++
	defer func() {
		delete(ev.path, domain)
		ev.depth--
	}()

	record, err := c.fetchRecord(ev, domain)
	if err != nil {
		return "", "", err
	}
	if record == "" {
		return AuthResultNone, "no SPF record found for " + domain, nil
	}
	return c.evaluateRecord(ev, record, domain)
}

// fetchRecord returns the single v=spf1 record of domain, "" if none.
func (c *SPFChecker) fetchRecord(ev *spfEval, domain string) (string, error) {
	records, err := c.resolver.LookupTXT(ev.ctx, domain)
	if err != nil {
		if isDNSNotFound(err) {
			return "", nil
		}
		return "", tempErr("DNS TXT lookup for %s failed: %v", domain, err)
	}
	var found []string
	for _, r := range records {
		lr := strings.ToLower(r)
		if lr == "v=spf1" || strings.HasPrefix(lr, "v=spf1 ") {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	default:
		return "", permErr("multiple SPF records for %s", domain)
	}
}

// evaluateRecord evaluates the terms of an SPF record left to right;
// redirect= is applied only when no mechanism matched (RFC 7208 §6.1).
func (c *SPFChecker) evaluateRecord(ev *spfEval, record, domain string) (AuthResult, string, error) {
	terms := strings.Fields(record)[1:] // skip "v=spf1"

	var redirect string
	haveRedirect := false
	// Modifiers are collected first: their position does not matter.
	for _, term := range terms {
		name, value, isMod := splitModifier(term)
		if !isMod {
			continue
		}
		if strings.EqualFold(name, "redirect") {
			if haveRedirect {
				return "", "", permErr("duplicate redirect modifier in %s", domain)
			}
			haveRedirect = true
			redirect = value
		}
		// exp= and unknown modifiers are ignored (RFC 7208 §6).
	}

	for _, term := range terms {
		if _, _, isMod := splitModifier(term); isMod {
			continue
		}
		qualifier := byte('+')
		mechanism := term
		if term[0] == '+' || term[0] == '-' || term[0] == '~' || term[0] == '?' {
			qualifier = term[0]
			mechanism = term[1:]
		}

		match, err := c.checkMechanism(ev, mechanism, domain)
		if err != nil {
			return "", "", err
		}
		if !match {
			continue
		}
		detail := "matched: " + term
		switch qualifier {
		case '-':
			return AuthResultFail, detail, nil
		case '~':
			return AuthResultSoftfail, detail, nil
		case '?':
			return AuthResultNeutral, detail, nil
		default:
			return AuthResultPass, detail, nil
		}
	}

	if haveRedirect {
		if err := ev.countLookup("redirect=" + redirect); err != nil {
			return "", "", err
		}
		target := normalizeSPFDomain(redirect)
		if strings.Contains(target, "%") {
			return "", "", permErr("SPF macros are not supported in redirect=%s", redirect)
		}
		result, detail, err := c.evalDomain(ev, target)
		if err != nil {
			return "", "", err
		}
		if result == AuthResultNone {
			return "", "", permErr("redirect target %s has no SPF record", target)
		}
		return result, detail, nil
	}

	return AuthResultNeutral, "no mechanism matched", nil
}

// splitModifier reports whether term is a modifier (name=value).
func splitModifier(term string) (name, value string, ok bool) {
	eq := strings.IndexByte(term, '=')
	if eq <= 0 {
		return "", "", false
	}
	// A '=' inside a mechanism argument (after ':' or '/') is not a modifier.
	if i := strings.IndexAny(term, ":/"); i >= 0 && i < eq {
		return "", "", false
	}
	return term[:eq], term[eq+1:], true
}

// checkMechanism evaluates a single SPF mechanism.
func (c *SPFChecker) checkMechanism(ev *spfEval, mechanism, domain string) (bool, error) {
	name, arg := mechanism, ""
	if i := strings.IndexAny(mechanism, ":/"); i >= 0 {
		name, arg = mechanism[:i], mechanism[i:]
	}
	name = strings.ToLower(name)

	switch name {
	case "all":
		if arg != "" {
			return false, permErr("invalid mechanism %q", mechanism)
		}
		return true, nil

	case "ip4", "ip6":
		if !strings.HasPrefix(arg, ":") {
			return false, permErr("invalid mechanism %q", mechanism)
		}
		return matchIPNet(arg[1:], name == "ip6", ev.ip)

	case "include":
		if err := ev.countLookup(mechanism); err != nil {
			return false, err
		}
		if !strings.HasPrefix(arg, ":") || len(arg) < 2 {
			return false, permErr("include without domain")
		}
		target := normalizeSPFDomain(arg[1:])
		if strings.Contains(target, "%") {
			return false, nil // macros unsupported: treat as non-matching
		}
		result, _, err := c.evalDomain(ev, target)
		if err != nil {
			return false, err
		}
		switch result {
		case AuthResultPass:
			return true, nil
		case AuthResultNone:
			return false, permErr("include target %s has no SPF record", target)
		default:
			return false, nil
		}

	case "a", "mx":
		if err := ev.countLookup(mechanism); err != nil {
			return false, err
		}
		target, cidr4, cidr6, err := parseDomainCIDR(arg, domain)
		if err != nil {
			return false, err
		}
		if strings.Contains(target, "%") {
			return false, nil
		}
		if name == "a" {
			return c.matchHostIP(ev, target, cidr4, cidr6)
		}
		return c.matchMX(ev, target, cidr4, cidr6)

	case "ptr":
		if err := ev.countLookup(mechanism); err != nil {
			return false, err
		}
		target := domain
		if strings.HasPrefix(arg, ":") {
			target = normalizeSPFDomain(arg[1:])
		}
		if strings.Contains(target, "%") {
			return false, nil
		}
		return c.matchPTR(ev, target)

	case "exists":
		if err := ev.countLookup(mechanism); err != nil {
			return false, err
		}
		if !strings.HasPrefix(arg, ":") || len(arg) < 2 {
			return false, permErr("exists without domain")
		}
		target := normalizeSPFDomain(arg[1:])
		if strings.Contains(target, "%") {
			return false, nil
		}
		addrs, err := c.lookupIP(ev, "ip4", target)
		if err != nil {
			return false, err
		}
		return len(addrs) > 0, nil
	}

	return false, permErr("unknown mechanism %q", mechanism)
}

// lookupIP resolves host, counting an empty answer as a void lookup.
func (c *SPFChecker) lookupIP(ev *spfEval, network, host string) ([]net.IP, error) {
	addrs, err := c.resolver.LookupIP(ev.ctx, network, host)
	if err != nil && !isDNSNotFound(err) {
		return nil, tempErr("DNS lookup for %s failed: %v", host, err)
	}
	if len(addrs) == 0 {
		if verr := ev.countVoid(host); verr != nil {
			return nil, verr
		}
		return nil, nil
	}
	return addrs, nil
}

func ipNetwork(ip net.IP) string {
	if ip.To4() != nil {
		return "ip4"
	}
	return "ip6"
}

func (c *SPFChecker) matchHostIP(ev *spfEval, host string, cidr4, cidr6 int) (bool, error) {
	addrs, err := c.lookupIP(ev, ipNetwork(ev.ip), host)
	if err != nil {
		return false, err
	}
	for _, a := range addrs {
		if ipInPrefix(a, ev.ip, cidr4, cidr6) {
			return true, nil
		}
	}
	return false, nil
}

func (c *SPFChecker) matchMX(ev *spfEval, domain string, cidr4, cidr6 int) (bool, error) {
	mxs, err := c.resolver.LookupMX(ev.ctx, domain)
	if err != nil && !isDNSNotFound(err) {
		return false, tempErr("DNS MX lookup for %s failed: %v", domain, err)
	}
	if len(mxs) == 0 {
		return false, ev.countVoid(domain)
	}
	if len(mxs) > spfMaxNames {
		return false, permErr("too many MX records for %s", domain)
	}
	for _, mx := range mxs {
		host := normalizeSPFDomain(mx.Host)
		addrs, err := c.resolver.LookupIP(ev.ctx, ipNetwork(ev.ip), host)
		if err != nil && !isDNSNotFound(err) {
			return false, tempErr("DNS lookup for %s failed: %v", host, err)
		}
		for _, a := range addrs {
			if ipInPrefix(a, ev.ip, cidr4, cidr6) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (c *SPFChecker) matchPTR(ev *spfEval, target string) (bool, error) {
	names, err := c.resolver.LookupAddr(ev.ctx, ev.ip.String())
	if err != nil || len(names) == 0 {
		// PTR failures are never errors, just non-matches (RFC 7208 §5.5).
		return false, nil
	}
	if len(names) > spfMaxNames {
		names = names[:spfMaxNames]
	}
	for _, n := range names {
		n = normalizeSPFDomain(n)
		if n != target && !strings.HasSuffix(n, "."+target) {
			continue
		}
		addrs, err := c.resolver.LookupIP(ev.ctx, ipNetwork(ev.ip), n)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if a.Equal(ev.ip) {
				return true, nil
			}
		}
	}
	return false, nil
}

// parseDomainCIDR parses the argument of a/mx: [":" domain] ["/" cidr4] ["//" cidr6].
func parseDomainCIDR(arg, defDomain string) (string, int, int, error) {
	cidr4, cidr6 := 32, 128
	target := defDomain
	if strings.HasPrefix(arg, ":") {
		rest := arg[1:]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			target, arg = rest[:i], rest[i:]
		} else {
			target, arg = rest, ""
		}
		target = normalizeSPFDomain(target)
		if target == "" {
			return "", 0, 0, permErr("empty domain in mechanism")
		}
	}
	if arg == "" {
		return target, cidr4, cidr6, nil
	}
	v4part, v6part := arg, ""
	if i := strings.Index(arg, "//"); i >= 0 {
		v4part, v6part = arg[:i], arg[i+2:]
	}
	if v4part != "" {
		if !strings.HasPrefix(v4part, "/") {
			return "", 0, 0, permErr("invalid cidr %q", arg)
		}
		n, err := strconv.Atoi(v4part[1:])
		if err != nil || n < 0 || n > 32 {
			return "", 0, 0, permErr("invalid ip4 cidr %q", arg)
		}
		cidr4 = n
	}
	if v6part != "" {
		n, err := strconv.Atoi(v6part)
		if err != nil || n < 0 || n > 128 {
			return "", 0, 0, permErr("invalid ip6 cidr %q", arg)
		}
		cidr6 = n
	}
	return target, cidr4, cidr6, nil
}

// ipInPrefix reports whether ip is within the cidr4/cidr6 prefix of addr.
func ipInPrefix(addr, ip net.IP, cidr4, cidr6 int) bool {
	if a4, i4 := addr.To4(), ip.To4(); a4 != nil || i4 != nil {
		if a4 == nil || i4 == nil {
			return false
		}
		mask := net.CIDRMask(cidr4, 32)
		return a4.Mask(mask).Equal(i4.Mask(mask))
	}
	mask := net.CIDRMask(cidr6, 128)
	return addr.Mask(mask).Equal(ip.Mask(mask))
}

// matchIPNet matches ip against an ip4:/ip6: argument (address or CIDR).
func matchIPNet(spec string, v6 bool, ip net.IP) (bool, error) {
	bits := 32
	if v6 {
		bits = 128
	}
	addrStr, prefix := spec, bits
	if i := strings.IndexByte(spec, '/'); i >= 0 {
		addrStr = spec[:i]
		n, err := strconv.Atoi(spec[i+1:])
		if err != nil || n < 0 || n > bits {
			return false, permErr("invalid cidr in %q", spec)
		}
		prefix = n
	}
	addr := net.ParseIP(addrStr)
	if addr == nil || (addr.To4() != nil) == v6 {
		return false, permErr("invalid address in %q", spec)
	}
	if v6 {
		if ip.To4() != nil {
			return false, nil
		}
		return ipInPrefix(addr, ip, 32, prefix), nil
	}
	if ip.To4() == nil {
		return false, nil
	}
	return ipInPrefix(addr, ip, prefix, 128), nil
}

func normalizeSPFDomain(d string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
}

func isDNSNotFound(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && de.IsNotFound
}
