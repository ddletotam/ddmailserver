package web

import (
	"github.com/yourusername/mailserver/internal/crypto"
)

// EncryptPassword encrypts a password using AES-GCM
func EncryptPassword(plaintext, secret string) (string, error) {
	return crypto.EncryptPassword(plaintext, secret)
}

// DecryptPassword decrypts a password encrypted with AES-GCM
func DecryptPassword(ciphertext, secret string) (string, error) {
	return crypto.DecryptPassword(ciphertext, secret)
}
