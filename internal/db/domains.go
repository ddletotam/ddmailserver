package db

import (
	"database/sql"
	"fmt"

	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// CreateDomain creates a new domain
func (db *DB) CreateDomain(domain *models.Domain) error {
	// created_at is passed explicitly: the column has no DB-side default, so
	// omitting it stores NULL — which then breaks every SELECT that scans
	// created_at into int64 (GetAllDomains feeds the MX server).
	domain.CreatedAt = timeutil.Now()

	query := `
		INSERT INTO domains (domain, user_id, enabled, created_at)
		VALUES ($1, $2, $3, $4)
		RETURNING id`

	err := db.QueryRow(
		query,
		domain.Domain,
		domain.UserID,
		domain.Enabled,
		domain.CreatedAt,
	).Scan(&domain.ID)

	if err != nil {
		return fmt.Errorf("failed to create domain: %w", err)
	}

	return nil
}

// GetDomainByID returns a domain by ID
func (db *DB) GetDomainByID(id int64) (*models.Domain, error) {
	query := `
		SELECT id, domain, user_id, enabled, COALESCE(created_at, 0)
		FROM domains
		WHERE id = $1`

	domain := &models.Domain{}
	err := db.QueryRow(query, id).Scan(
		&domain.ID,
		&domain.Domain,
		&domain.UserID,
		&domain.Enabled,
		&domain.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("domain not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get domain: %w", err)
	}

	return domain, nil
}

// GetDomainByName returns a domain by domain name
func (db *DB) GetDomainByName(domainName string) (*models.Domain, error) {
	query := `
		SELECT id, domain, user_id, enabled, COALESCE(created_at, 0)
		FROM domains
		WHERE domain = $1`

	domain := &models.Domain{}
	err := db.QueryRow(query, domainName).Scan(
		&domain.ID,
		&domain.Domain,
		&domain.UserID,
		&domain.Enabled,
		&domain.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("domain not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get domain: %w", err)
	}

	return domain, nil
}

// GetDomainsByUserID returns all domains for a user
func (db *DB) GetDomainsByUserID(userID int64) ([]*models.Domain, error) {
	query := `
		SELECT id, domain, user_id, enabled, COALESCE(created_at, 0)
		FROM domains
		WHERE user_id = $1
		ORDER BY domain`

	rows, err := db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to query domains: %w", err)
	}
	defer rows.Close()

	var domains []*models.Domain
	for rows.Next() {
		domain := &models.Domain{}
		err := rows.Scan(
			&domain.ID,
			&domain.Domain,
			&domain.UserID,
			&domain.Enabled,
			&domain.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan domain: %w", err)
		}
		domains = append(domains, domain)
	}

	return domains, nil
}

// GetAllDomains returns all enabled domains (for MX server)
func (db *DB) GetAllDomains() ([]*models.Domain, error) {
	query := `
		SELECT id, domain, user_id, enabled, COALESCE(created_at, 0)
		FROM domains
		WHERE enabled = true
		ORDER BY domain`

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to query domains: %w", err)
	}
	defer rows.Close()

	var domains []*models.Domain
	for rows.Next() {
		domain := &models.Domain{}
		err := rows.Scan(
			&domain.ID,
			&domain.Domain,
			&domain.UserID,
			&domain.Enabled,
			&domain.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan domain: %w", err)
		}
		domains = append(domains, domain)
	}

	return domains, nil
}

// UpdateDomain updates a domain
func (db *DB) UpdateDomain(domain *models.Domain) error {
	query := `
		UPDATE domains
		SET domain = $1, enabled = $2
		WHERE id = $3 AND user_id = $4`

	result, err := db.Exec(query, domain.Domain, domain.Enabled, domain.ID, domain.UserID)
	if err != nil {
		return fmt.Errorf("failed to update domain: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("domain not found or access denied")
	}

	return nil
}

// DeleteDomain deletes a domain
func (db *DB) DeleteDomain(id, userID int64) error {
	query := `DELETE FROM domains WHERE id = $1 AND user_id = $2`

	result, err := db.Exec(query, id, userID)
	if err != nil {
		return fmt.Errorf("failed to delete domain: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("domain not found or access denied")
	}

	return nil
}
