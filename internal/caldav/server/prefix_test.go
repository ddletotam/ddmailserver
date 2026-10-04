package server

import (
	"net/http/httptest"
	"testing"
)

func TestEffectivePrefixTrustsForwardedHostOnlyFromProxy(t *testing.T) {
	s := New(nil, "/caldav/")
	cases := []struct {
		name, host, remote, fwd, want string
	}{
		{"subdomain via proxy", "127.0.0.1:8080", "127.0.0.1:5000", "caldav.letotam.ru", "/"},
		{"subdomain direct Host", "caldav.letotam.ru:443", "203.0.113.1:5000", "", "/"},
		{"main host via proxy", "127.0.0.1:8080", "127.0.0.1:5000", "mail.letotam.ru", "/caldav/"},
		{"forged forwarded host from outside", "mail.letotam.ru", "203.0.113.1:5000", "caldav.letotam.ru", "/caldav/"},
	}
	for _, c := range cases {
		req := httptest.NewRequest("PROPFIND", "/caldav/", nil)
		req.Host = c.host
		req.RemoteAddr = c.remote
		if c.fwd != "" {
			req.Header.Set("X-Forwarded-Host", c.fwd)
		}
		if got := s.effectivePrefix(req); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
