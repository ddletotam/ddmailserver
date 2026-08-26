package tlsverify

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The point of this file is not that valid chains pass — it is that invalid
// ones fail. Setting InsecureSkipVerify hands this package the entire burden of
// verification, and the failure mode of getting it wrong is silent acceptance
// of forged certificates. Every "must be rejected" case below stands in for a
// way that could happen.

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

func newCA(t *testing.T, name string) *testCA {
	t.Helper()
	return issue(t, nil, name, true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), nil)
}

// issue mints a certificate, self-signed when parent is nil.
func issue(t *testing.T, parent *testCA, name string, isCA bool, notBefore, notAfter time.Time, dnsNames []string) *testCA {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		BasicConstraintsValid: true,
		IsCA:                  isCA,
		DNSNames:              dnsNames,
	}
	if isCA {
		tmpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature
	} else {
		tmpl.KeyUsage = x509.KeyUsageDigitalSignature
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}

	signerCert, signerKey := tmpl, key
	if parent != nil {
		signerCert, signerKey = parent.cert, parent.key
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, signerCert, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatalf("create certificate %q: %v", name, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate %q: %v", name, err)
	}

	return &testCA{cert: cert, key: key, der: der}
}

// verifyWithRoots runs the same logic verifyChain does, against a private root
// set. The production path deliberately hardcodes the system store — that is
// the property being protected — so the tests exercise the decision through
// x509.Verify with the same options.
func verifyWithRoots(serverName string, chain []*x509.Certificate, roots *x509.CertPool) error {
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	_, err := chain[0].Verify(x509.VerifyOptions{
		DNSName:       serverName,
		Intermediates: intermediates,
		Roots:         roots,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err
}

func TestValidChainAccepted(t *testing.T) {
	root := newCA(t, "Test Root")
	inter := issue(t, root, "Test Intermediate", true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), nil)
	leaf := issue(t, inter, "leaf", false, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), []string{"example.test"})

	roots := x509.NewCertPool()
	roots.AddCert(root.cert)

	if err := verifyWithRoots("example.test", []*x509.Certificate{leaf.cert, inter.cert}, roots); err != nil {
		t.Fatalf("a complete valid chain should verify: %v", err)
	}
}

// TestRejections covers each way a forged or broken certificate could slip
// through if the hand-rolled verification forgot a check.
func TestRejections(t *testing.T) {
	root := newCA(t, "Test Root")
	inter := issue(t, root, "Test Intermediate", true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), nil)
	roots := x509.NewCertPool()
	roots.AddCert(root.cert)

	t.Run("wrong hostname", func(t *testing.T) {
		leaf := issue(t, inter, "leaf", false, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), []string{"example.test"})
		err := verifyWithRoots("attacker.test", []*x509.Certificate{leaf.cert, inter.cert}, roots)
		if err == nil {
			t.Fatal("a certificate for another name was accepted")
		}
		if _, ok := err.(x509.HostnameError); !ok {
			t.Errorf("expected a hostname error, got %T: %v", err, err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		leaf := issue(t, inter, "leaf", false,
			time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour), []string{"example.test"})
		err := verifyWithRoots("example.test", []*x509.Certificate{leaf.cert, inter.cert}, roots)
		if err == nil {
			t.Fatal("an expired certificate was accepted")
		}
	})

	t.Run("not yet valid", func(t *testing.T) {
		leaf := issue(t, inter, "leaf", false,
			time.Now().Add(24*time.Hour), time.Now().Add(48*time.Hour), []string{"example.test"})
		if err := verifyWithRoots("example.test", []*x509.Certificate{leaf.cert, inter.cert}, roots); err == nil {
			t.Fatal("a not-yet-valid certificate was accepted")
		}
	})

	t.Run("self-signed", func(t *testing.T) {
		self := issue(t, nil, "example.test", false, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), []string{"example.test"})
		if err := verifyWithRoots("example.test", []*x509.Certificate{self.cert}, roots); err == nil {
			t.Fatal("a self-signed certificate was accepted")
		}
	})

	t.Run("chains to an untrusted root", func(t *testing.T) {
		rogueRoot := newCA(t, "Rogue Root")
		rogueInter := issue(t, rogueRoot, "Rogue Intermediate", true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), nil)
		leaf := issue(t, rogueInter, "leaf", false, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), []string{"example.test"})

		// The rogue chain is complete and internally consistent — only its root
		// is not trusted. Supplying every intermediate must not help.
		err := verifyWithRoots("example.test", []*x509.Certificate{leaf.cert, rogueInter.cert, rogueRoot.cert}, roots)
		if err == nil {
			t.Fatal("a chain to an untrusted root was accepted")
		}
		if _, ok := err.(x509.UnknownAuthorityError); !ok {
			t.Errorf("expected UnknownAuthorityError, got %T: %v", err, err)
		}
	})

	t.Run("leaf without server-auth usage", func(t *testing.T) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		tmpl := &x509.Certificate{
			SerialNumber:          serial,
			Subject:               pkix.Name{CommonName: "leaf"},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(24 * time.Hour),
			BasicConstraintsValid: true,
			DNSNames:              []string{"example.test"},
			// Client auth only — must not be usable to impersonate a server.
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, inter.cert, &key.PublicKey, inter.key)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		leaf, _ := x509.ParseCertificate(der)

		if err := verifyWithRoots("example.test", []*x509.Certificate{leaf, inter.cert}, roots); err == nil {
			t.Fatal("a client-auth certificate was accepted for a server")
		}
	})
}

// TestVerifyChainRejectsUntrusted: whatever the reason, a certificate that
// cannot be tied to a system root must not pass. AIA chasing may only ever
// close a gap — never conclude "fetched something, good enough".
//
// Note the error kind: x509.Verify builds the chain before it looks at the
// hostname, so a self-signed certificate fails as UnknownAuthorityError even
// when the name is also wrong. That ordering is why verifyChain cannot use
// "is this a hostname error?" to decide whether to chase — it keys off the
// authority error instead, and every other outcome is returned untouched.
func TestVerifyChainRejectsUntrusted(t *testing.T) {
	cases := map[string]*x509.Certificate{
		"self-signed, right name": issue(t, nil, "example.test", false,
			time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), []string{"example.test"}).cert,
		"self-signed, wrong name": issue(t, nil, "example.test", false,
			time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), []string{"example.test"}).cert,
		"self-signed and expired": issue(t, nil, "example.test", false,
			time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour), []string{"example.test"}).cert,
	}

	for name, cert := range cases {
		host := "example.test"
		if strings.Contains(name, "wrong name") {
			host = "attacker.test"
		}
		// No AIA URL on these, so nothing is fetched and the original failure
		// is what comes back.
		if err := verifyChain(host, []*x509.Certificate{cert}); err == nil {
			t.Errorf("%s: verifyChain accepted an untrusted certificate", name)
		}
	}
}

// TestVerifyChainReturnsNonGapErrorsUnchanged pins the branch that decides
// whether to chase. A rogue chain that is complete and self-consistent still
// fails as UnknownAuthorityError — and after AIA finds nothing, that same
// error must surface rather than being replaced by something about the fetch.
func TestVerifyChainReturnsNonGapErrorsUnchanged(t *testing.T) {
	rogueRoot := newCA(t, "Rogue Root")
	leaf := issue(t, rogueRoot, "leaf", false,
		time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), []string{"example.test"})

	err := verifyChain("example.test", []*x509.Certificate{leaf.cert, rogueRoot.cert})
	if err == nil {
		t.Fatal("a chain to a rogue root was accepted")
	}
	if _, ok := err.(x509.UnknownAuthorityError); !ok {
		t.Errorf("expected UnknownAuthorityError to survive the AIA attempt, got %T: %v", err, err)
	}
}

// TestFetchIssuerURLRefusesPrivateHosts is the SSRF guard. The AIA URL comes
// out of an unverified certificate, so at that moment anyone who can complete a
// TCP connection chooses it.
func TestFetchIssuerURLRefusesPrivateHosts(t *testing.T) {
	// A local server that would answer if it were ever reached. It must not be.
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.Write([]byte("nope"))
	}))
	defer srv.Close()

	ctx := t.Context()

	if _, ok := fetchIssuerURL(ctx, srv.URL); ok {
		t.Error("a loopback AIA URL was fetched")
	}
	if reached {
		t.Error("the request actually reached a loopback service — the guard runs too late")
	}

	for _, u := range []string{
		"http://127.0.0.1/ca.crt",
		"http://localhost/ca.crt",
		"http://10.0.0.5/ca.crt",
		"http://192.168.1.1/ca.crt",
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"http://[::1]/ca.crt",
		"http://100.64.0.1/ca.crt", // CGNAT
	} {
		if _, ok := fetchIssuerURL(ctx, u); ok {
			t.Errorf("private/loopback URL was fetched: %s", u)
		}
	}
}

// TestFetchIssuerURLRefusesOddSchemes: an AIA pointer may name LDAP or a file,
// and neither is something to go and open.
func TestFetchIssuerURLRefusesOddSchemes(t *testing.T) {
	for _, u := range []string{
		"ldap://ca.example.com/cn=CA",
		"file:///etc/passwd",
		"ftp://ca.example.com/ca.crt",
		"gopher://ca.example.com/",
		"not a url at all",
	} {
		if _, ok := fetchIssuerURL(t.Context(), u); ok {
			t.Errorf("non-HTTP scheme was fetched: %s", u)
		}
	}
}

// TestParseCertificatesFormats covers what CAs actually serve on an AIA URL.
func TestParseCertificatesFormats(t *testing.T) {
	ca := newCA(t, "Format Test CA")

	if got := parseCertificates(ca.der); len(got) != 1 {
		t.Errorf("DER: got %d certificates, want 1", len(got))
	}

	pemBytes := []byte("-----BEGIN CERTIFICATE-----\n" +
		wrap(encodeBase64(ca.der), 64) +
		"\n-----END CERTIFICATE-----\n")
	if got := parseCertificates(pemBytes); len(got) != 1 {
		t.Errorf("PEM: got %d certificates, want 1", len(got))
	}

	if got := parseCertificates([]byte("garbage")); len(got) != 0 {
		t.Errorf("garbage: got %d certificates, want 0", len(got))
	}
	if got := parseCertificates(nil); len(got) != 0 {
		t.Errorf("nil: got %d certificates, want 0", len(got))
	}
}

// TestConfigShape guards the two settings the whole design rests on: the
// callback must be present (without it InsecureSkipVerify means what it says),
// and the server name must be fixed by the caller rather than taken from the
// peer.
func TestConfigShape(t *testing.T) {
	cfg := Config("mail.example.com")

	if cfg.VerifyPeerCertificate == nil {
		t.Fatal("no VerifyPeerCertificate — InsecureSkipVerify would disable verification outright")
	}
	if !cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must be set, or the callback never runs on failure")
	}
	if cfg.ServerName != "mail.example.com" {
		t.Errorf("ServerName = %q", cfg.ServerName)
	}
	if cfg.MinVersion < tlsMinAcceptable {
		t.Errorf("MinVersion = %d, want at least TLS 1.2", cfg.MinVersion)
	}

	// An empty peer certificate list must be refused, not treated as "nothing
	// to check".
	if err := cfg.VerifyPeerCertificate(nil, nil); err == nil {
		t.Error("an empty certificate list was accepted")
	}
	if err := cfg.VerifyPeerCertificate([][]byte{[]byte("not a certificate")}, nil); err == nil {
		t.Error("an unparseable certificate was accepted")
	}
}

const tlsMinAcceptable = 0x0303 // tls.VersionTLS12

func encodeBase64(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var sb strings.Builder
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], b[i:])
		v := uint32(chunk[0])<<16 | uint32(chunk[1])<<8 | uint32(chunk[2])
		sb.WriteByte(alphabet[(v>>18)&0x3f])
		sb.WriteByte(alphabet[(v>>12)&0x3f])
		if n > 1 {
			sb.WriteByte(alphabet[(v>>6)&0x3f])
		} else {
			sb.WriteByte('=')
		}
		if n > 2 {
			sb.WriteByte(alphabet[v&0x3f])
		} else {
			sb.WriteByte('=')
		}
	}
	return sb.String()
}

func wrap(s string, width int) string {
	var sb strings.Builder
	for i := 0; i < len(s); i += width {
		if i > 0 {
			sb.WriteByte('\n')
		}
		end := i + width
		if end > len(s) {
			end = len(s)
		}
		sb.WriteString(s[i:end])
	}
	return sb.String()
}

var _ = net.ParseIP // keep the net import honest if the address list above shrinks

// --- the completion loop itself -------------------------------------------
//
// Everything above proves bad certificates are rejected. These prove the
// feature actually works: a server that sends only its leaf must end up
// verified once the intermediate is fetched. Without this the package could be
// a very well-tested no-op.

// withTestChain points verification at a private root and a stub AIA fetcher,
// restoring both afterwards.
func withTestChain(t *testing.T, roots *x509.CertPool, fetch func(*x509.Certificate) []*x509.Certificate) {
	t.Helper()

	oldRoots, oldFetch := rootsForVerify, fetchIssuersFn
	rootsForVerify = roots
	fetchIssuersFn = func(_ context.Context, cert *x509.Certificate) []*x509.Certificate {
		return fetch(cert)
	}
	t.Cleanup(func() {
		rootsForVerify, fetchIssuersFn = oldRoots, oldFetch
		cache.Lock()
		cache.entries = make(map[string]cacheEntry)
		cache.Unlock()
	})
}

// TestAIACompletesOneLevel is the SmallKZZZ case exactly: the server sends only
// the leaf, and the intermediate has to be fetched before the chain reaches a
// trusted root.
func TestAIACompletesOneLevel(t *testing.T) {
	root := newCA(t, "AIA Root")
	inter := issue(t, root, "AIA Intermediate", true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), nil)
	leaf := issue(t, inter, "leaf", false, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), []string{"example.test"})

	roots := x509.NewCertPool()
	roots.AddCert(root.cert)

	fetched := 0
	withTestChain(t, roots, func(cert *x509.Certificate) []*x509.Certificate {
		fetched++
		if cert.Equal(leaf.cert) {
			return []*x509.Certificate{inter.cert}
		}
		return nil
	})

	// Leaf alone — precisely what mail.skiftrade.kz sends.
	if err := verifyChain("example.test", []*x509.Certificate{leaf.cert}); err != nil {
		t.Fatalf("AIA completion failed: %v", err)
	}
	if fetched != 1 {
		t.Errorf("fetched %d times, want 1", fetched)
	}
}

// TestAIANotUsedWhenChainIsComplete: a correctly configured server must cost
// zero network round trips inside its handshake.
func TestAIANotUsedWhenChainIsComplete(t *testing.T) {
	root := newCA(t, "AIA Root")
	inter := issue(t, root, "AIA Intermediate", true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), nil)
	leaf := issue(t, inter, "leaf", false, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), []string{"example.test"})

	roots := x509.NewCertPool()
	roots.AddCert(root.cert)

	withTestChain(t, roots, func(*x509.Certificate) []*x509.Certificate {
		t.Error("AIA was consulted even though the peer sent a complete chain")
		return nil
	})

	if err := verifyChain("example.test", []*x509.Certificate{leaf.cert, inter.cert}); err != nil {
		t.Fatalf("complete chain rejected: %v", err)
	}
}

// TestAIACannotRescueABadCertificate is the one that matters most. Fetching
// must close a gap and nothing else: a certificate for the wrong host, or an
// expired one, stays rejected however helpful the AIA answer is.
func TestAIACannotRescueABadCertificate(t *testing.T) {
	root := newCA(t, "AIA Root")
	inter := issue(t, root, "AIA Intermediate", true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), nil)
	roots := x509.NewCertPool()
	roots.AddCert(root.cert)

	t.Run("wrong hostname", func(t *testing.T) {
		leaf := issue(t, inter, "leaf", false, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), []string{"example.test"})
		withTestChain(t, roots, func(*x509.Certificate) []*x509.Certificate {
			return []*x509.Certificate{inter.cert}
		})
		if err := verifyChain("attacker.test", []*x509.Certificate{leaf.cert}); err == nil {
			t.Fatal("AIA completion accepted a certificate issued for another name")
		}
	})

	t.Run("expired", func(t *testing.T) {
		leaf := issue(t, inter, "leaf", false,
			time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour), []string{"example.test"})
		withTestChain(t, roots, func(*x509.Certificate) []*x509.Certificate {
			return []*x509.Certificate{inter.cert}
		})
		if err := verifyChain("example.test", []*x509.Certificate{leaf.cert}); err == nil {
			t.Fatal("AIA completion accepted an expired certificate")
		}
	})

	t.Run("fetched intermediate does not actually sign the leaf", func(t *testing.T) {
		// A hostile AIA answer: a perfectly valid intermediate under our root,
		// but not the one that issued this leaf.
		otherInter := issue(t, root, "Unrelated Intermediate", true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), nil)
		rogueRoot := newCA(t, "Rogue Root")
		rogueInter := issue(t, rogueRoot, "Rogue Intermediate", true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), nil)
		leaf := issue(t, rogueInter, "leaf", false, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), []string{"example.test"})

		withTestChain(t, roots, func(*x509.Certificate) []*x509.Certificate {
			return []*x509.Certificate{otherInter.cert}
		})
		if err := verifyChain("example.test", []*x509.Certificate{leaf.cert}); err == nil {
			t.Fatal("an unrelated intermediate was accepted as completing the chain")
		}
	})
}

// TestAIADepthIsBounded: a CA that keeps pointing at another certificate must
// not hold a handshake open indefinitely.
func TestAIADepthIsBounded(t *testing.T) {
	root := newCA(t, "AIA Root")
	roots := x509.NewCertPool()
	roots.AddCert(root.cert)

	rogue := newCA(t, "Endless")
	leaf := issue(t, rogue, "leaf", false, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), []string{"example.test"})

	calls := 0
	withTestChain(t, roots, func(*x509.Certificate) []*x509.Certificate {
		calls++
		// Always answers, never helps.
		next := issue(t, rogue, "filler", true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), nil)
		return []*x509.Certificate{next.cert}
	})

	if err := verifyChain("example.test", []*x509.Certificate{leaf.cert}); err == nil {
		t.Fatal("an endless AIA chain was accepted")
	}
	if calls > maxAIADepth {
		t.Errorf("fetched %d times, want at most %d", calls, maxAIADepth)
	}
}
