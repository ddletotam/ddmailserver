package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
	"github.com/lib/pq"
)

// ErrDomainNotFound is returned by the domain lookups when no such domain is
// configured — as opposed to a database failure, which callers must not
// mistake for "not ours" (outbound routing decides local vs MX on it).
var ErrDomainNotFound = errors.New("domain not found")

// ErrDomainTaken means the domain is already verified (by its owner or for
// another user), or this user already has it — it cannot be added again.
var ErrDomainTaken = errors.New("domain already exists")

// domainColumns is the SELECT list scanned by scanDomain.
const domainColumns = `id, domain, user_id, enabled, COALESCE(created_at, 0),
	verified, verify_token, COALESCE(verified_at, 0), COALESCE(verified_method, '')`

func scanDomain(row interface{ Scan(...any) error }) (*models.Domain, error) {
	d := &models.Domain{}
	err := row.Scan(&d.ID, &d.Domain, &d.UserID, &d.Enabled, &d.CreatedAt,
		&d.Verified, &d.VerifyToken, &d.VerifiedAt, &d.VerifiedMethod)
	return d, err
}

// NewDomainVerifyToken returns a fresh random token for the TXT record.
func NewDomainVerifyToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate domain token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// CreateDomain creates a new domain. It is stored as given: Verified is the
// caller's decision (true only for an administrator adding it, or tests);
// a user's own claim starts unverified and is proven with VerifyDomain.
// A missing VerifyToken is generated. ErrDomainTaken if the name is already
// verified (for anyone) or this user already has it.
func (db *DB) CreateDomain(domain *models.Domain) error {
	// created_at is passed explicitly: the column has no DB-side default, so
	// omitting it stores NULL — which then breaks every SELECT that scans
	// created_at into int64 (GetAllDomains feeds the MX server).
	domain.CreatedAt = timeutil.Now()
	if domain.VerifyToken == "" {
		token, err := NewDomainVerifyToken()
		if err != nil {
			return err
		}
		domain.VerifyToken = token
	}
	var verifiedAt, verifiedMethod any
	if domain.Verified {
		if domain.VerifiedMethod == "" {
			domain.VerifiedMethod = models.DomainVerifiedAdmin
		}
		domain.VerifiedAt = domain.CreatedAt
		verifiedAt, verifiedMethod = domain.VerifiedAt, domain.VerifiedMethod
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to create domain: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var taken bool
	if err := tx.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM domains WHERE domain = $1 AND (verified OR user_id = $2))`,
		domain.Domain, domain.UserID,
	).Scan(&taken); err != nil {
		return fmt.Errorf("failed to create domain: %w", err)
	}
	if taken {
		return ErrDomainTaken
	}
	if domain.Verified {
		// Pending claims by others are moot once the domain is verified.
		if _, err := tx.Exec(`DELETE FROM domains WHERE domain = $1 AND NOT verified`, domain.Domain); err != nil {
			return fmt.Errorf("failed to create domain: %w", err)
		}
	}

	err = tx.QueryRow(`
		INSERT INTO domains (domain, user_id, enabled, created_at, verified, verify_token, verified_at, verified_method)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`,
		domain.Domain, domain.UserID, domain.Enabled, domain.CreatedAt,
		domain.Verified, domain.VerifyToken, verifiedAt, verifiedMethod,
	).Scan(&domain.ID)
	if isUniqueViolation(err) {
		return ErrDomainTaken
	}
	if err != nil {
		return fmt.Errorf("failed to create domain: %w", err)
	}
	if err := tx.Commit(); err != nil {
		if isUniqueViolation(err) {
			return ErrDomainTaken
		}
		return fmt.Errorf("failed to create domain: %w", err)
	}
	return nil
}

// VerifyDomain marks the user's domain verified by method and drops other
// users' pending claims to the same name. ErrDomainTaken if somebody else
// verified it first; ErrDomainNotFound if the user has no such domain.
func (db *DB) VerifyDomain(id, userID int64, method string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to verify domain: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var name string
	var verified bool
	err = tx.QueryRow(`SELECT domain, verified FROM domains WHERE id = $1 AND user_id = $2 FOR UPDATE`, id, userID).
		Scan(&name, &verified)
	if err == sql.ErrNoRows {
		return ErrDomainNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to verify domain: %w", err)
	}
	if verified {
		return nil
	}
	if _, err := tx.Exec(`DELETE FROM domains WHERE domain = $1 AND id <> $2 AND NOT verified`, name, id); err != nil {
		return fmt.Errorf("failed to verify domain: %w", err)
	}
	_, err = tx.Exec(`UPDATE domains SET verified = TRUE, verified_at = $1, verified_method = $2 WHERE id = $3`,
		timeutil.Now(), method, id)
	if isUniqueViolation(err) {
		return ErrDomainTaken
	}
	if err != nil {
		return fmt.Errorf("failed to verify domain: %w", err)
	}
	if err := tx.Commit(); err != nil {
		if isUniqueViolation(err) {
			return ErrDomainTaken
		}
		return fmt.Errorf("failed to verify domain: %w", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

// GetDomainByID returns a domain by ID (verified or not).
func (db *DB) GetDomainByID(id int64) (*models.Domain, error) {
	domain, err := scanDomain(db.QueryRow(`SELECT `+domainColumns+` FROM domains WHERE id = $1`, id))
	if err == sql.ErrNoRows {
		return nil, ErrDomainNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get domain: %w", err)
	}
	return domain, nil
}

// GetDomainByName returns the domain this server hosts under that name —
// the verified one. Unverified claims are invisible here: this lookup decides
// local delivery vs MX, MX acceptance and the submission RCPT check, and an
// unproven claim must not change any of them.
func (db *DB) GetDomainByName(domainName string) (*models.Domain, error) {
	domain, err := scanDomain(db.QueryRow(
		`SELECT `+domainColumns+` FROM domains WHERE domain = $1 AND verified`, domainName))
	if err == sql.ErrNoRows {
		return nil, ErrDomainNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get domain: %w", err)
	}
	return domain, nil
}

// GetDomainsByUserID returns all domains for a user, verified or pending.
func (db *DB) GetDomainsByUserID(userID int64) ([]*models.Domain, error) {
	return db.queryDomains(`SELECT `+domainColumns+` FROM domains WHERE user_id = $1 ORDER BY domain`, userID)
}

// GetAllDomains returns all enabled, verified domains (for MX server)
func (db *DB) GetAllDomains() ([]*models.Domain, error) {
	return db.queryDomains(`SELECT ` + domainColumns + ` FROM domains WHERE enabled = true AND verified ORDER BY domain`)
}

func (db *DB) queryDomains(query string, args ...any) ([]*models.Domain, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query domains: %w", err)
	}
	defer rows.Close()

	var domains []*models.Domain
	for rows.Next() {
		domain, err := scanDomain(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan domain: %w", err)
		}
		domains = append(domains, domain)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to query domains: %w", err)
	}
	return domains, nil
}

// UpdateDomain updates a domain's enabled flag. The name and the verification
// state are not changed here.
func (db *DB) UpdateDomain(domain *models.Domain) error {
	query := `
		UPDATE domains
		SET enabled = $1
		WHERE id = $2 AND user_id = $3`

	result, err := db.Exec(query, domain.Enabled, domain.ID, domain.UserID)
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
