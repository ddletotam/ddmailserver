package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"sync"

	"golang.org/x/crypto/pbkdf2"
)

const (
	// encryptedPrefix marks encrypted passwords for reliable detection
	encryptedPrefix = "$enc$v1$"
	// pbkdf2Iterations is the number of PBKDF2 iterations for key derivation
	pbkdf2Iterations = 100000
	// saltSize is the size of the random salt for PBKDF2
	saltSize = 16
)

// deriveKey derives a 32-byte key from the secret using PBKDF2-SHA256
func deriveKey(secret string, salt []byte) []byte {
	return pbkdf2.Key([]byte(secret), salt, pbkdf2Iterations, 32, sha256.New)
}

// EncryptPassword encrypts a password using AES-GCM with PBKDF2 key derivation
func EncryptPassword(plaintext, secret string) (string, error) {
	if plaintext == "" {
		return "", nil
	}

	// Generate random salt for PBKDF2
	salt := make([]byte, saltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", err
	}

	key := deriveKey(secret, salt)

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := aesGCM.Seal(nonce, nonce, []byte(plaintext), nil)

	// Prepend salt to ciphertext: salt + nonce + ciphertext
	result := append(salt, ciphertext...)

	return encryptedPrefix + base64.StdEncoding.EncodeToString(result), nil
}

// DecryptPassword decrypts a password encrypted with AES-GCM
func DecryptPassword(ciphertext, secret string) (string, error) {
	return decrypt(ciphertext, func(salt []byte) []byte { return deriveKey(secret, salt) })
}

// keyCacheLimit bounds a KeyCache. There is one entry per stored ciphertext
// (its salt is random and fixed for life) — a few per account; past the
// limit the cache simply starts over.
const keyCacheLimit = 4096

// KeyCache decrypts like DecryptPassword but derives each key once. The
// PBKDF2 step (100k iterations) is the whole cost of a decryption, and
// decryption sits on hot paths: loading a user's accounts decrypts every
// secret of every account, and /send, /identities and /conversations do it
// on each call — about 0.7 s per call for six accounts. The derived key is no
// more sensitive than the secret it comes from, which the process holds
// anyway. Safe for concurrent use.
type KeyCache struct {
	mu   sync.Mutex
	keys map[[sha256.Size]byte][]byte
}

// NewKeyCache returns an empty cache.
func NewKeyCache() *KeyCache {
	return &KeyCache{keys: make(map[[sha256.Size]byte][]byte)}
}

// Decrypt is DecryptPassword with the derived key remembered per (secret, salt).
func (c *KeyCache) Decrypt(ciphertext, secret string) (string, error) {
	return decrypt(ciphertext, func(salt []byte) []byte { return c.key(secret, salt) })
}

func (c *KeyCache) key(secret string, salt []byte) []byte {
	// The secret itself is not kept as a map key — only a digest of it.
	h := sha256.New()
	h.Write([]byte(secret))
	h.Write([]byte{0})
	h.Write(salt)
	var id [sha256.Size]byte
	copy(id[:], h.Sum(nil))

	c.mu.Lock()
	key, ok := c.keys[id]
	c.mu.Unlock()
	if ok {
		return key
	}
	key = deriveKey(secret, salt)
	c.mu.Lock()
	if len(c.keys) >= keyCacheLimit {
		c.keys = make(map[[sha256.Size]byte][]byte)
	}
	c.keys[id] = key
	c.mu.Unlock()
	return key
}

// decrypt opens a ciphertext of EncryptPassword with the key derive gives for its salt.
func decrypt(ciphertext string, derive func(salt []byte) []byte) (string, error) {
	if ciphertext == "" {
		return "", nil
	}

	// Check for encrypted prefix
	if !strings.HasPrefix(ciphertext, encryptedPrefix) {
		return "", errors.New("invalid encrypted password format")
	}

	// Remove prefix
	ciphertext = strings.TrimPrefix(ciphertext, encryptedPrefix)

	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}

	// Extract salt (first 16 bytes)
	if len(data) < saltSize {
		return "", errors.New("ciphertext too short")
	}
	salt := data[:saltSize]
	data = data[saltSize:]

	key := derive(salt)

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonceSize := aesGCM.NonceSize()
	if len(data) < nonceSize {
		return "", errors.New("ciphertext too short")
	}

	nonce, ciphertextBytes := data[:nonceSize], data[nonceSize:]
	plaintext, err := aesGCM.Open(nil, nonce, ciphertextBytes, nil)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}

// IsEncrypted checks if a password is encrypted by checking for the prefix
func IsEncrypted(password string) bool {
	if password == "" {
		return true // Empty passwords are considered "encrypted" (no action needed)
	}
	return strings.HasPrefix(password, encryptedPrefix)
}
