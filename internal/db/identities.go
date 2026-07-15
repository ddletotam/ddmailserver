package db

import "fmt"

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
