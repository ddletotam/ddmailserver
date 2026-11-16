package mx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/parser"
	"github.com/emersion/go-smtp"
)

func writeSelfSigned(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "mx.test"},
		DNSNames:     []string{"mx.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func serve(t *testing.T, s *Server) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.smtpServer.Serve(l) }()
	t.Cleanup(func() { _ = s.Stop() })
	return l.Addr().String()
}

// The MX port offers STARTTLS and the handshake completes: before 2026-11-16
// EHLO listed no STARTTLS and all inbound mail arrived in plaintext.
func TestMXOffersSTARTTLS(t *testing.T) {
	certFile, keyFile := writeSelfSigned(t)
	s := New(nil, "127.0.0.1:0", "mx.test")
	if err := s.EnableSTARTTLS(certFile, keyFile); err != nil {
		t.Fatal(err)
	}
	addr := serve(t, s)
	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Hello("sender.test"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Extension("STARTTLS"); !ok {
		t.Fatal("EHLO does not offer STARTTLS")
	}
	tc, err := smtp.DialStartTLS(addr, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("STARTTLS: %v", err)
	}
	defer tc.Close()
	if err := tc.Noop(); err != nil { // go-smtp greets lazily: STARTTLS runs on the first command
		t.Fatalf("STARTTLS: %v", err)
	}
	if st, ok := tc.TLSConnectionState(); !ok || !st.HandshakeComplete {
		t.Fatal("no TLS after STARTTLS")
	}
}

// Without a certificate the port still takes mail, just without STARTTLS.
func TestMXWithoutCertificateHasNoSTARTTLS(t *testing.T) {
	s := New(nil, "127.0.0.1:0", "mx.test")
	c, err := smtp.Dial(serve(t, s))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Hello("sender.test"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		t.Fatal("STARTTLS offered without a certificate")
	}
}

func TestFormatReceived(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	st := &tls.ConnectionState{Version: tls.VersionTLS13, CipherSuite: tls.TLS_AES_128_GCM_SHA256}
	got := formatReceived("smtp.mail.ru", "send172.i.mail.ru", "89.221.237.201", "mail.letotam.ru", st, "info@help-it-audit.ru", now)
	want := "Received: from smtp.mail.ru (send172.i.mail.ru [89.221.237.201])\r\n\tby mail.letotam.ru with ESMTPS (TLS 1.3 TLS_AES_128_GCM_SHA256)\r\n\tfor <info@help-it-audit.ru>; Mon, 16 Nov 2026 12:00:00 +0000\r\n"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	// Plain session, hostile HELO: no line breaks or brackets make it through.
	got = formatReceived("evil\r\nX-Injected: 1 (x)", "unknown", "1.2.3.4", "mx.test", nil, "", now)
	want = "Received: from evilX-Injected:1x (unknown [1.2.3.4])\r\n\tby mx.test with ESMTP; Mon, 16 Nov 2026 12:00:00 +0000\r\n"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	// The spam check reads HELO, PTR and IP back from it.
	hop := parser.ParseReceivedChain(map[string][]string{"Received": {strings.TrimPrefix(strings.TrimSpace(formatReceived("smtp.mail.ru", "send172.i.mail.ru", "89.221.237.201", "mx.test", nil, "", now)), "Received: ")}})
	if len(hop) != 1 || hop[0].From != "smtp.mail.ru" || hop[0].FromPTR != "send172.i.mail.ru" || hop[0].FromIP != "89.221.237.201" {
		t.Fatalf("parsed back as %+v", hop)
	}
}
