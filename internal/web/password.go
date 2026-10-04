package web

import (
	"log"

	"github.com/ddletotam/ddmailserver/internal/models"
	"golang.org/x/crypto/bcrypt"
)

// HashPassword creates a bcrypt hash of a password
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// VerifyPassword checks if a password matches the hash
func VerifyPassword(hash, password string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// providerPassword is what an edit form's "leave blank to keep current"
// password field really asks to store. A browser that sees an empty password
// input on this site autofills the user's own ddmail login password into it
// — prod lost a working Yandex CardDAV password that way: the next save
// stored the ddmail password, and the provider answered 401 from then on.
// That value is never what the user meant for a third-party server, so it
// counts as blank (keep the current password) and is logged.
func providerPassword(user *models.User, field, value string) string {
	if value == "" || user == nil || user.PasswordHash == "" {
		return value
	}
	if VerifyPassword(user.PasswordHash, value) {
		log.Printf("%s: ignored the user's own login password (browser autofill), keeping the current one", field)
		return ""
	}
	return value
}
