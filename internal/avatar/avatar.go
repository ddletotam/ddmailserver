// Package avatar resolves an email address to an avatar image by trying a
// chain of sources in priority order. Results are cached in the avatar_cache
// DB table with separate TTLs for hits and negative responses.
//
// Chain: CardDAV vCard PHOTO → Libravatar → Gravatar → BIMI → favicon.
// Each source has a tight per-call timeout so a single slow miss can't stall
// the whole chain.
package avatar

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/yourusername/mailserver/internal/db"
)

const (
	// Image fetch deadlines. Kept short so a slow source can't stall the page.
	perSourceTimeout = 3 * time.Second

	// Cache TTLs. Positive hits live long enough to feel cached but short
	// enough that a user updating their Gravatar gets picked up the same day.
	positiveTTL = 7 * 24 * time.Hour
	negativeTTL = 24 * time.Hour

	// Reasonable upper bound for an avatar payload; rejects anyone serving a
	// PDF or a multi-MB hi-res image at the favicon URL.
	maxBytes = 512 * 1024

	bimiLookupFmt = "default._bimi.%s"
	libravatarDefault = "https://seccdn.libravatar.org/avatar/%s?d=404&s=96"
	gravatarURL       = "https://www.gravatar.com/avatar/%s?d=404&s=96"
)

// Result is what the fetcher returns to the HTTP handler.
type Result struct {
	Data   []byte
	MIME   string
	Source string // "carddav" | "libravatar" | "gravatar" | "bimi" | "favicon" | "none"
}

// IsEmpty reports whether the resolver tried every source and found nothing.
func (r *Result) IsEmpty() bool {
	return r == nil || len(r.Data) == 0
}

// Fetcher orchestrates the source chain and the cache.
type Fetcher struct {
	db   *db.DB
	http *http.Client
}

// New builds a Fetcher; supply a non-nil DB so CardDAV lookups + caching work.
func New(database *db.DB) *Fetcher {
	return &Fetcher{
		db: database,
		http: &http.Client{
			Timeout: perSourceTimeout,
		},
	}
}

// Get returns the avatar for the given email, using the cache when possible.
// The userID scopes the CardDAV lookup; pass 0 to skip CardDAV.
func (f *Fetcher) Get(ctx context.Context, userID int64, email string) (*Result, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, errors.New("empty email")
	}

	// Cache first.
	if f.db != nil {
		entry, err := f.db.GetAvatar(email)
		if err == nil && entry != nil && !entry.Expired() {
			if len(entry.Data) > 0 {
				return &Result{Data: entry.Data, MIME: entry.MIME, Source: entry.Source}, nil
			}
			// Negative cache hit — still serve "empty" without re-trying.
			return &Result{Source: "none"}, nil
		}
	}

	result := f.resolve(ctx, userID, email)
	f.cache(email, result)
	return result, nil
}

// resolve walks the source chain. The first successful source wins.
func (f *Fetcher) resolve(ctx context.Context, userID int64, email string) *Result {
	domain := domainOf(email)

	if userID > 0 && f.db != nil {
		if r := f.fromCardDAV(ctx, userID, email); r != nil {
			return r
		}
	}
	if r := f.fromLibravatar(ctx, email); r != nil {
		return r
	}
	if r := f.fromGravatar(ctx, email); r != nil {
		return r
	}
	if domain != "" {
		if r := f.fromBIMI(ctx, domain); r != nil {
			return r
		}
		if r := f.fromFavicon(ctx, domain); r != nil {
			return r
		}
	}
	return &Result{Source: "none"}
}

func (f *Fetcher) cache(email string, r *Result) {
	if f.db == nil || r == nil {
		return
	}
	ttl := positiveTTL
	if len(r.Data) == 0 {
		ttl = negativeTTL
	}
	_ = f.db.PutAvatar(email, r.Source, r.Data, r.MIME, ttl.Milliseconds())
}

// ── Sources ──

func (f *Fetcher) fromCardDAV(ctx context.Context, userID int64, email string) *Result {
	c, err := f.db.GetContactByEmail(userID, email)
	if err != nil || c == nil || c.PhotoURL == "" {
		return nil
	}
	// PHOTO can be inline (data: URL) or a remote URL. The CardDAV sync only
	// keeps http(s) URLs today; once it learns to keep inline base64, this
	// branch returns immediately without a network round trip.
	if strings.HasPrefix(c.PhotoURL, "data:") {
		mime, data, ok := decodeDataURL(c.PhotoURL)
		if !ok {
			return nil
		}
		return &Result{Data: data, MIME: mime, Source: "carddav"}
	}
	if data, mime, ok := f.fetchURL(ctx, c.PhotoURL); ok {
		return &Result{Data: data, MIME: mime, Source: "carddav"}
	}
	return nil
}

func (f *Fetcher) fromLibravatar(ctx context.Context, email string) *Result {
	url := libravatarURL(email)
	if data, mime, ok := f.fetchURL(ctx, url); ok {
		return &Result{Data: data, MIME: mime, Source: "libravatar"}
	}
	return nil
}

func (f *Fetcher) fromGravatar(ctx context.Context, email string) *Result {
	url := fmt.Sprintf(gravatarURL, md5Hex(email))
	if data, mime, ok := f.fetchURL(ctx, url); ok {
		return &Result{Data: data, MIME: mime, Source: "gravatar"}
	}
	return nil
}

func (f *Fetcher) fromBIMI(ctx context.Context, domain string) *Result {
	url, ok := lookupBIMI(ctx, domain)
	if !ok {
		return nil
	}
	if data, mime, ok := f.fetchURL(ctx, url); ok {
		// BIMI images are SVG by spec — but some senders serve PNG. Accept both.
		return &Result{Data: data, MIME: mime, Source: "bimi"}
	}
	return nil
}

func (f *Fetcher) fromFavicon(ctx context.Context, domain string) *Result {
	for _, p := range []string{"/favicon.ico", "/favicon.png", "/apple-touch-icon.png"} {
		full := "https://" + domain + p
		if data, mime, ok := f.fetchURL(ctx, full); ok {
			return &Result{Data: data, MIME: mime, Source: "favicon"}
		}
	}
	// Last resort — Google's favicon mirror works even when the domain itself
	// doesn't serve a favicon at a guessable path.
	mirror := "https://www.google.com/s2/favicons?domain=" + url.QueryEscape(domain) + "&sz=64"
	if data, mime, ok := f.fetchURL(ctx, mirror); ok {
		return &Result{Data: data, MIME: mime, Source: "favicon"}
	}
	return nil
}

// ── Helpers ──

func md5Hex(email string) string {
	sum := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(sum[:])
}

func domainOf(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}

// libravatarURL uses the federated SRV record when present, otherwise the
// public seccdn. Libravatar is a drop-in Gravatar fallback used in OSS/fediverse.
func libravatarURL(email string) string {
	hash := md5Hex(email)
	domain := domainOf(email)
	if domain == "" {
		return fmt.Sprintf(libravatarDefault, hash)
	}
	_, addrs, err := net.LookupSRV("avatars", "tcp", domain)
	if err == nil && len(addrs) > 0 {
		host := strings.TrimSuffix(addrs[0].Target, ".")
		scheme := "https"
		if addrs[0].Port == 80 {
			scheme = "http"
		}
		return fmt.Sprintf("%s://%s/avatar/%s?d=404&s=96", scheme, host, hash)
	}
	return fmt.Sprintf(libravatarDefault, hash)
}

// lookupBIMI reads the default._bimi TXT record and returns the l= URL if
// present. We deliberately don't verify the VMC (a=) certificate — that's
// about brand trust, not about whether we have something to render.
var bimiL = regexp.MustCompile(`(?i)\bl\s*=\s*([^;\s]+)`)

func lookupBIMI(ctx context.Context, domain string) (string, bool) {
	name := fmt.Sprintf(bimiLookupFmt, domain)
	resolver := &net.Resolver{}
	cctx, cancel := context.WithTimeout(ctx, perSourceTimeout)
	defer cancel()
	records, err := resolver.LookupTXT(cctx, name)
	if err != nil || len(records) == 0 {
		return "", false
	}
	for _, rec := range records {
		// Some resolvers split long TXTs into chunks — join before regex.
		if m := bimiL.FindStringSubmatch(rec); len(m) == 2 {
			u := strings.Trim(m[1], "\"'")
			if strings.HasPrefix(u, "http") {
				return u, true
			}
		}
	}
	return "", false
}

// fetchURL pulls a URL with a per-source deadline and basic content sniffing.
// Returns false when the response is non-2xx, an HTML page, or larger than
// maxBytes (some servers return a 200 HTML 404 page for missing favicons).
func (f *Fetcher) fetchURL(ctx context.Context, urlStr string) ([]byte, string, bool) {
	cctx, cancel := context.WithTimeout(ctx, perSourceTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "GET", urlStr, nil)
	if err != nil {
		return nil, "", false
	}
	req.Header.Set("User-Agent", "DDMail-Avatar/1.0")
	req.Header.Set("Accept", "image/png,image/jpeg,image/webp,image/svg+xml,image/*;q=0.8,*/*;q=0.1")
	resp, err := f.http.Do(req)
	if err != nil {
		return nil, "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", false
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", false
	}
	if len(body) == 0 || len(body) > maxBytes {
		return nil, "", false
	}

	mime := resp.Header.Get("Content-Type")
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	mime = strings.ToLower(mime)
	if mime == "" {
		mime = http.DetectContentType(body)
		if i := strings.Index(mime, ";"); i >= 0 {
			mime = strings.TrimSpace(mime[:i])
		}
	}
	// Reject HTML "soft 404" responses.
	if strings.HasPrefix(mime, "text/html") || bytes.HasPrefix(bytes.TrimSpace(body), []byte("<")) && !strings.HasPrefix(mime, "image/svg") {
		return nil, "", false
	}
	return body, mime, true
}

// decodeDataURL extracts MIME + payload from a `data:image/png;base64,...` URL.
// Returns ok=false on malformed input — the caller falls through to the next source.
func decodeDataURL(u string) (string, []byte, bool) {
	if !strings.HasPrefix(u, "data:") {
		return "", nil, false
	}
	rest := strings.TrimPrefix(u, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", nil, false
	}
	meta, payload := rest[:comma], rest[comma+1:]
	parts := strings.Split(meta, ";")
	mime := parts[0]
	isB64 := false
	for _, p := range parts[1:] {
		if strings.EqualFold(p, "base64") {
			isB64 = true
		}
	}
	if !isB64 {
		return mime, []byte(payload), true
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, false
	}
	return mime, data, true
}
