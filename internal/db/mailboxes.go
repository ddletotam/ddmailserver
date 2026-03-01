package db

import (
	"database/sql"
	"fmt"

	"github.com/yourusername/mailserver/internal/models"
)

// CreateMailbox creates a new mailbox
func (db *DB) CreateMailbox(mailbox *models.Mailbox) error {
	query := `
		INSERT INTO mailboxes (user_id, domain_id, local_part, enabled)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at`

	err := db.QueryRow(
		query,
		mailbox.UserID,
		mailbox.DomainID,
		mailbox.LocalPart,
		mailbox.Enabled,
	).Scan(&mailbox.ID, &mailbox.CreatedAt)

	if err != nil {
		return fmt.Errorf("failed to create mailbox: %w", err)
	}

	return nil
}

// GetMailboxByID returns a mailbox by ID
func (db *DB) GetMailboxByID(id int64) (*models.Mailbox, error) {
	query := `
		SELECT id, user_id, domain_id, local_part, enabled, created_at
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
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("mailbox not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get mailbox: %w", err)
	}

	return mailbox, nil
}

// GetMailbox returns a mailbox by domain ID and local part (for MX server)
func (db *DB) GetMailbox(domainID int64, localPart string) (*models.Mailbox, error) {
	query := `
		SELECT id, user_id, domain_id, local_part, enabled, created_at
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
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("mailbox not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get mailbox: %w", err)
	}

	return mailbox, nil
}

// GetMailboxesByUserID returns all mailboxes for a user
func (db *DB) GetMailboxesByUserID(userID int64) ([]*models.Mailbox, error) {
	query := `
		SELECT id, user_id, domain_id, local_part, enabled, created_at
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
		SELECT id, user_id, domain_id, local_part, enabled, created_at
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
	DomainName string `json:"domain_name"`
}

// GetMailboxesWithDomainByUserID returns mailboxes with domain names
func (db *DB) GetMailboxesWithDomainByUserID(userID int64) ([]*MailboxWithDomain, error) {
	query := `
		SELECT m.id, m.user_id, m.domain_id, m.local_part, m.enabled, m.created_at, d.domain
		FROM mailboxes m
		JOIN domains d ON m.domain_id = d.id
		WHERE m.user_id = $1
		ORDER BY d.domain, m.local_part`

	rows, err := db.Query(query, userID)
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
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan mailbox: %w", err)
		}
		mailboxes = append(mailboxes, m)
	}

	return mailboxes, nil
}
