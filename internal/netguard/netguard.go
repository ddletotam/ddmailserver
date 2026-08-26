// Package netguard holds the checks that decide whether an outbound request
// to a host we did not choose is safe to make.
//
// It exists so there is exactly one answer to "is this address on the public
// Internet". The rule lived inside the avatar fetcher first; the moment a
// second caller needed it — AIA certificate fetching, whose URL comes out of a
// certificate an attacker may have supplied — copying it would have created two
// definitions of "private" that drift apart, and the weaker one becomes the
// hole.
package netguard

import (
	"context"
	"net"
	"time"
)

// DefaultResolveTimeout bounds the DNS lookup. Callers with their own deadline
// can pass a context that expires sooner.
const DefaultResolveTimeout = 5 * time.Second

// HostIsPublic resolves the hostname and reports whether every answer is a
// routable public address.
//
// Refuses on loopback, RFC 1918 private ranges, link-local, multicast,
// unspecified and CGNAT. Used to keep a URL we did not choose — a BIMI logo, a
// favicon, an AIA certificate pointer — from being turned into an SSRF probe
// against whatever is reachable from this host.
//
// Every answer must pass, not merely the first: a name that resolves to one
// public and one private address is a DNS-rebinding attempt, and taking the
// public answer as permission is exactly the mistake being avoided.
//
// Resolution is synchronous on purpose. The gap between "resolved and approved"
// and "connected" is a TOCTOU either way; keeping it as small as possible
// matters more than the milliseconds a cache would save.
func HostIsPublic(ctx context.Context, host string) bool {
	if host == "" {
		return false
	}

	// A literal IP needs no lookup — and must not get one, since resolving it
	// would just echo it back.
	if ip := net.ParseIP(host); ip != nil {
		return ipIsPublic(ip)
	}

	cctx, cancel := context.WithTimeout(ctx, DefaultResolveTimeout)
	defer cancel()

	resolver := &net.Resolver{}
	addrs, err := resolver.LookupIPAddr(cctx, host)
	if err != nil || len(addrs) == 0 {
		return false
	}

	for _, a := range addrs {
		if !ipIsPublic(a.IP) {
			return false
		}
	}
	return true
}

// ipIsPublic reports whether a single address is routable on the open Internet.
func ipIsPublic(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}

	if ip4 := ip.To4(); ip4 != nil {
		// 100.64.0.0/10 — Carrier-Grade NAT. Not private by net.IP.IsPrivate,
		// and not routable on the public Internet either.
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return false
		}
		// 192.0.0.0/24 — IETF protocol assignments, includes NAT64 discovery.
		if ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0 {
			return false
		}
		// 198.18.0.0/15 — benchmarking, routed inside some networks.
		if ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19) {
			return false
		}
		return true
	}

	// IPv6 unique-local (fc00::/7). net.IP.IsPrivate covers this, but it is
	// spelled out because the v4 branch returns early above and a future edit
	// to that branch must not silently change what v6 accepts.
	if len(ip) == net.IPv6len && ip[0]&0xfe == 0xfc {
		return false
	}

	return true
}
