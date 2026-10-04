package web

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// publicHost returns the host[:port] to put into links and OAuth redirect
// URIs built for this request.
//
// The request's own host (X-Forwarded-Host only from a trusted proxy, see
// clientip) is used when it is one this server is known by: the configured
// public hostname, the host of a configured OAuth redirect URI, or a
// loopback name for development. Anything else falls back to the public
// hostname: a forged Host must never steer an OAuth redirect_uri — that
// would hand the authorization code to someone else's host. Only when no
// public hostname is configured at all is a syntactically valid request host
// taken as is.
func (s *Server) publicHost(r *http.Request) string {
	host := strings.TrimSpace(s.clientIP.RequestHost(r))
	canonical := s.canonicalPublicHost()

	if isValidHostPort(host) {
		name := strings.ToLower(hostWithoutPort(host))
		if canonical == "" || s.isKnownHostname(name) {
			return host
		}
	}
	if canonical != "" {
		return canonical
	}
	return "localhost"
}

// publicBaseURL is scheme://publicHost(r).
func (s *Server) publicBaseURL(r *http.Request) string {
	host := s.publicHost(r)
	scheme := s.clientIP.RequestScheme(r)
	if scheme == "" {
		scheme = "https"
		if isLoopbackName(hostWithoutPort(host)) {
			scheme = "http"
		}
	}
	return scheme + "://" + host
}

// canonicalPublicHost is the configured internet-facing host[:port], "" if
// none is configured.
func (s *Server) canonicalPublicHost() string {
	h := strings.ToLower(strings.TrimSpace(s.publicEndpoints.Hostname))
	if h == "" {
		return ""
	}
	if p := s.publicEndpoints.HTTPSPort; p != 0 && p != 443 {
		return net.JoinHostPort(h, strconv.Itoa(p))
	}
	return h
}

// isKnownHostname reports whether name (no port, lowercase) is a name this
// server is legitimately reached by.
func (s *Server) isKnownHostname(name string) bool {
	if name == "" {
		return false
	}
	if isLoopbackName(name) {
		return true
	}
	if name == strings.ToLower(strings.TrimSpace(s.publicEndpoints.Hostname)) {
		return true
	}
	if s.oauthConfig != nil {
		for _, raw := range []string{s.oauthConfig.Google.RedirectURI, s.oauthConfig.Microsoft.RedirectURI} {
			if u, err := url.Parse(raw); err == nil && u.Hostname() != "" && strings.EqualFold(u.Hostname(), name) {
				return true
			}
		}
	}
	return false
}

func isLoopbackName(name string) bool {
	switch strings.ToLower(name) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// isValidHostPort accepts only a bare host[:port] — no scheme, path,
// userinfo, query or whitespace that could reshape a URL built from it.
func isValidHostPort(h string) bool {
	if h == "" || strings.ContainsAny(h, "/\\@?#% \t\r\n") {
		return false
	}
	u, err := url.Parse("https://" + h)
	return err == nil && u.Host == h && u.Hostname() != ""
}
