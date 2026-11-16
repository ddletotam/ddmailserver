package db

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// Registration modes (system setting "registration_mode").
const (
	RegistrationOpen      = "open"      // anyone signs up and signs in at once
	RegistrationClosed    = "closed"    // only by an administrator's invitation
	RegistrationModerated = "moderated" // anyone signs up, signs in after approval

	SettingRegistrationMode = "registration_mode"
)

// ValidRegistrationMode reports whether m is one of the three modes.
func ValidRegistrationMode(m string) bool {
	return m == RegistrationOpen || m == RegistrationClosed || m == RegistrationModerated
}

// Invite is an administrator's one-time sign-up link.
type Invite struct {
	ID        int64
	Note      string
	CreatedBy int64
	CreatedAt int64
	ExpiresAt int64
	UsedAt    int64 // 0 — not used
	UsedBy    int64
}

// ErrInviteInvalid: unknown, expired, or already used.
var ErrInviteInvalid = errors.New("invitation is not valid")

func inviteHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateInvite stores a new invitation and returns its token — shown to the
// administrator once; only the hash is kept.
func (db *DB) CreateInvite(createdBy int64, note string, ttlMs int64) (string, *Invite, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("invite token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := timeutil.Now()
	inv := &Invite{Note: note, CreatedBy: createdBy, CreatedAt: now, ExpiresAt: now + ttlMs}
	err := db.QueryRow(`
		INSERT INTO user_invites (token_hash, note, created_by, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		inviteHash(token), note, createdBy, now, inv.ExpiresAt).Scan(&inv.ID)
	if err != nil {
		return "", nil, fmt.Errorf("create invite: %w", err)
	}
	return token, inv, nil
}

// InviteByToken returns a usable invitation (not expired, not used).
func (db *DB) InviteByToken(token string) (*Invite, error) {
	inv := &Invite{}
	var createdBy sql.NullInt64
	err := db.QueryRow(`
		SELECT id, note, created_by, created_at, expires_at FROM user_invites
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2`,
		inviteHash(token), timeutil.Now()).Scan(&inv.ID, &inv.Note, &createdBy, &inv.CreatedAt, &inv.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInviteInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("invite lookup: %w", err)
	}
	inv.CreatedBy = createdBy.Int64
	return inv, nil
}

// UseInvite marks the invitation used by userID; false if someone used it
// first (two people opening the same link).
func (db *DB) UseInvite(id, userID int64) (bool, error) {
	res, err := db.Exec(`UPDATE user_invites SET used_at = $1, used_by = $2 WHERE id = $3 AND used_at IS NULL`,
		timeutil.Now(), userID, id)
	if err != nil {
		return false, fmt.Errorf("use invite: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ListActiveInvites: not used and not expired, newest first.
func (db *DB) ListActiveInvites() ([]Invite, error) {
	rows, err := db.Query(`
		SELECT id, note, COALESCE(created_by, 0), created_at, expires_at FROM user_invites
		WHERE used_at IS NULL AND expires_at > $1 ORDER BY created_at DESC`, timeutil.Now())
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var inv Invite
		if err := rows.Scan(&inv.ID, &inv.Note, &inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan invite: %w", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// RevokeInvite deletes an unused invitation.
func (db *DB) RevokeInvite(id int64) error {
	if _, err := db.Exec(`DELETE FROM user_invites WHERE id = $1 AND used_at IS NULL`, id); err != nil {
		return fmt.Errorf("revoke invite: %w", err)
	}
	return nil
}
