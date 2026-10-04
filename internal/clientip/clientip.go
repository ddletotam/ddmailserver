// Package clientip determines the real client address of a connection or an
// HTTP request. Forwarding headers (X-Forwarded-For, X-Real-IP,
// X-Forwarded-Host) are honoured only when the direct peer is a configured
// trusted proxy; otherwise anyone could spoof them.
package clientip

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// DefaultTrustedProxies returns the ranges trusted when the configuration
// names none: loopback, since in production the web server sits behind
// nginx on the same host.
func DefaultTrustedProxies() []string {
	return []string{"127.0.0.0/8", "::1/128"}
}

// Resolver extracts client addresses, trusting forwarding headers only from
// the configured proxies.
type Resolver struct {
	trusted []netip.Prefix
}

// New builds a Resolver from a list of IPs and/or CIDRs. A nil list means
// "use the loopback default"; an empty non-nil list trusts no proxy at all.
func New(trustedProxies []string) (*Resolver, error) {
	if trustedProxies == nil {
		trustedProxies = DefaultTrustedProxies()
	}
	r := &Resolver{}
	for _, s := range trustedProxies {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		var p netip.Prefix
		if strings.Contains(s, "/") {
			pp, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("invalid trusted proxy %q: %w", s, err)
			}
			p = pp.Masked()
		} else {
			a, err := netip.ParseAddr(s)
			if err != nil {
				return nil, fmt.Errorf("invalid trusted proxy %q: %w", s, err)
			}
			a = a.Unmap().WithZone("")
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		r.trusted = append(r.trusted, p)
	}
	return r, nil
}

// Default returns a Resolver trusting DefaultTrustedProxies.
func Default() *Resolver {
	return &Resolver{trusted: []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("::1/128"),
	}}
}

// IsTrusted reports whether addr belongs to a trusted proxy.
func (r *Resolver) IsTrusted(addr netip.Addr) bool {
	if r == nil || !addr.IsValid() {
		return false
	}
	addr = addr.Unmap().WithZone("")
	for _, p := range r.trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// parseAddr parses a bare IP or an "ip:port" / "[ip]:port" pair.
func parseAddr(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, false
	}
	if a, err := netip.ParseAddr(strings.Trim(s, "[]")); err == nil {
		return a.Unmap().WithZone(""), true
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		if a, err := netip.ParseAddr(host); err == nil {
			return a.Unmap().WithZone(""), true
		}
	}
	return netip.Addr{}, false
}

// FromAddr returns the IP of a network address such as net.Conn.RemoteAddr()
// or http.Request.RemoteAddr, without the port. IPv6 addresses keep all
// their colons. An unparsable address is returned as-is so it still forms
// its own bucket rather than collapsing with others.
func FromAddr(addr string) string {
	if a, ok := parseAddr(addr); ok {
		return a.String()
	}
	return strings.TrimSpace(addr)
}

// FromNetAddr is FromAddr for a net.Addr; nil yields "".
func FromNetAddr(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	if ta, ok := addr.(*net.TCPAddr); ok {
		if a, ok := netip.AddrFromSlice(ta.IP); ok {
			return a.Unmap().String()
		}
	}
	return FromAddr(addr.String())
}

// FromRequest returns the client IP of an HTTP request. X-Forwarded-For is
// walked right to left, skipping trusted proxies; the first untrusted hop is
// the client. Without X-Forwarded-For, X-Real-IP is used. Both only count
// when the direct peer is trusted.
func (r *Resolver) FromRequest(req *http.Request) string {
	peer, ok := parseAddr(req.RemoteAddr)
	if !ok {
		return FromAddr(req.RemoteAddr)
	}
	if !r.IsTrusted(peer) {
		return peer.String()
	}

	var hops []string
	for _, v := range req.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	if len(hops) > 0 {
		client := peer
		for i := len(hops) - 1; i >= 0; i-- {
			a, ok := parseAddr(hops[i])
			if !ok {
				// Everything left of a malformed hop is unverifiable.
				break
			}
			client = a
			if !r.IsTrusted(a) {
				break
			}
		}
		return client.String()
	}

	if a, ok := parseAddr(req.Header.Get("X-Real-IP")); ok {
		return a.String()
	}
	return peer.String()
}

// RequestHost returns the host the client addressed: X-Forwarded-Host when
// the peer is a trusted proxy and set the header, otherwise req.Host.
func (r *Resolver) RequestHost(req *http.Request) string {
	if peer, ok := parseAddr(req.RemoteAddr); ok && r.IsTrusted(peer) {
		if h := req.Header.Get("X-Forwarded-Host"); h != "" {
			// A proxy chain may list several; the first is what the client sent.
			return strings.TrimSpace(strings.Split(h, ",")[0])
		}
	}
	return req.Host
}
