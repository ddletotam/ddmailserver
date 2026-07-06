// Package dkimsign signs outgoing direct-delivery mail with DKIM.
//
// One RSA key per sending domain: <key_dir>/<domain>.key (PEM, PKCS#1 or
// PKCS#8). The signing domain is taken from the From address, so a single
// server signs for every local domain it hosts — as long as a key file for
// that domain exists. Domains without a key are sent unsigned (best effort:
// deliverability degrades, delivery still happens).
package dkimsign

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/emersion/go-msgauth/dkim"
)

// Signer holds per-domain signing keys. Immutable after New; safe for
// concurrent use.
type Signer struct {
	selector string
	keys     map[string]crypto.Signer
}

// New loads every "<domain>.key" from keyDir. Returns nil (signing disabled)
// when the selector is empty, the dir is unset, or no keys load — callers
// treat a nil *Signer as "pass through unsigned".
func New(selector, keyDir string) *Signer {
	if selector == "" || keyDir == "" {
		return nil
	}
	entries, err := os.ReadDir(keyDir)
	if err != nil {
		log.Printf("DKIM: key dir %s unreadable, signing disabled: %v", keyDir, err)
		return nil
	}
	keys := make(map[string]crypto.Signer)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".key") {
			continue
		}
		domain := strings.ToLower(strings.TrimSuffix(name, ".key"))
		key, err := loadPrivateKey(filepath.Join(keyDir, name))
		if err != nil {
			log.Printf("DKIM: skipping key for %s: %v", domain, err)
			continue
		}
		keys[domain] = key
		log.Printf("DKIM: loaded signing key for %s (selector %s)", domain, selector)
	}
	if len(keys) == 0 {
		log.Printf("DKIM: no usable keys in %s, signing disabled", keyDir)
		return nil
	}
	return &Signer{selector: selector, keys: keys}
}

func loadPrivateKey(path string) (crypto.Signer, error) {
	pemData, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in %s", path)
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse key: %w", err)
	}
	signer, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("unsupported key type %T (want RSA)", k)
	}
	return signer, nil
}

// Sign returns msg with a DKIM-Signature header prepended when a key exists
// for the From domain; the original msg otherwise. Never fails the send: a
// signing error logs and returns the unsigned message.
func (s *Signer) Sign(from string, msg []byte) []byte {
	if s == nil {
		return msg
	}
	domain := domainOf(from)
	key, ok := s.keys[domain]
	if !ok {
		return msg
	}
	opts := &dkim.SignOptions{
		Domain:   domain,
		Selector: s.selector,
		Signer:   key,
		HeaderKeys: []string{
			"From", "To", "Subject", "Date", "Message-Id",
		},
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationRelaxed,
	}
	var signed bytes.Buffer
	if err := dkim.Sign(&signed, bytes.NewReader(msg), opts); err != nil {
		log.Printf("DKIM: signing for %s failed, sending unsigned: %v", domain, err)
		return msg
	}
	return signed.Bytes()
}

// domainOf extracts the lowercased domain of an address that may be either
// bare or "Name <addr>".
func domainOf(from string) string {
	addr := from
	if i := strings.Index(addr, "<"); i != -1 {
		if j := strings.Index(addr[i:], ">"); j != -1 {
			addr = addr[i+1 : i+j]
		}
	}
	at := strings.LastIndexByte(addr, '@')
	if at == -1 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(addr[at+1:]))
}
