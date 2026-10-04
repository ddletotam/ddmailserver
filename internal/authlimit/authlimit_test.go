package authlimit

import (
	"fmt"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(t *testing.T, cfg Config) (*Limiter, *clock) {
	t.Helper()
	l, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l.now = c.now
	return l, c
}

func fail(l *Limiter, ip, user string, n int) {
	for i := 0; i < n; i++ {
		l.Failure(ip, user, fmt.Sprintf("guess-%d", i))
	}
}

func TestIPBlockAfterLimit(t *testing.T) {
	l, clk := newTestLimiter(t, Config{MaxFailuresPerIP: 3, MaxFailuresPerUser: 100, Window: time.Minute, BlockDuration: 5 * time.Minute})

	fail(l, "198.51.100.1", "", 2)
	if !l.Allow("198.51.100.1", "alice") {
		t.Fatal("blocked before limit")
	}
	l.Failure("198.51.100.1", "bob", "x")
	if l.Allow("198.51.100.1", "alice") {
		t.Fatal("not blocked after limit")
	}
	if !l.Allow("198.51.100.2", "alice") {
		t.Fatal("other IP blocked")
	}
	clk.advance(5*time.Minute + time.Second)
	if !l.Allow("198.51.100.1", "alice") {
		t.Fatal("block did not expire")
	}
}

func TestWindowExpiresFailures(t *testing.T) {
	l, clk := newTestLimiter(t, Config{MaxFailuresPerIP: 3, MaxFailuresPerUser: 100, Window: time.Minute, BlockDuration: time.Hour})
	fail(l, "198.51.100.1", "", 2)
	clk.advance(2 * time.Minute)
	l.Failure("198.51.100.1", "", "another")
	if !l.Allow("198.51.100.1", "") {
		t.Fatal("old failures outside the window still counted")
	}
}

func TestUserBlockAcrossIPs(t *testing.T) {
	l, _ := newTestLimiter(t, Config{MaxFailuresPerIP: 100, MaxFailuresPerUser: 3, Window: time.Hour, BlockDuration: time.Hour})
	for i := 0; i < 3; i++ {
		l.Failure(fmt.Sprintf("203.0.113.%d", i+1), "Alice@example.com", fmt.Sprintf("p%d", i))
	}
	if l.Allow("203.0.113.99", "alice") {
		t.Fatal("distributed guessing against one user not blocked")
	}
	if !l.Allow("203.0.113.99", "bob") {
		t.Fatal("unrelated user blocked")
	}
}

func TestKnownIPExemptFromUserBlock(t *testing.T) {
	l, _ := newTestLimiter(t, Config{MaxFailuresPerIP: 100, MaxFailuresPerUser: 3, Window: time.Hour, BlockDuration: time.Hour})
	l.Success("192.0.2.10", "alice")
	fail(l, "203.0.113.5", "alice", 3)
	if l.Allow("203.0.113.5", "alice") {
		t.Fatal("attacker IP not blocked")
	}
	if !l.Allow("192.0.2.10", "alice") {
		t.Fatal("owner's known IP locked out by attacker")
	}
}

func TestRepeatedSameSecretCountsOnce(t *testing.T) {
	l, _ := newTestLimiter(t, Config{MaxFailuresPerIP: 3, MaxFailuresPerUser: 3, Window: time.Hour, BlockDuration: time.Hour})
	for i := 0; i < 50; i++ {
		l.Failure("192.0.2.1", "alice", "old-password")
	}
	if !l.Allow("192.0.2.1", "alice") {
		t.Fatal("stale-password retries caused a block")
	}
}

func TestIPv6SlashSixtyFourShared(t *testing.T) {
	l, _ := newTestLimiter(t, Config{MaxFailuresPerIP: 3, MaxFailuresPerUser: 100, Window: time.Hour, BlockDuration: time.Hour})
	l.Failure("2001:db8:1:2::1", "", "a")
	l.Failure("2001:db8:1:2::2", "", "b")
	l.Failure("[2001:db8:1:2:ffff::3]", "", "c")
	if l.Allow("2001:db8:1:2::abcd", "") {
		t.Fatal("rotating addresses inside one /64 evaded the limit")
	}
	if !l.Allow("2001:db8:1:3::1", "") {
		t.Fatal("neighbouring /64 blocked")
	}
	if !l.Allow("198.51.100.1", "") {
		t.Fatal("IPv4 blocked by IPv6 failures")
	}
}

func TestIPKey(t *testing.T) {
	cases := map[string]string{
		"192.0.2.1":          "192.0.2.1",
		"::ffff:192.0.2.1":   "192.0.2.1",
		"2001:db8::1":        "2001:db8::/64",
		"[2001:db8:0:1::5]":  "2001:db8:0:1::/64",
		"fe80::1%eth0":       "fe80::/64",
		"not-an-ip":          "not-an-ip",
		"2001:db8:aa:bb::ff": "2001:db8:aa:bb::/64",
	}
	for in, want := range cases {
		if got := IPKey(in); got != want {
			t.Errorf("IPKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUserKey(t *testing.T) {
	if UserKey(" Alice@Example.COM ") != "alice" {
		t.Fatal("user key not normalised")
	}
}

func TestNilLimiterAllows(t *testing.T) {
	var l *Limiter
	l.Failure("1.2.3.4", "a", "b")
	l.Success("1.2.3.4", "a")
	if !l.Allow("1.2.3.4", "a") {
		t.Fatal("nil limiter must allow")
	}
}

func TestPruneDropsStale(t *testing.T) {
	l, clk := newTestLimiter(t, Config{MaxFailuresPerIP: 5, MaxFailuresPerUser: 5, Window: time.Minute, BlockDuration: time.Minute})
	fail(l, "192.0.2.1", "alice", 2)
	clk.advance(10 * time.Minute)
	l.Allow("192.0.2.2", "bob") // triggers a prune
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.byIP) != 0 || len(l.byUser) != 0 {
		t.Fatalf("stale entries kept: ip=%d user=%d", len(l.byIP), len(l.byUser))
	}
}
