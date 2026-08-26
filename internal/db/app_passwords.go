package db

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// appPasswordAlphabet is lowercase latin only, the same shape Apple uses for
// its app-specific passwords. Restricting it to letters means the secret can be
// read aloud and typed on a phone keyboard without a case switch, and leaves no
// 0/O or 1/l pairs to misread.
const appPasswordAlphabet = "abcdefghijklmnopqrstuvwxyz"

// appPasswordLen is the number of random characters, before grouping. 16 of a
// 26-letter alphabet is ~75 bits — far past the point where guessing is the
// weakest link, and it matches what users already expect from Apple/Google.
const appPasswordLen = 16

// appPasswordUsedAtThrottle is how stale last_used_at may get before it is
// worth a write. CalDAV clients authenticate on every request; without this,
// every calendar poll would also be an UPDATE.
const appPasswordUsedAtThrottle = 60 * 1000 // ms

// GenerateAppPassword returns a new secret in display form,
// "abcd-efgh-ijkl-mnop". The dashes are cosmetic — NormalizeAppPassword strips
// them, so a client that was given the grouped form and a client that was given
// the bare letters both authenticate.
func GenerateAppPassword() (string, error) {
	raw := make([]byte, appPasswordLen)

	// Rejection sampling. 256 is not a multiple of 26, so plain `b % 26` would
	// make the first six letters of the alphabet measurably more likely — a
	// small bias, but free to avoid.
	const limit = 256 - (256 % len(appPasswordAlphabet))
	buf := make([]byte, 1)
	for i := 0; i < appPasswordLen; {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("failed to read random bytes: %w", err)
		}
		if int(buf[0]) >= limit {
			continue
		}
		raw[i] = appPasswordAlphabet[int(buf[0])%len(appPasswordAlphabet)]
		i++
	}

	var sb strings.Builder
	for i, c := range raw {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(byte(c))
	}
	return sb.String(), nil
}

// NormalizeAppPassword reduces a secret to the form that gets hashed: no
// dashes, no spaces, lowercase. Clients and humans re-type these, and a
// credential that fails because someone kept the grouping dashes (or dropped
// them) would be indistinguishable from a wrong password.
func NormalizeAppPassword(secret string) string {
	var sb strings.Builder
	sb.Grow(len(secret))
	for _, r := range strings.ToLower(secret) {
		if r >= 'a' && r <= 'z' {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// hashAppPassword returns the hex SHA-256 of the normalised secret.
func hashAppPassword(secret string) string {
	sum := sha256.Sum256([]byte(NormalizeAppPassword(secret)))
	return hex.EncodeToString(sum[:])
}

// CreateAppPassword issues a new application password for a user and returns
// the stored record along with the plaintext secret.
//
// The secret is returned exactly once and never persisted: only its SHA-256
// reaches the database. Callers must show it to the user immediately and must
// not log it.
func (db *DB) CreateAppPassword(userID int64, label string) (*models.AppPassword, string, error) {
	secret, err := GenerateAppPassword()
	if err != nil {
		return nil, "", err
	}

	normalized := NormalizeAppPassword(secret)
	record := &models.AppPassword{
		UserID:    userID,
		Label:     strings.TrimSpace(label),
		Last4:     normalized[len(normalized)-4:],
		CreatedAt: timeutil.Now(),
	}

	query := `
		INSERT INTO app_passwords (user_id, label, token_sha256, last4, created_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`
	err = db.QueryRow(query, record.UserID, record.Label, hashAppPassword(secret),
		record.Last4, record.CreatedAt).Scan(&record.ID)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create app password: %w", err)
	}

	return record, secret, nil
}

// VerifyAppPassword reports whether secret is a live application password for
// this user, and stamps last_used_at when it is.
//
// This is a protocol-only credential: IMAP, SMTP, CalDAV and CardDAV call it,
// the web and desktop logins deliberately do not. See migrations/047.
func (db *DB) VerifyAppPassword(userID int64, secret string) (bool, error) {
	// A short or empty secret can never be one of ours; skip the query rather
	// than let an empty Basic Auth header probe the table.
	if len(NormalizeAppPassword(secret)) != appPasswordLen {
		return false, nil
	}

	var id int64
	var storedHash string
	var lastUsedAt int64

	query := `
		SELECT id, token_sha256, last_used_at
		FROM app_passwords
		WHERE user_id = $1 AND token_sha256 = $2 AND revoked_at IS NULL
	`
	err := db.QueryRow(query, userID, hashAppPassword(secret)).Scan(&id, &storedHash, &lastUsedAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to verify app password: %w", err)
	}

	// The index lookup already matched, so this only guards against a
	// same-length hash arriving by some other path. Constant-time on principle:
	// this is a credential comparison and it costs nothing here.
	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(hashAppPassword(secret))) != 1 {
		return false, nil
	}

	now := timeutil.Now()
	if now-lastUsedAt > appPasswordUsedAtThrottle {
		if _, err := db.Exec(`UPDATE app_passwords SET last_used_at = $1 WHERE id = $2`, now, id); err != nil {
			// Bookkeeping only — a failed stamp must not fail the login.
			return true, nil
		}
	}

	return true, nil
}

// ListAppPasswords returns a user's application passwords, newest first,
// including revoked ones so the UI can show that a credential once existed.
func (db *DB) ListAppPasswords(userID int64) ([]*models.AppPassword, error) {
	query := `
		SELECT id, user_id, label, last4, created_at, last_used_at, revoked_at
		FROM app_passwords
		WHERE user_id = $1
		ORDER BY created_at DESC, id DESC
	`

	rows, err := db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to list app passwords: %w", err)
	}
	defer rows.Close()

	var out []*models.AppPassword
	for rows.Next() {
		p := &models.AppPassword{}
		var revokedAt sql.NullInt64
		if err := rows.Scan(&p.ID, &p.UserID, &p.Label, &p.Last4,
			&p.CreatedAt, &p.LastUsedAt, &revokedAt); err != nil {
			return nil, fmt.Errorf("failed to scan app password: %w", err)
		}
		if revokedAt.Valid {
			v := revokedAt.Int64
			p.RevokedAt = &v
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate app passwords: %w", err)
	}

	return out, nil
}

// RevokeAppPassword withdraws one credential. Scoped by user_id so an id from
// another account cannot be revoked by guessing the number.
func (db *DB) RevokeAppPassword(userID, id int64) error {
	res, err := db.Exec(
		`UPDATE app_passwords SET revoked_at = $1 WHERE id = $2 AND user_id = $3 AND revoked_at IS NULL`,
		timeutil.Now(), id, userID,
	)
	if err != nil {
		return fmt.Errorf("failed to revoke app password: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check revoke result: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("app password %d not found", id)
	}

	return nil
}

// RevokeAppPasswordsByLabel withdraws every live credential carrying this
// label. Used when a device profile is re-exported: the old profile's password
// is replaced rather than left behind on a phone nobody remembers handing back.
func (db *DB) RevokeAppPasswordsByLabel(userID int64, label string) (int64, error) {
	res, err := db.Exec(
		`UPDATE app_passwords SET revoked_at = $1 WHERE user_id = $2 AND label = $3 AND revoked_at IS NULL`,
		timeutil.Now(), userID, strings.TrimSpace(label),
	)
	if err != nil {
		return 0, fmt.Errorf("failed to revoke app passwords by label: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to check revoke result: %w", err)
	}

	return affected, nil
}
