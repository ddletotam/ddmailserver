package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const recoveryKeyWordCount = 12

// GenerateRecoveryKey generates a recovery key using random words from wordlist
func GenerateRecoveryKey() (string, error) {
	words := make([]string, recoveryKeyWordCount)
	wordlistLen := big.NewInt(int64(len(recoveryKeyWordlist)))

	for i := 0; i < recoveryKeyWordCount; i++ {
		// Generate cryptographically secure random index
		index, err := rand.Int(rand.Reader, wordlistLen)
		if err != nil {
			return "", fmt.Errorf("failed to generate random number: %w", err)
		}
		words[i] = recoveryKeyWordlist[index.Int64()]
	}

	return strings.Join(words, " "), nil
}

// HashRecoveryKey creates a bcrypt hash of the recovery key
// First hashes with SHA-256 to avoid bcrypt's 72-byte limit
func HashRecoveryKey(recoveryKey string) (string, error) {
	// Normalize: trim spaces and lowercase
	normalized := strings.ToLower(strings.TrimSpace(recoveryKey))

	// Hash with SHA-256 first (to get fixed length, avoiding bcrypt's 72-byte limit)
	sha := sha256.Sum256([]byte(normalized))
	shaHex := hex.EncodeToString(sha[:])

	// Then hash the SHA-256 hex string with bcrypt
	hash, err := bcrypt.GenerateFromPassword([]byte(shaHex), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("failed to hash recovery key: %w", err)
	}

	return string(hash), nil
}

// VerifyRecoveryKey checks if the provided recovery key matches the hash
func VerifyRecoveryKey(recoveryKey, hash string) bool {
	// Normalize: trim spaces and lowercase
	normalized := strings.ToLower(strings.TrimSpace(recoveryKey))

	// Hash with SHA-256 first (same as in HashRecoveryKey)
	sha := sha256.Sum256([]byte(normalized))
	shaHex := hex.EncodeToString(sha[:])

	// Compare the SHA-256 hex string with the bcrypt hash
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(shaHex))
	return err == nil
}
