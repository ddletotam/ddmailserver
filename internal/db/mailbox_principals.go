package db

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// ErrMailboxHasAccount: the mailbox already has its own account.
var ErrMailboxHasAccount = errors.New("mailbox already has its own account")

// CreateMailboxPrincipal gives a hosted mailbox its own account
// (docs/mailbox-accounts-plan.md): a users row of kind 'mailbox', login =
// the full address, password = passwordHash (bcrypt). From then on the
// mailbox's new mail is stored under that account and IMAP/SMTP sign-in with
// the address sees only it.
func (db *DB) CreateMailboxPrincipal(mailboxID int64, address, passwordHash string) (*models.User, error) {
	address = strings.ToLower(strings.TrimSpace(address))
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var existing int64
	if err := tx.QueryRow(`SELECT COALESCE(principal_id, 0) FROM mailboxes WHERE id = $1 FOR UPDATE`, mailboxID).Scan(&existing); err != nil {
		return nil, fmt.Errorf("mailbox %d: %w", mailboxID, err)
	}
	if existing != 0 {
		return nil, ErrMailboxHasAccount
	}
	now := timeutil.Now()
	u := &models.User{Username: address, PasswordHash: passwordHash, Kind: "mailbox", CreatedAt: now, UpdatedAt: now}
	if err := tx.QueryRow(`
		INSERT INTO users (username, password_hash, recovery_key_hash, kind, created_at, updated_at)
		VALUES ($1, $2, '', 'mailbox', $3, $3) RETURNING id`, address, passwordHash, now).Scan(&u.ID); err != nil {
		return nil, fmt.Errorf("create mailbox account: %w", err)
	}
	if _, err := tx.Exec(`UPDATE mailboxes SET principal_id = $1 WHERE id = $2`, u.ID, mailboxID); err != nil {
		return nil, fmt.Errorf("link mailbox account: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	if err := db.EnsureDefaultFolders(u.ID); err != nil {
		return nil, fmt.Errorf("mailbox folders: %w", err)
	}
	return u, nil
}

// SetMailboxPassword changes the password of a mailbox's own account.
func (db *DB) SetMailboxPassword(principalID int64, passwordHash string) error {
	res, err := db.Exec(`UPDATE users SET password_hash = $1, updated_at = $2 WHERE id = $3 AND kind = 'mailbox'`,
		passwordHash, timeutil.Now(), principalID)
	if err != nil {
		return fmt.Errorf("set mailbox password: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("mailbox account %d not found", principalID)
	}
	return nil
}

// mailboxPrincipalActive: the mailbox of this account exists and both it and
// its domain are enabled — a disabled mailbox cannot sign in.
func (db *DB) mailboxPrincipalActive(principalID int64) (bool, error) {
	var ok bool
	err := db.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM mailboxes m JOIN domains d ON d.id = m.domain_id
		               WHERE m.principal_id = $1 AND m.enabled AND d.enabled AND d.verified)`, principalID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("mailbox account state: %w", err)
	}
	return ok, nil
}

// DeleteMailboxAndAccount removes a mailbox and, if it has one, its own
// account with all its mail (administrator action). Returns the account id
// (0 — none) so open sessions can be closed.
func (db *DB) DeleteMailboxAndAccount(mailboxID int64) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var principal int64
	if err := tx.QueryRow(`DELETE FROM mailboxes WHERE id = $1 RETURNING COALESCE(principal_id, 0)`, mailboxID).Scan(&principal); err != nil {
		return 0, fmt.Errorf("delete mailbox %d: %w", mailboxID, err)
	}
	if principal != 0 {
		if _, err := tx.Exec(`DELETE FROM users WHERE id = $1 AND kind = 'mailbox'`, principal); err != nil {
			return 0, fmt.Errorf("delete mailbox account: %w", err)
		}
	}
	return principal, tx.Commit()
}

// DeleteDomainAndAccounts removes a domain, its mailboxes and their own
// accounts with their mail. Returns the deleted account ids.
func (db *DB) DeleteDomainAndAccounts(domainID int64) ([]int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	rows, err := tx.Query(`SELECT principal_id FROM mailboxes WHERE domain_id = $1 AND principal_id IS NOT NULL`, domainID)
	if err != nil {
		return nil, fmt.Errorf("domain mailboxes: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if _, err := tx.Exec(`DELETE FROM domains WHERE id = $1`, domainID); err != nil {
		return nil, fmt.Errorf("delete domain: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM users WHERE id = $1 AND kind = 'mailbox'`, id); err != nil {
			return nil, fmt.Errorf("delete mailbox account: %w", err)
		}
	}
	return ids, tx.Commit()
}

// SetMailboxEnabled / SetDomainEnabled: a disabled mailbox or domain neither
// receives mail nor signs in.
func (db *DB) SetMailboxEnabled(id int64, enabled bool) (principal int64, err error) {
	err = db.QueryRow(`UPDATE mailboxes SET enabled = $1 WHERE id = $2 RETURNING COALESCE(principal_id, 0)`, enabled, id).Scan(&principal)
	if err != nil {
		return 0, fmt.Errorf("set mailbox enabled: %w", err)
	}
	return principal, nil
}

func (db *DB) SetDomainEnabled(id int64, enabled bool) error {
	if _, err := db.Exec(`UPDATE domains SET enabled = $1 WHERE id = $2`, enabled, id); err != nil {
		return fmt.Errorf("set domain enabled: %w", err)
	}
	return nil
}

// CountDomainsOwnedBy: domains whose owner row is userID (an administrator
// who owns domains cannot be deleted — the domains would cascade away).
func (db *DB) CountDomainsOwnedBy(userID int64) (int, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM domains WHERE user_id = $1`, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count domains: %w", err)
	}
	return n, nil
}
