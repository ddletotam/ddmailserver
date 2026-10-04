package clientip

import (
	"net"
	"net/http/httptest"
	"testing"
)

func mustResolver(t *testing.T, trusted []string) *Resolver {
	t.Helper()
	r, err := New(trusted)
	if err != nil {
		t.Fatalf("New(%v): %v", trusted, err)
	}
	return r
}

func TestFromAddr(t *testing.T) {
	cases := map[string]string{
		"192.0.2.1:5555":             "192.0.2.1",
		"[2001:db8::1]:443":          "2001:db8::1",
		"[2001:db8::2]:443":          "2001:db8::2",
		"[fe80::1%eth0]:22":          "fe80::1",
		"2001:db8::3":                "2001:db8::3",
		"192.0.2.9":                  "192.0.2.9",
		"[::ffff:198.51.100.4]:1234": "198.51.100.4",
		"garbage":                    "garbage",
	}
	for in, want := range cases {
		if got := FromAddr(in); got != want {
			t.Errorf("FromAddr(%q) = %q, want %q", in, got, want)
		}
	}
	// Two IPv6 clients must not share a bucket (the old code cut at ':').
	if FromAddr("[2001:db8::1]:1") == FromAddr("[2001:db8::2]:1") {
		t.Error("distinct IPv6 clients collapse to one key")
	}
}

func TestFromNetAddr(t *testing.T) {
	if got := FromNetAddr(&net.TCPAddr{IP: net.ParseIP("2001:db8::7"), Port: 993}); got != "2001:db8::7" {
		t.Errorf("got %q", got)
	}
	if got := FromNetAddr(&net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 993}); got != "192.0.2.7" {
		t.Errorf("got %q", got)
	}
	if got := FromNetAddr(nil); got != "" {
		t.Errorf("nil addr: got %q", got)
	}
}

func TestFromRequestUntrustedPeerIgnoresHeaders(t *testing.T) {
	r := mustResolver(t, nil)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.50:40000"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Real-IP", "5.6.7.8")
	if got := r.FromRequest(req); got != "203.0.113.50" {
		t.Errorf("got %q, want direct peer", got)
	}
}

func TestFromRequestTrustedProxy(t *testing.T) {
	r := mustResolver(t, nil)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	// Client spoofed "1.1.1.1"; nginx appended the real peer.
	req.Header.Set("X-Forwarded-For", "1.1.1.1, 203.0.113.9")
	if got := r.FromRequest(req); got != "203.0.113.9" {
		t.Errorf("got %q, want rightmost untrusted hop", got)
	}

	req = httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "[::1]:50000"
	req.Header.Set("X-Forwarded-For", "2001:db8::abcd")
	if got := r.FromRequest(req); got != "2001:db8::abcd" {
		t.Errorf("IPv6 via proxy: got %q", got)
	}

	req = httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("X-Real-IP", "198.51.100.3")
	if got := r.FromRequest(req); got != "198.51.100.3" {
		t.Errorf("X-Real-IP: got %q", got)
	}

	req = httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	if got := r.FromRequest(req); got != "127.0.0.1" {
		t.Errorf("no headers: got %q", got)
	}

	// Malformed hop: stop, do not trust anything to its left.
	req = httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("X-Forwarded-For", "9.9.9.9, not-an-ip")
	if got := r.FromRequest(req); got != "127.0.0.1" {
		t.Errorf("malformed hop: got %q", got)
	}
}

func TestFromRequestProxyChain(t *testing.T) {
	r := mustResolver(t, []string{"10.0.0.0/8", "127.0.0.1"})
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Add("X-Forwarded-For", "6.6.6.6, 198.51.100.77")
	req.Header.Add("X-Forwarded-For", "10.1.2.3")
	if got := r.FromRequest(req); got != "198.51.100.77" {
		t.Errorf("chain: got %q", got)
	}
}

func TestEmptyTrustListTrustsNobody(t *testing.T) {
	r := mustResolver(t, []string{})
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := r.FromRequest(req); got != "127.0.0.1" {
		t.Errorf("got %q", got)
	}
}

func TestRequestHost(t *testing.T) {
	r := mustResolver(t, nil)
	req := httptest.NewRequest("GET", "http://mail.example.com/", nil)
	req.RemoteAddr = "203.0.113.1:1"
	req.Header.Set("X-Forwarded-Host", "evil.example")
	if got := r.RequestHost(req); got != "mail.example.com" {
		t.Errorf("untrusted peer: got %q", got)
	}
	req.RemoteAddr = "127.0.0.1:1"
	if got := r.RequestHost(req); got != "evil.example" {
		t.Errorf("trusted peer: got %q", got)
	}
}

func TestNewRejectsGarbage(t *testing.T) {
	if _, err := New([]string{"not-a-cidr/99"}); err == nil {
		t.Error("expected error")
	}
	if _, err := New([]string{"300.1.1.1"}); err == nil {
		t.Error("expected error")
	}
}

func TestRequestScheme(t *testing.T) {
	r := mustResolver(t, nil)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.1:1"
	req.Header.Set("X-Forwarded-Proto", "http")
	if got := r.RequestScheme(req); got != "" {
		t.Errorf("untrusted peer: got %q", got)
	}
	req.RemoteAddr = "127.0.0.1:1"
	if got := r.RequestScheme(req); got != "http" {
		t.Errorf("trusted peer: got %q", got)
	}
	req.Header.Set("X-Forwarded-Proto", "javascript")
	if got := r.RequestScheme(req); got != "" {
		t.Errorf("bogus proto: got %q", got)
	}
}
