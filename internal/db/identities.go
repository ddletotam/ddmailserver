package db

import (
	"database/sql"
	"fmt"
)

// DefaultWriteCalendarID returns the calendar a NEW event should land in when
// the client creates it "under" an identity. Preference: a local calendar of
// that identity, else its oldest writable calendar. Returns 0 (no error) when
// the identity has no writable calendar — the caller turns that into a 403.
func (db *DB) DefaultWriteCalendarID(userID int64, identityEmail string) (int64, error) {
	query := `
		SELECT c.id
		FROM calendars c JOIN calendar_sources s ON c.source_id = s.id
		WHERE c.user_id = $1 AND s.identity_email = $2 AND c.can_write = true
		ORDER BY (s.source_type = 'local') DESC, c.created_at ASC
		LIMIT 1
	`
	var id int64
	err := db.QueryRow(query, userID, identityEmail).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to resolve write calendar for %s: %w", identityEmail, err)
	}
	return id, nil
}

// WritableCalendarIdentities returns the set of identity emails (for a user)
// that can host a NEW event: an identity owning at least one local calendar
// source, or a CalDAV source with at least one writable calendar. Used by
// /identities to tell the client which "create under…" options to offer.
func (db *DB) WritableCalendarIdentities(userID int64) (map[string]bool, error) {
	query := `
		SELECT DISTINCT cs.identity_email
		FROM calendar_sources cs
		WHERE cs.user_id = $1 AND COALESCE(cs.identity_email, '') <> '' AND (
			cs.source_type = 'local'
			OR EXISTS (SELECT 1 FROM calendars c WHERE c.source_id = cs.id AND c.can_write = true)
		)
	`
	return db.identitySet(query, userID)
}

// WritableContactIdentities is the address-book analogue: identities that can
// host a NEW contact.
func (db *DB) WritableContactIdentities(userID int64) (map[string]bool, error) {
	query := `
		SELECT DISTINCT cs.identity_email
		FROM contact_sources cs
		WHERE cs.user_id = $1 AND COALESCE(cs.identity_email, '') <> '' AND (
			cs.source_type = 'local'
			OR EXISTS (SELECT 1 FROM address_books ab WHERE ab.source_id = cs.id AND ab.can_write = true)
		)
	`
	return db.identitySet(query, userID)
}

// DefaultWriteAddressBookID returns the address book a NEW contact should land
// in for the given identity: a local book preferred, else the oldest writable
// one. 0 (no error) when the identity has no writable book.
func (db *DB) DefaultWriteAddressBookID(userID int64, identityEmail string) (int64, error) {
	query := `
		SELECT ab.id
		FROM address_books ab JOIN contact_sources cs ON ab.source_id = cs.id
		WHERE ab.user_id = $1 AND cs.identity_email = $2 AND ab.can_write = true
		ORDER BY (cs.source_type = 'local') DESC, ab.id ASC
		LIMIT 1
	`
	var id int64
	err := db.QueryRow(query, userID, identityEmail).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to resolve write address book for %s: %w", identityEmail, err)
	}
	return id, nil
}

func (db *DB) identitySet(query string, userID int64) (map[string]bool, error) {
	rows, err := db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to query writable identities: %w", err)
	}
	defer rows.Close()
	set := make(map[string]bool)
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, fmt.Errorf("failed to scan identity: %w", err)
		}
		set[email] = true
	}
	return set, nil
}

// DefaultIdentityEmail resolves a user's default identity (email address),
// used to satisfy the "no orphan sources" invariant when a caller creates a
// source without specifying one. The fallback chain mirrors migration 045 and
// the /identities handler: first local mailbox, else first external account
// with an email, else the username.
func (db *DB) DefaultIdentityEmail(userID int64) (string, error) {
	query := `
		SELECT COALESCE(
			(SELECT m.local_part || '@' || d.domain
			   FROM mailboxes m JOIN domains d ON m.domain_id = d.id
			   WHERE m.user_id = $1 ORDER BY m.id LIMIT 1),
			(SELECT a.email FROM accounts a
			   WHERE a.user_id = $1 AND COALESCE(a.email, '') <> '' ORDER BY a.id LIMIT 1),
			(SELECT u.username FROM users u WHERE u.id = $1),
			''
		)
	`
	var email string
	if err := db.QueryRow(query, userID).Scan(&email); err != nil {
		return "", fmt.Errorf("failed to resolve default identity for user %d: %w", userID, err)
	}
	return email, nil
}
