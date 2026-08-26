package db

import (
	"errors"
	"strings"

	"github.com/yourusername/mailserver/internal/models"
	"golang.org/x/crypto/bcrypt"
)

// ErrInvalidCredentials is what every protocol endpoint reports outward. The
// reason a login failed — no such user, wrong password, revoked app password —
// stays in the log and never reaches the client, so a caller cannot probe which
// usernames exist.
var ErrInvalidCredentials = errors.New("invalid credentials")

// AuthenticateProtocol resolves a login on a protocol endpoint: IMAP, SMTP,
// CalDAV or CardDAV. It accepts either the account password or one of the
// user's application passwords.
//
// The web and desktop logins deliberately do NOT call this. An application
// password that reached the UI could mint more of itself and read the rest,
// which would defeat the point of issuing a scoped credential at all — see
// migrations/047. Those paths keep verifying PasswordHash directly.
//
// `username` may arrive in either form: some clients insist on an email-shaped
// login. The domain part is dropped, matching what IMAP and SMTP already did
// before this helper existed.
func (db *DB) AuthenticateProtocol(username, secret string) (*models.User, error) {
	if idx := strings.IndexByte(username, '@'); idx != -1 {
		username = username[:idx]
	}

	user, err := db.GetUserByUsername(username)
	if err != nil {
		return nil, ErrInvalidCredentials
	}

	// Application passwords are tried first when the secret has their exact
	// shape — 16 letters, dashes optional. That check is free, and it spares a
	// bcrypt round on every single request from a device that uses one. CalDAV
	// clients re-authenticate on each PROPFIND, so "every single request" is
	// not a figure of speech.
	if len(NormalizeAppPassword(secret)) == appPasswordLen {
		ok, err := db.VerifyAppPassword(user.ID, secret)
		if err != nil {
			return nil, err
		}
		if ok {
			// Unlike the account-password path below, this one honours bans.
			// A banned user keeping protocol access is a pre-existing gap and
			// widening it through a credential added today would be a choice;
			// closing it for the account password too is a separate decision.
			if user.IsBanned() {
				return nil, ErrInvalidCredentials
			}
			return user, nil
		}
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(secret)); err != nil {
		return nil, ErrInvalidCredentials
	}

	return user, nil
}
