package db

import (
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// SetLunlunLink records the lunlun.io account of a user.
func (db *DB) SetLunlunLink(userID, lunlunID int64) error {
	_, err := db.Exec(`INSERT INTO lunlun_links (user_id, lunlun_id, created_at) VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET lunlun_id = EXCLUDED.lunlun_id`, userID, lunlunID, timeutil.Now())
	if err != nil {
		return fmt.Errorf("lunlun link of user %d: %w", userID, err)
	}
	return nil
}

// HasLunlunLink reports whether the user has a lunlun.io account.
func (db *DB) HasLunlunLink(userID int64) (bool, error) {
	var ok bool
	err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM lunlun_links WHERE user_id = $1)`, userID).Scan(&ok)
	return ok, err
}

// UnlinkedServiceUser is a bot sign-up without a lunlun.io account yet.
type UnlinkedServiceUser struct {
	UserID   int64
	Username string
}

// ServiceUsersWithoutLunlun lists bot sign-ups that have no lunlun.io account.
func (db *DB) ServiceUsersWithoutLunlun(limit int) ([]UnlinkedServiceUser, error) {
	rows, err := db.Query(`SELECT u.id, u.username FROM telegram_links l JOIN users u ON u.id = l.user_id
		WHERE NOT l.banned AND NOT EXISTS (SELECT 1 FROM lunlun_links k WHERE k.user_id = u.id) ORDER BY u.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnlinkedServiceUser
	for rows.Next() {
		var u UnlinkedServiceUser
		if err := rows.Scan(&u.UserID, &u.Username); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Space is a service user's space: the quota (0 — none) and what mail takes.
type Space struct {
	QuotaBytes int64 `json:"quota_bytes"`
	MailBytes  int64 `json:"mail_bytes"`
}

// SpaceOf returns the space of a user's account (resolved to its owner).
func (db *DB) SpaceOf(userID int64) (Space, error) {
	var sp Space
	owner, err := billingOwner(db, userID)
	if err != nil {
		return sp, err
	}
	if err := db.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM messages WHERE user_id IN `+ownedUsers, owner).Scan(&sp.MailBytes); err != nil {
		return sp, fmt.Errorf("mail of user %d: %w", owner, err)
	}
	since, err := freeTierSince(db, owner)
	if err != nil || since == 0 {
		return sp, err
	}
	quotaMB := int64(FreeQuotaMB)
	if paid, err := hasEntitlement(db, owner, FeatureExtraStorage); err != nil {
		return sp, err
	} else if paid {
		quotaMB += ExtraStorageMB
	}
	sp.QuotaBytes = quotaMB * 1024 * 1024
	return sp, nil
}
