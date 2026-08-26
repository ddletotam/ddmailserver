// Package tlsverify provides TLS certificate verification that completes an
// incomplete chain by following the certificate's own AIA pointer.
//
// # Why this exists
//
// A server is supposed to send its leaf certificate together with every
// intermediate needed to reach a trusted root. Plenty do not — they send the
// leaf alone. Browsers paper over it: the leaf carries an "Authority
// Information Access · CA Issuers" URL naming the missing intermediate, and
// Safari, Chrome and Windows fetch it. Go's crypto/tls deliberately does not,
// so the same server that opens fine in a browser fails here with
// "x509: certificate signed by unknown authority".
//
// The alternative in this codebase used to be worse: two ICS fetch paths simply
// set InsecureSkipVerify and accepted anything at all. Completing the chain is
// strictly better than not checking it.
//
// # The danger
//
// Go offers no hook that runs only when normal verification fails. To intervene
// at all, InsecureSkipVerify must be set — which turns off the chain check, the
// hostname check and the expiry check together. Everything this package does
// after that is re-implementing them by hand, and a mistake there does not
// crash: it silently accepts forged certificates forever.
//
// So the rule for editing this file: verifyChain must always end in a real
// x509.Certificate.Verify against the system roots, with DNSName set and
// ExtKeyUsageServerAuth required. AIA fetching may only ever ADD candidate
// intermediates to that call. It must never become a reason to skip it.
package tlsverify

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/yourusername/mailserver/internal/netguard"
)

const (
	// maxAIADepth caps how many intermediates will be chased for one handshake.
	// Real chains are one or two deep; anything longer is a loop or a stall
	// dressed as a certificate.
	maxAIADepth = 4

	// maxCertSize bounds a downloaded certificate. A DER certificate is a few
	// kilobytes; this leaves room for a PKCS#7 bundle without letting the URL
	// stream indefinitely.
	maxCertSize = 256 * 1024

	// fetchTimeout bounds one AIA download. It happens inside a TLS handshake,
	// so a slow CA host must not hold the connection open.
	fetchTimeout = 5 * time.Second

	// cacheTTL is how long a fetched intermediate is reused. Intermediates
	// rotate on a scale of years; an hour keeps a busy CalDAV poller from
	// re-fetching on every connection while still letting a revocation-driven
	// change land the same day.
	cacheTTL = time.Hour
)

// cache holds intermediates already fetched, keyed by AIA URL.
var cache = struct {
	sync.RWMutex
	entries map[string]cacheEntry
}{entries: make(map[string]cacheEntry)}

type cacheEntry struct {
	certs   []*x509.Certificate
	fetched time.Time
}

// Config returns a tls.Config that verifies serverName with AIA completion.
//
// serverName is required and is what the certificate is checked against. It is
// not read back out of the connection: taking the name from the peer would let
// the peer choose which name it is judged by, which is the whole point of the
// check.
func Config(serverName string) *tls.Config {
	return &tls.Config{
		ServerName: serverName,
		// Disabled so the callback below runs at all; the callback performs
		// the full verification that this flag switches off. See the package
		// comment before changing anything here.
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: verifier(serverName),
		MinVersion:            tls.VersionTLS12,
	}
}

// HTTPClient returns an http.Client whose TLS verification completes chains via
// AIA. Suitable wherever a plain http.Client would be used.
func HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: Transport(),
	}
}

// Transport returns an http.Transport with AIA-completing verification.
//
// The per-connection ServerName is not known when the transport is built, so
// verification is wired through DialTLSContext, where the host being dialled is
// finally in hand.
func Transport() *http.Transport {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		dialer := &tls.Dialer{Config: Config(host)}
		return dialer.DialContext(ctx, network, addr)
	}
	return base
}

// verifier builds the VerifyPeerCertificate callback for one expected hostname.
func verifier(serverName string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("tls: peer sent no certificates")
		}

		certs := make([]*x509.Certificate, 0, len(rawCerts))
		for i, raw := range rawCerts {
			cert, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("tls: parsing peer certificate %d: %w", i, err)
			}
			certs = append(certs, cert)
		}

		return verifyChain(serverName, certs)
	}
}

// fetchIssuersFn is the AIA downloader, swappable so the completion loop can be
// tested without a network. Production never reassigns it.
var fetchIssuersFn = fetchIssuers

// rootsForVerify names the trust store. nil means the host's own, which is the
// only value production ever uses; tests substitute a private pool so the
// completion loop can be exercised against certificates they minted.
//
// It is a variable rather than a parameter deliberately: a parameter would show
// up at the call site as an invitation to pass something, and "which roots did
// this call trust?" must not be a question anyone has to ask when reading the
// verification path.
var rootsForVerify *x509.CertPool

// verifyChain performs the verification crypto/tls was told to skip, fetching
// missing intermediates along the way.
func verifyChain(serverName string, certs []*x509.Certificate) error {
	leaf := certs[0]

	intermediates := x509.NewCertPool()
	for _, c := range certs[1:] {
		intermediates.AddCert(c)
	}

	// The chain the peer supplied is tried first and unchanged. A correctly
	// configured server never reaches the AIA code below.
	err := verifyAgainstSystemRoots(serverName, leaf, intermediates)
	if err == nil {
		return nil
	}

	// Only a missing link is worth chasing. An expired certificate, a name
	// mismatch or a revoked chain are answers, not gaps — retrying with more
	// intermediates cannot change them, and treating them as retryable is how
	// a verifier quietly turns into a rubber stamp.
	if _, isUnknownAuthority := err.(x509.UnknownAuthorityError); !isUnknownAuthority {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout*maxAIADepth)
	defer cancel()

	// Walk upwards: fetch the issuer of the highest certificate known so far,
	// add it, and re-verify. Each round can only add to the pool.
	current := certs[len(certs)-1]
	for depth := 0; depth < maxAIADepth; depth++ {
		issuers := fetchIssuersFn(ctx, current)
		if len(issuers) == 0 {
			// Nothing more to add: report the original failure rather than a
			// vaguer one about the fetch, since the chain gap is the problem
			// the operator has to fix.
			return err
		}

		var highest *x509.Certificate
		for _, issuer := range issuers {
			intermediates.AddCert(issuer)
			highest = issuer
		}

		if vErr := verifyAgainstSystemRoots(serverName, leaf, intermediates); vErr == nil {
			log.Printf("tlsverify: completed chain for %s via AIA (%d level(s))", serverName, depth+1)
			return nil
		} else if _, isUnknownAuthority := vErr.(x509.UnknownAuthorityError); !isUnknownAuthority {
			// The gap closed and something else is wrong — that is the real
			// answer now.
			return vErr
		}

		if highest == nil {
			return err
		}
		current = highest
	}

	return err
}

// verifyAgainstSystemRoots is the single place a certificate is actually
// judged. rootsForVerify is nil in production, and nil means the host's trust
// store — the pool is a variable only so tests can mint their own chain; see
// its declaration.
func verifyAgainstSystemRoots(serverName string, leaf *x509.Certificate, intermediates *x509.CertPool) error {
	_, err := leaf.Verify(x509.VerifyOptions{
		DNSName:       serverName,
		Intermediates: intermediates,
		Roots:         rootsForVerify,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err
}

// fetchIssuers downloads the certificates named by a certificate's AIA
// "CA Issuers" pointers.
func fetchIssuers(ctx context.Context, cert *x509.Certificate) []*x509.Certificate {
	var out []*x509.Certificate

	for _, rawURL := range cert.IssuingCertificateURL {
		certs, ok := fetchIssuerURL(ctx, rawURL)
		if !ok {
			continue
		}
		out = append(out, certs...)
	}

	return out
}

// fetchIssuerURL downloads and parses one AIA pointer.
//
// The URL comes out of a certificate that has NOT been validated yet — at this
// point anyone able to complete a TCP connection can choose it. That makes this
// function an SSRF primitive unless it is fenced, so: scheme allow-list, public
// addresses only, hard timeout, hard size cap.
func fetchIssuerURL(ctx context.Context, rawURL string) ([]*x509.Certificate, bool) {
	if cached, ok := cacheGet(rawURL); ok {
		return cached, true
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, false
	}
	// AIA pointers are conventionally plain HTTP — deliberately, since fetching
	// them over HTTPS would need a certificate chain to validate the
	// certificate chain. Nothing is trusted on the basis of the transport here:
	// whatever comes back still has to verify against the system roots.
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, false
	}

	if !netguard.HostIsPublic(ctx, u.Hostname()) {
		log.Printf("tlsverify: refusing AIA fetch from non-public host %q", u.Hostname())
		return nil, false
	}

	cctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("User-Agent", "DDMailServer/1.0 (AIA)")

	// A plain client, not this package's own: fetching over HTTPS must not
	// recurse back into AIA completion.
	client := &http.Client{
		Timeout: fetchTimeout,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("too many redirects")
			}
			// Each hop is a fresh chance to be pointed somewhere internal.
			if !netguard.HostIsPublic(r.Context(), r.URL.Hostname()) {
				return fmt.Errorf("redirect to non-public host")
			}
			return nil
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("tlsverify: AIA fetch %s: %v", rawURL, err)
		return nil, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, false
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCertSize+1))
	if err != nil || len(body) == 0 || len(body) > maxCertSize {
		return nil, false
	}

	certs := parseCertificates(body)
	if len(certs) == 0 {
		return nil, false
	}

	cachePut(rawURL, certs)
	return certs, true
}

// parseCertificates reads the three encodings an AIA URL answers with: bare
// DER, PEM, and a PKCS#7 bundle (`.p7c`, which some CAs serve).
func parseCertificates(body []byte) []*x509.Certificate {
	if certs, err := x509.ParseCertificates(body); err == nil && len(certs) > 0 {
		return certs
	}

	var out []*x509.Certificate
	rest := body
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
			out = append(out, cert)
		}
	}
	if len(out) > 0 {
		return out
	}

	// PKCS#7: Go has no parser in the standard library, so the certificates are
	// lifted out by walking the DER for the embedded certificate SET. Anything
	// recovered still has to verify, so a sloppy extraction cannot grant trust
	// — at worst it finds nothing and the chain stays incomplete.
	return certsFromPKCS7(body)
}

// certsFromPKCS7 scavenges X.509 certificates out of a PKCS#7 container by
// scanning for DER SEQUENCE headers and trying to parse each as a certificate.
func certsFromPKCS7(body []byte) []*x509.Certificate {
	var out []*x509.Certificate

	for i := 0; i+4 < len(body); i++ {
		// 0x30 0x82 <len-hi> <len-lo> — a SEQUENCE with a two-byte length, the
		// shape every real certificate starts with.
		if body[i] != 0x30 || body[i+1] != 0x82 {
			continue
		}
		length := int(body[i+2])<<8 | int(body[i+3])
		end := i + 4 + length
		if end > len(body) {
			continue
		}
		cert, err := x509.ParseCertificate(body[i:end])
		if err != nil {
			continue
		}
		out = append(out, cert)
		i = end - 1
	}

	return out
}

func cacheGet(key string) ([]*x509.Certificate, bool) {
	cache.RLock()
	entry, ok := cache.entries[key]
	cache.RUnlock()
	if !ok || time.Since(entry.fetched) > cacheTTL {
		return nil, false
	}
	return entry.certs, true
}

func cachePut(key string, certs []*x509.Certificate) {
	cache.Lock()
	defer cache.Unlock()
	cache.entries[key] = cacheEntry{certs: certs, fetched: time.Now()}
}
