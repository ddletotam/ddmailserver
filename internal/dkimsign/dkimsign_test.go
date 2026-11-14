package dkimsign

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
)

func writeKey(t *testing.T, dir, domain string) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(filepath.Join(dir, domain+".key"), pemData, 0o600); err != nil {
		t.Fatal(err)
	}
	return key
}

// Подпись ставится ключом домена From и проходит проверку; MIME-заголовки —
// под подписью. Домен без ключа уходит без подписи, и Has это говорит.
func TestSignVerifiesAndCoversMIME(t *testing.T) {
	dir := t.TempDir()
	key := writeKey(t, dir, "help-it-audit.ru")
	s := New("mf", dir)
	if !s.Has(`"lucky" <info@Help-IT-Audit.ru>`) || s.Has("info@letotam.ru") {
		t.Fatalf("Has: неверно")
	}
	var nilSigner *Signer
	if nilSigner.Has("info@help-it-audit.ru") {
		t.Fatalf("Has у nil")
	}

	msg := []byte("Message-ID: <1@help-it-audit.ru>\r\nDate: Sat, 14 Nov 2026 18:01:29 +0300\r\n" +
		"From: \"lucky\" <info@help-it-audit.ru>\r\nTo: denis.danilin@internet.ru\r\nSubject: test\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n=D0=90=D1=83=D0=B4=D0=B8=D1=82\r\n")
	signed := s.Sign(`"lucky" <info@help-it-audit.ru>`, msg)
	if bytes.Equal(signed, msg) {
		t.Fatal("не подписано")
	}

	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	txt := "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(pub)
	verifs, err := dkim.VerifyWithOptions(bytes.NewReader(signed), &dkim.VerifyOptions{
		LookupTXT: func(domain string) ([]string, error) {
			if domain != "mf._domainkey.help-it-audit.ru" {
				t.Errorf("lookup %s", domain)
			}
			return []string{txt}, nil
		},
	})
	if err != nil || len(verifs) != 1 || verifs[0].Err != nil {
		t.Fatalf("проверка: %v %+v", err, verifs)
	}
	v := verifs[0]
	if v.Domain != "help-it-audit.ru" {
		t.Errorf("d=%s", v.Domain)
	}
	h := strings.ToLower(strings.Join(v.HeaderKeys, ":"))
	for _, want := range []string{"from", "content-type", "mime-version", "subject"} {
		if !strings.Contains(h, want) {
			t.Errorf("h= без %s: %s", want, h)
		}
	}

	if out := s.Sign("info@letotam.ru", msg); !bytes.Equal(out, msg) {
		t.Errorf("домен без ключа подписан")
	}
}
