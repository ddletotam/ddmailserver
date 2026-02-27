package web

import (
	"crypto/rand"
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
func HashRecoveryKey(recoveryKey string) (string, error) {
	// Normalize: trim spaces and lowercase
	normalized := strings.ToLower(strings.TrimSpace(recoveryKey))

	hash, err := bcrypt.GenerateFromPassword([]byte(normalized), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("failed to hash recovery key: %w", err)
	}

	return string(hash), nil
}

// VerifyRecoveryKey checks if the provided recovery key matches the hash
func VerifyRecoveryKey(recoveryKey, hash string) bool {
	// Normalize: trim spaces and lowercase
	normalized := strings.ToLower(strings.TrimSpace(recoveryKey))

	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(normalized))
	return err == nil
}
