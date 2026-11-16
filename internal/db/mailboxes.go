package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// ErrMailboxNotFound is returned by the mailbox lookups when the mailbox does
// not exist, distinct from a database failure (550 vs 451 at RCPT).
var ErrMailboxNotFound = errors.New("mailbox not found")

// CreateMailbox creates a new mailbox
func (db *DB) CreateMailbox(mailbox *models.Mailbox) error {
	// created_at is passed explicitly: the column has no DB-side default, so
	// omitting it stores NULL — which then breaks every SELECT that scans
	// created_at into int64 (GetMailbox decides MX recipient acceptance).
	mailbox.CreatedAt = timeutil.Now()

	query := `
		INSERT INTO mailboxes (user_id, domain_id, local_part, enabled, created_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`

	err := db.QueryRow(
		query,
		mailbox.UserID,
		mailbox.DomainID,
		mailbox.LocalPart,
		mailbox.Enabled,
		mailbox.CreatedAt,
	).Scan(&mailbox.ID)

	if err != nil {
		return fmt.Errorf("failed to create mailbox: %w", err)
	}

	return nil
}

// GetMailboxByID returns a mailbox by ID
func (db *DB) GetMailboxByID(id int64) (*models.Mailbox, error) {
	query := `
		SELECT id, user_id, domain_id, local_part, enabled, COALESCE(created_at, 0), COALESCE(principal_id, 0)
		FROM mailboxes
		WHERE id = $1`

	mailbox := &models.Mailbox{}
	err := db.QueryRow(query, id).Scan(
		&mailbox.ID,
		&mailbox.UserID,
		&mailbox.DomainID,
		&mailbox.LocalPart,
		&mailbox.Enabled,
		&mailbox.CreatedAt,
		&mailbox.PrincipalID,
	)

	if err == sql.ErrNoRows {
		return nil, ErrMailboxNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get mailbox: %w", err)
	}

	return mailbox, nil
}

// GetMailbox returns a mailbox by domain ID and local part (for MX server)
func (db *DB) GetMailbox(domainID int64, localPart string) (*models.Mailbox, error) {
	query := `
		SELECT id, user_id, domain_id, local_part, enabled, COALESCE(created_at, 0), COALESCE(principal_id, 0)
		FROM mailboxes
		WHERE domain_id = $1 AND local_part = $2`

	mailbox := &models.Mailbox{}
	err := db.QueryRow(query, domainID, localPart).Scan(
		&mailbox.ID,
		&mailbox.UserID,
		&mailbox.DomainID,
		&mailbox.LocalPart,
		&mailbox.Enabled,
		&mailbox.CreatedAt,
		&mailbox.PrincipalID,
	)

	if err == sql.ErrNoRows {
		return nil, ErrMailboxNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get mailbox: %w", err)
	}

	return mailbox, nil
}

// GetMailboxesByUserID returns all mailboxes for a user
func (db *DB) GetMailboxesByUserID(userID int64) ([]*models.Mailbox, error) {
	query := `
		SELECT id, user_id, domain_id, local_part, enabled, COALESCE(created_at, 0)
		FROM mailboxes
		WHERE user_id = $1
		ORDER BY local_part`

	rows, err := db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to query mailboxes: %w", err)
	}
	defer rows.Close()

	var mailboxes []*models.Mailbox
	for rows.Next() {
		mailbox := &models.Mailbox{}
		err := rows.Scan(
			&mailbox.ID,
			&mailbox.UserID,
			&mailbox.DomainID,
			&mailbox.LocalPart,
			&mailbox.Enabled,
			&mailbox.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan mailbox: %w", err)
		}
		mailboxes = append(mailboxes, mailbox)
	}

	return mailboxes, nil
}

// GetMailboxesByDomainID returns all mailboxes for a domain
func (db *DB) GetMailboxesByDomainID(domainID int64) ([]*models.Mailbox, error) {
	query := `
		SELECT id, user_id, domain_id, local_part, enabled, COALESCE(created_at, 0)
		FROM mailboxes
		WHERE domain_id = $1
		ORDER BY local_part`

	rows, err := db.Query(query, domainID)
	if err != nil {
		return nil, fmt.Errorf("failed to query mailboxes: %w", err)
	}
	defer rows.Close()

	var mailboxes []*models.Mailbox
	for rows.Next() {
		mailbox := &models.Mailbox{}
		err := rows.Scan(
			&mailbox.ID,
			&mailbox.UserID,
			&mailbox.DomainID,
			&mailbox.LocalPart,
			&mailbox.Enabled,
			&mailbox.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan mailbox: %w", err)
		}
		mailboxes = append(mailboxes, mailbox)
	}

	return mailboxes, nil
}

// UpdateMailbox updates a mailbox
func (db *DB) UpdateMailbox(mailbox *models.Mailbox) error {
	query := `
		UPDATE mailboxes
		SET local_part = $1, enabled = $2
		WHERE id = $3 AND user_id = $4`

	result, err := db.Exec(query, mailbox.LocalPart, mailbox.Enabled, mailbox.ID, mailbox.UserID)
	if err != nil {
		return fmt.Errorf("failed to update mailbox: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("mailbox not found or access denied")
	}

	return nil
}

// DeleteMailbox deletes a mailbox
func (db *DB) DeleteMailbox(id, userID int64) error {
	query := `DELETE FROM mailboxes WHERE id = $1 AND user_id = $2`

	result, err := db.Exec(query, id, userID)
	if err != nil {
		return fmt.Errorf("failed to delete mailbox: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("mailbox not found or access denied")
	}

	return nil
}

// MailboxWithDomain is a helper struct that includes domain info
type MailboxWithDomain struct {
	models.Mailbox
	DomainName     string `json:"domain_name"`
	DomainVerified bool   `json:"domain_verified"`
}

// A mailbox that has its own account (principal) is that account's identity,
// not the owner's — the owner reaches it as an ordinary connected IMAP
// account; its UserID is reported as the principal so ownership checks hold.
//
// GetMailboxesWithDomainByUserID returns the user's mailboxes on verified
// domains — the addresses the user actually owns. Every identity consumer
// (sender ownership, desktop /identities, IMAP METADATA, "is this me?")
// reads this, so a mailbox on an unverified domain is never offered or
// accepted as a sender.
func (db *DB) GetMailboxesWithDomainByUserID(userID int64) ([]*MailboxWithDomain, error) {
	return db.queryMailboxesWithDomain(userID, true)
}

// GetAllMailboxesWithDomainByUserID is GetMailboxesWithDomainByUserID
// including mailboxes on unverified domains (the domains management page).
func (db *DB) GetAllMailboxesWithDomainByUserID(userID int64) ([]*MailboxWithDomain, error) {
	return db.queryMailboxesWithDomain(userID, false)
}

func (db *DB) queryMailboxesWithDomain(userID int64, verifiedOnly bool) ([]*MailboxWithDomain, error) {
	query := `
		SELECT m.id, CASE WHEN $2 THEN COALESCE(m.principal_id, m.user_id) ELSE m.user_id END,
		       m.domain_id, m.local_part, m.enabled, COALESCE(m.created_at, 0), d.domain, d.verified
		FROM mailboxes m
		JOIN domains d ON m.domain_id = d.id
		WHERE (CASE WHEN $2 THEN (m.user_id = $1 AND m.principal_id IS NULL) OR m.principal_id = $1
		            ELSE m.user_id = $1 END)
		  AND (d.verified OR NOT $2)
		ORDER BY d.domain, m.local_part`

	rows, err := db.Query(query, userID, verifiedOnly)
	if err != nil {
		return nil, fmt.Errorf("failed to query mailboxes: %w", err)
	}
	defer rows.Close()

	var mailboxes []*MailboxWithDomain
	for rows.Next() {
		m := &MailboxWithDomain{}
		err := rows.Scan(
			&m.ID,
			&m.UserID,
			&m.DomainID,
			&m.LocalPart,
			&m.Enabled,
			&m.CreatedAt,
			&m.DomainName,
			&m.DomainVerified,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan mailbox: %w", err)
		}
		mailboxes = append(mailboxes, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to query mailboxes: %w", err)
	}

	return mailboxes, nil
}
