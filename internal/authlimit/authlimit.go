// Package authlimit throttles password guessing across every protocol that
// accepts credentials (web, desktop API, IMAP, SMTP submission, CalDAV,
// CardDAV).
//
// Failed attempts are counted per client IP (IPv6 by /64, since one host
// controls a whole /64) and per username within a sliding window. Reaching
// the limit blocks that key for BlockDuration; while blocked, callers must
// reject the login without checking the password.
//
// Two measures keep the limiter from locking out legitimate users:
//   - The same wrong secret repeated by the same key counts once. A phone or
//     CalDAV client with a stale password retries the same secret on every
//     request; an attacker has to try different ones.
//   - An IP that has logged in successfully as a user is exempt from that
//     user's username block (not from its own IP block) for KnownIPTTL, so an
//     attacker hammering an account from elsewhere cannot lock its owner out.
package authlimit

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Config tunes the limiter. Zero fields take the DefaultConfig value.
type Config struct {
	MaxFailuresPerIP   int           `yaml:"max_failures_per_ip"`
	MaxFailuresPerUser int           `yaml:"max_failures_per_user"`
	Window             time.Duration `yaml:"window"`
	BlockDuration      time.Duration `yaml:"block_duration"`
	KnownIPTTL         time.Duration `yaml:"known_ip_ttl"`
}

// DefaultConfig returns the limits used for unset fields.
func DefaultConfig() Config {
	return Config{
		MaxFailuresPerIP:   10,
		MaxFailuresPerUser: 10,
		Window:             15 * time.Minute,
		BlockDuration:      15 * time.Minute,
		KnownIPTTL:         7 * 24 * time.Hour,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.MaxFailuresPerIP <= 0 {
		c.MaxFailuresPerIP = d.MaxFailuresPerIP
	}
	if c.MaxFailuresPerUser <= 0 {
		c.MaxFailuresPerUser = d.MaxFailuresPerUser
	}
	if c.Window <= 0 {
		c.Window = d.Window
	}
	if c.BlockDuration <= 0 {
		c.BlockDuration = d.BlockDuration
	}
	if c.KnownIPTTL <= 0 {
		c.KnownIPTTL = d.KnownIPTTL
	}
	return c
}

const (
	// maxEntries bounds each map so a flood of distinct IPs/usernames cannot
	// exhaust memory. Beyond it new keys go untracked (fail open) until
	// expired entries are pruned.
	maxEntries = 100000
	// pruneEvery is how often expired entries are swept.
	pruneEvery = time.Minute
)

type failure struct {
	at time.Time
	fp uint64
}

type counter struct {
	fails        []failure
	blockedUntil time.Time
}

// Limiter tracks failed authentication attempts. A nil *Limiter allows
// everything, which keeps call sites and tests simple.
type Limiter struct {
	cfg  Config
	salt []byte
	now  func() time.Time

	mu        sync.Mutex
	byIP      map[string]*counter
	byUser    map[string]*counter
	knownIP   map[string]time.Time // user + "\x00" + ipKey → expiry
	lastPrune time.Time
	overflow  bool
}

// New creates a limiter.
func New(cfg Config) (*Limiter, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("authlimit: generating salt: %w", err)
	}
	return &Limiter{
		cfg:     cfg.withDefaults(),
		salt:    salt,
		now:     time.Now,
		byIP:    make(map[string]*counter),
		byUser:  make(map[string]*counter),
		knownIP: make(map[string]time.Time),
	}, nil
}

// IPKey normalises a client IP: IPv4 as is, IPv6 reduced to its /64.
// Unparsable input is used verbatim so it still gets its own bucket.
func IPKey(ip string) string {
	a, err := netip.ParseAddr(strings.Trim(strings.TrimSpace(ip), "[]"))
	if err != nil {
		return strings.TrimSpace(ip)
	}
	a = a.Unmap().WithZone("")
	if a.Is6() {
		p, err := a.Prefix(64)
		if err == nil {
			return p.String()
		}
	}
	return a.String()
}

// UserKey normalises a login name the way protocol logins resolve it:
// case-insensitive, with any @domain part dropped.
func UserKey(username string) string {
	u := strings.ToLower(strings.TrimSpace(username))
	if i := strings.IndexByte(u, '@'); i != -1 {
		u = u[:i]
	}
	return u
}

// Allow reports whether an attempt from ip for username may proceed. When it
// returns false the caller must fail the login without verifying the secret.
func (l *Limiter) Allow(ip, username string) bool {
	if l == nil {
		return true
	}
	now := l.now()
	ipk, uk := IPKey(ip), UserKey(username)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybePrune(now)

	if c := l.byIP[ipk]; c != nil && now.Before(c.blockedUntil) {
		return false
	}
	if uk == "" {
		return true
	}
	if c := l.byUser[uk]; c != nil && now.Before(c.blockedUntil) {
		if exp, ok := l.knownIP[uk+"\x00"+ipk]; ok && now.Before(exp) {
			return true
		}
		return false
	}
	return true
}

// Failure records a failed attempt. secret is only fingerprinted (keyed
// hash, never stored) so that repeating one wrong secret counts once.
func (l *Limiter) Failure(ip, username, secret string) {
	if l == nil {
		return
	}
	now := l.now()
	ipk, uk := IPKey(ip), UserKey(username)
	fp := l.fingerprint(uk, secret)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybePrune(now)

	if l.record(l.byIP, ipk, fp, now, l.cfg.MaxFailuresPerIP) {
		log.Printf("authlimit: blocking IP %s for %s after %d failed logins", ipk, l.cfg.BlockDuration, l.cfg.MaxFailuresPerIP)
	}
	if uk != "" && l.record(l.byUser, uk, fp, now, l.cfg.MaxFailuresPerUser) {
		log.Printf("authlimit: blocking user %q for %s after %d failed logins", uk, l.cfg.BlockDuration, l.cfg.MaxFailuresPerUser)
	}
}

// Success remembers that ip legitimately authenticated as username, which
// exempts it from that username's block.
func (l *Limiter) Success(ip, username string) {
	if l == nil {
		return
	}
	uk := UserKey(username)
	if uk == "" {
		return
	}
	now := l.now()
	key := uk + "\x00" + IPKey(ip)

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.knownIP[key]; !ok && len(l.knownIP) >= maxEntries {
		l.pruneLocked(now)
		if len(l.knownIP) >= maxEntries {
			return
		}
	}
	l.knownIP[key] = now.Add(l.cfg.KnownIPTTL)
}

func (l *Limiter) fingerprint(userKey, secret string) uint64 {
	m := hmac.New(sha256.New, l.salt)
	m.Write([]byte(userKey))
	m.Write([]byte{0})
	m.Write([]byte(secret))
	return binary.BigEndian.Uint64(m.Sum(nil))
}

// record adds a failure to key's counter and reports whether it just became
// blocked.
func (l *Limiter) record(m map[string]*counter, key string, fp uint64, now time.Time, max int) bool {
	c := m[key]
	if c == nil {
		if len(m) >= maxEntries {
			l.pruneLocked(now)
			if len(m) >= maxEntries {
				if !l.overflow {
					log.Printf("authlimit: table full (%d entries), new keys are not tracked", maxEntries)
					l.overflow = true
				}
				return false
			}
		}
		c = &counter{}
		m[key] = c
	}
	if now.Before(c.blockedUntil) {
		return false
	}

	cutoff := now.Add(-l.cfg.Window)
	kept := c.fails[:0]
	for _, f := range c.fails {
		if f.at.After(cutoff) {
			kept = append(kept, f)
		}
	}
	c.fails = kept
	for _, f := range c.fails {
		if f.fp == fp {
			return false
		}
	}
	c.fails = append(c.fails, failure{at: now, fp: fp})
	if len(c.fails) >= max {
		c.blockedUntil = now.Add(l.cfg.BlockDuration)
		c.fails = nil
		return true
	}
	return false
}

func (l *Limiter) maybePrune(now time.Time) {
	if now.Sub(l.lastPrune) >= pruneEvery {
		l.pruneLocked(now)
	}
}

func (l *Limiter) pruneLocked(now time.Time) {
	l.lastPrune = now
	cutoff := now.Add(-l.cfg.Window)
	for _, m := range []map[string]*counter{l.byIP, l.byUser} {
		for k, c := range m {
			if now.Before(c.blockedUntil) {
				continue
			}
			if len(c.fails) == 0 || !c.fails[len(c.fails)-1].at.After(cutoff) {
				delete(m, k)
			}
		}
	}
	for k, exp := range l.knownIP {
		if !now.Before(exp) {
			delete(l.knownIP, k)
		}
	}
	if len(l.byIP) < maxEntries && len(l.byUser) < maxEntries {
		l.overflow = false
	}
}
