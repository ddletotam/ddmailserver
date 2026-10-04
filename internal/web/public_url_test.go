package web

import (
	"net/http/httptest"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/clientip"
	"github.com/ddletotam/ddmailserver/internal/config"
)

func newURLServer(publicHost string) *Server {
	return &Server{
		clientIP:        clientip.Default(),
		publicEndpoints: config.PublicEndpoints{Hostname: publicHost, HTTPSPort: 443},
		oauthConfig: &config.OAuthConfig{Google: config.GoogleOAuthConfig{
			RedirectURI: "https://alt.example.com/oauth/google/callback",
		}},
	}
}

func TestPublicBaseURL(t *testing.T) {
	const ext = "203.0.113.7:4000"
	const proxy = "127.0.0.1:4000"
	cases := []struct {
		name, public, host, remote, fwdHost, fwdProto, want string
	}{
		{"behind proxy, real host", "mail.example.com", "127.0.0.1:8080", proxy, "mail.example.com", "https", "https://mail.example.com"},
		{"forged X-Forwarded-Host from outside", "mail.example.com", "mail.example.com", ext, "evil.com", "http", "https://mail.example.com"},
		{"forged Host via trusted proxy", "mail.example.com", "evil.com", proxy, "evil.com", "https", "https://mail.example.com"},
		{"forged Host direct", "mail.example.com", "evil.com", ext, "", "", "https://mail.example.com"},
		{"host from oauth redirect config", "mail.example.com", "alt.example.com", ext, "", "", "https://alt.example.com"},
		{"localhost dev", "mail.example.com", "localhost:8080", "127.0.0.1:1", "", "", "http://localhost:8080"},
		{"userinfo trick", "mail.example.com", "127.0.0.1:8080", proxy, "mail.example.com@evil.com", "https", "https://mail.example.com"},
		{"path trick", "mail.example.com", "127.0.0.1:8080", proxy, "evil.com/x", "https", "https://mail.example.com"},
		{"no public host configured", "", "dev.box:8080", ext, "", "", "https://dev.box:8080"},
		{"no public host, garbage", "", "a@b", ext, "", "", "http://localhost"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newURLServer(c.public)
			req := httptest.NewRequest("GET", "/oauth/google/calendar/start", nil)
			req.Host = c.host
			req.RemoteAddr = c.remote
			if c.fwdHost != "" {
				req.Header.Set("X-Forwarded-Host", c.fwdHost)
			}
			if c.fwdProto != "" {
				req.Header.Set("X-Forwarded-Proto", c.fwdProto)
			}
			if got := s.publicBaseURL(req); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestCanonicalPublicHostPort(t *testing.T) {
	s := &Server{publicEndpoints: config.PublicEndpoints{Hostname: "Mail.Example.com", HTTPSPort: 8443}}
	if got := s.canonicalPublicHost(); got != "mail.example.com:8443" {
		t.Fatalf("got %q", got)
	}
}
