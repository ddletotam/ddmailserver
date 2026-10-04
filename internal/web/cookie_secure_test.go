package web

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"

	"github.com/yourusername/mailserver/internal/clientip"
	"github.com/yourusername/mailserver/internal/config"
)

func TestSecureCookieRule(t *testing.T) {
	const proxy = "127.0.0.1:5000"
	const ext = "203.0.113.9:5000"
	cases := []struct {
		name, mode, host, remote, proto string
		tls                             bool
		want                            bool
	}{
		{"auto: https from nginx", "auto", "mail.example.com", proxy, "https", false, true},
		{"auto: explicit http from nginx", "auto", "mail.example.com", proxy, "http", false, false},
		{"auto: forged proto from outside ignored", "auto", "mail.example.com", ext, "http", false, true},
		{"auto: direct TLS", "auto", "mail.example.com", ext, "", true, true},
		{"auto: unknown scheme, public host", "auto", "mail.example.com", proxy, "", false, true},
		{"auto: local dev", "auto", "localhost:8080", "127.0.0.1:1", "", false, false},
		{"empty mode = auto", "", "mail.example.com", proxy, "https", false, true},
		{"forced true on local dev", "true", "localhost:8080", "127.0.0.1:1", "", false, true},
		{"forced false over https", "false", "mail.example.com", proxy, "https", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{
				clientIP:        clientip.Default(),
				publicEndpoints: config.PublicEndpoints{SecureCookies: c.mode},
			}
			req := httptest.NewRequest("GET", "/", nil)
			req.Host = c.host
			req.RemoteAddr = c.remote
			if c.proto != "" {
				req.Header.Set("X-Forwarded-Proto", c.proto)
			}
			if c.tls {
				req.TLS = &tls.ConnectionState{}
			}
			if got := s.secureCookie(req); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

// Session and OAuth cookies both follow the rule — the OAuth ones used to
// look at r.TLS only, which is never set behind nginx.
func TestCookiesCarrySecureBehindProxy(t *testing.T) {
	s := &Server{clientIP: clientip.Default(), publicEndpoints: config.PublicEndpoints{SecureCookies: "auto"}}
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "mail.example.com"
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("X-Forwarded-Proto", "https")

	rec := httptest.NewRecorder()
	s.SetSessionCookie(rec, req, "tok")
	s.clearSessionCookie(rec, req)
	for _, c := range rec.Result().Cookies() {
		if !c.Secure || !c.HttpOnly {
			t.Errorf("cookie %s: Secure=%v HttpOnly=%v", c.Name, c.Secure, c.HttpOnly)
		}
	}
	if n := len(rec.Result().Cookies()); n != 2 {
		t.Fatalf("got %d cookies", n)
	}
}
