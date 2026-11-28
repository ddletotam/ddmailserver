package db

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	mcrypto "github.com/ddletotam/ddmailserver/internal/crypto"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// EnsureDKIMKey returns the public key (base64 DER, for the DNS record) of the
// domain's DKIM key, making the key on first call.
func (db *DB) EnsureDKIMKey(domain string) (string, error) {
	domain = strings.ToLower(domain)
	var pub string
	err := db.QueryRow(`SELECT public_key FROM dkim_keys WHERE domain = $1`, domain).Scan(&pub)
	if err == nil {
		return pub, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("dkim key of %s: %w", domain, err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", fmt.Errorf("generate dkim key: %w", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", err
	}
	pub = base64.StdEncoding.EncodeToString(der)
	priv := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	enc, err := mcrypto.EncryptPassword(priv, db.encryptionKey)
	if err != nil {
		return "", fmt.Errorf("encrypt dkim key: %w", err)
	}
	if _, err := db.Exec(`INSERT INTO dkim_keys (domain, private_key, public_key, created_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (domain) DO NOTHING`, domain, enc, pub, timeutil.Now()); err != nil {
		return "", fmt.Errorf("store dkim key: %w", err)
	}
	// A concurrent call may have won the insert: read back what is stored.
	if err := db.QueryRow(`SELECT public_key FROM dkim_keys WHERE domain = $1`, domain).Scan(&pub); err != nil {
		return "", err
	}
	return pub, nil
}

// DKIMSigner returns the signing key of the domain, or nil if it has none.
func (db *DB) DKIMSigner(domain string) (crypto.Signer, error) {
	var enc string
	err := db.QueryRow(`SELECT private_key FROM dkim_keys WHERE domain = $1`, strings.ToLower(domain)).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	priv, err := db.decryptSecret(enc)
	if err != nil {
		return nil, fmt.Errorf("decrypt dkim key of %s: %w", domain, err)
	}
	block, _ := pem.Decode([]byte(priv))
	if block == nil {
		return nil, fmt.Errorf("dkim key of %s: no PEM", domain)
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}
