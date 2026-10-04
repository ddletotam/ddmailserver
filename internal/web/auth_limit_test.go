package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourusername/mailserver/internal/authlimit"
	"github.com/yourusername/mailserver/internal/clientip"
)

func blockedLimiter(t *testing.T, ip string) *authlimit.Limiter {
	t.Helper()
	l, err := authlimit.New(authlimit.Config{MaxFailuresPerIP: 1})
	if err != nil {
		t.Fatal(err)
	}
	l.Failure(ip, "", "guess")
	return l
}

// Throttled logins answer 401 without touching the database (nil here:
// reaching it would panic), i.e. without checking the password.
func TestWebLoginThrottled(t *testing.T) {
	s := &Server{clientIP: clientip.Default(), authLimiter: blockedLimiter(t, "2001:db8:1:2::1")}

	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"alice","password":"x"}`))
	req.RemoteAddr = "[2001:db8:1:2::77]:5000" // same /64 as the blocked address
	rec := httptest.NewRecorder()
	s.HandleLogin(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("web login: got %d, want 401", rec.Code)
	}

	req = httptest.NewRequest("POST", "/api/desktop/v1/auth/login", strings.NewReader(`{"username":"alice","password":"x"}`))
	req.RemoteAddr = "[2001:db8:1:2::78]:5000"
	rec = httptest.NewRecorder()
	s.HandleDesktopLogin(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("desktop login: got %d, want 401", rec.Code)
	}
}

// The request rate limiter keys on the real client behind the proxy, and
// does not let a direct client pick its own key via X-Forwarded-For.
func TestRateLimitMiddlewareClientIP(t *testing.T) {
	s := &Server{clientIP: clientip.Default()}
	rl := NewRateLimiter(1, time.Minute)
	defer rl.Stop()
	h := s.RateLimitMiddleware(rl)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	do := func(remote, xff string) int {
		req := httptest.NewRequest("POST", "/api/login", nil)
		req.RemoteAddr = remote
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if do("203.0.113.5:1", "1.1.1.1") != http.StatusOK {
		t.Fatal("first request rejected")
	}
	// Spoofed header from a direct client: still the same bucket.
	if do("203.0.113.5:2", "2.2.2.2") != http.StatusTooManyRequests {
		t.Fatal("X-Forwarded-For from untrusted peer bypassed the limit")
	}
	// Two IPv6 clients are distinct buckets.
	if do("[2001:db8::1]:1", "") != http.StatusOK || do("[2001:db8::2]:1", "") != http.StatusOK {
		t.Fatal("IPv6 clients share one bucket")
	}
	// Behind the proxy, the forwarded client is the key.
	if do("127.0.0.1:1", "198.51.100.1") != http.StatusOK || do("127.0.0.1:1", "198.51.100.2") != http.StatusOK {
		t.Fatal("clients behind the proxy share one bucket")
	}
}
