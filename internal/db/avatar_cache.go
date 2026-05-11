package db

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/yourusername/mailserver/internal/timeutil"
)

// AvatarCacheEntry is a single row in the avatar_cache table.
type AvatarCacheEntry struct {
	Email     string
	Source    string // "carddav" | "libravatar" | "gravatar" | "bimi" | "favicon" | "none"
	Data      []byte // nil for negative cache
	MIME      string
	FetchedAt int64 // ms
	TTLMS     int64 // ms
}

// Expired reports whether the entry should be re-fetched.
func (e *AvatarCacheEntry) Expired() bool {
	return timeutil.Now() >= e.FetchedAt+e.TTLMS
}

// GetAvatar looks up a cached avatar (or negative-cache row) for the email.
// Returns (nil, nil) when no row exists — the caller should fetch fresh.
func (db *DB) GetAvatar(email string) (*AvatarCacheEntry, error) {
	row := db.QueryRow(`
		SELECT email, source, data, COALESCE(mime, ''), fetched_at, ttl_ms
		FROM avatar_cache
		WHERE email = $1
	`, strings.ToLower(strings.TrimSpace(email)))

	var e AvatarCacheEntry
	var data sql.RawBytes
	err := row.Scan(&e.Email, &e.Source, &data, &e.MIME, &e.FetchedAt, &e.TTLMS)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get avatar: %w", err)
	}
	if data != nil {
		e.Data = append([]byte(nil), data...) // RawBytes lifetime is per-row
	}
	return &e, nil
}

// PutAvatar upserts an avatar (or negative-cache row) for the email.
// Pass nil/empty data with source="none" to remember "no avatar found".
func (db *DB) PutAvatar(email, source string, data []byte, mime string, ttlMS int64) error {
	email = strings.ToLower(strings.TrimSpace(email))
	now := timeutil.Now()
	var dataArg interface{}
	if len(data) > 0 {
		dataArg = data
	}
	_, err := db.Exec(`
		INSERT INTO avatar_cache (email, source, data, mime, fetched_at, ttl_ms)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (email) DO UPDATE SET
			source = EXCLUDED.source,
			data = EXCLUDED.data,
			mime = EXCLUDED.mime,
			fetched_at = EXCLUDED.fetched_at,
			ttl_ms = EXCLUDED.ttl_ms
	`, email, source, dataArg, mime, now, ttlMS)
	if err != nil {
		return fmt.Errorf("put avatar: %w", err)
	}
	return nil
}
