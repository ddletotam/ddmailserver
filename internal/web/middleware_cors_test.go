package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/clientip"
)

func corsRequest(t *testing.T, origin, host, remote, fwdHost string) *httptest.ResponseRecorder {
	t.Helper()
	s := &Server{clientIP: clientip.Default()}
	h := s.CORSMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "http://"+host+"/api/x", nil)
	req.Host = host
	req.RemoteAddr = remote
	req.Header.Set("Origin", origin)
	if fwdHost != "" {
		req.Header.Set("X-Forwarded-Host", fwdHost)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCORSOrigins(t *testing.T) {
	const ext = "203.0.113.10:5000"
	const proxy = "127.0.0.1:5000"
	cases := []struct {
		name, origin, host, remote, fwd string
		allowed                         bool
	}{
		{"localhost dev", "http://localhost:3000", "mail.example.com", ext, "", true},
		{"localhost no port", "http://localhost", "mail.example.com", ext, "", true},
		{"127.0.0.1", "http://127.0.0.1:8080", "mail.example.com", ext, "", true},
		{"ipv6 loopback", "http://[::1]:8080", "mail.example.com", ext, "", true},
		{"localhost suffix attack", "http://localhost.evil.com", "mail.example.com", ext, "", false},
		{"127 suffix attack", "http://127.0.0.1.evil.com", "mail.example.com", ext, "", false},
		{"localhost userinfo trick", "http://localhost@evil.com", "mail.example.com", ext, "", false},
		{"same host", "https://mail.example.com", "mail.example.com", ext, "", true},
		{"same host with port", "https://mail.example.com", "mail.example.com:8080", ext, "", true},
		{"host suffix attack", "https://mail.example.com.evil.com", "mail.example.com", ext, "", false},
		{"null origin", "null", "mail.example.com", ext, "", false},
		{"non-http scheme", "file://localhost", "mail.example.com", ext, "", false},
		{"forwarded host from trusted proxy", "https://mail.example.com", "127.0.0.1:8080", proxy, "mail.example.com", true},
		{"forwarded host from untrusted peer", "https://evil.com", "mail.example.com", ext, "evil.com", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := corsRequest(t, c.origin, c.host, c.remote, c.fwd)
			gotACAO := rec.Header().Get("Access-Control-Allow-Origin")
			if c.allowed {
				if rec.Code != http.StatusOK || gotACAO != c.origin || rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
					t.Fatalf("want allowed: code=%d ACAO=%q", rec.Code, gotACAO)
				}
			} else {
				if rec.Code != http.StatusForbidden || gotACAO != "" {
					t.Fatalf("want rejected: code=%d ACAO=%q", rec.Code, gotACAO)
				}
			}
		})
	}
}
