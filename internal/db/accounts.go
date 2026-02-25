package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// CreateAccount creates a new external email account
func (db *DB) CreateAccount(account *models.Account) error {
	account.CreatedAt = time.Now()
	account.UpdatedAt = time.Now()

	query := `
		INSERT INTO accounts (
			user_id, name, email, imap_host, imap_port, imap_username, imap_password, imap_tls,
			smtp_host, smtp_port, smtp_username, smtp_password, smtp_tls, enabled, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		RETURNING id
	`

	err := db.QueryRow(
		query,
		account.UserID, account.Name, account.Email,
		account.IMAPHost, account.IMAPPort, account.IMAPUsername, account.IMAPPassword, account.IMAPTLS,
		account.SMTPHost, account.SMTPPort, account.SMTPUsername, account.SMTPPassword, account.SMTPTLS,
		account.Enabled, account.CreatedAt, account.UpdatedAt,
	).Scan(&account.ID)

	if err != nil {
		return fmt.Errorf("failed to create account: %w", err)
	}

	return nil
}

// GetAccountsByUserID retrieves all accounts for a user
func (db *DB) GetAccountsByUserID(userID int64) ([]*models.Account, error) {
	query := `
		SELECT id, user_id, name, email, imap_host, imap_port, imap_username, imap_password, imap_tls,
		       smtp_host, smtp_port, smtp_username, smtp_password, smtp_tls, enabled, last_sync, created_at, updated_at
		FROM accounts
		WHERE user_id = $1
		ORDER BY created_at DESC
	`

	rows, err := db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get accounts: %w", err)
	}
	defer rows.Close()

	var accounts []*models.Account
	for rows.Next() {
		account := &models.Account{}
		var lastSync sql.NullTime

		err := rows.Scan(
			&account.ID, &account.UserID, &account.Name, &account.Email,
			&account.IMAPHost, &account.IMAPPort, &account.IMAPUsername, &account.IMAPPassword, &account.IMAPTLS,
			&account.SMTPHost, &account.SMTPPort, &account.SMTPUsername, &account.SMTPPassword, &account.SMTPTLS,
			&account.Enabled, &lastSync, &account.CreatedAt, &account.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan account: %w", err)
		}

		if lastSync.Valid {
			account.LastSync = lastSync.Time
		}

		accounts = append(accounts, account)
	}

	return accounts, nil
}

// GetAccountByID retrieves an account by ID
func (db *DB) GetAccountByID(id int64) (*models.Account, error) {
	account := &models.Account{}
	var lastSync sql.NullTime

	query := `
		SELECT id, user_id, name, email, imap_host, imap_port, imap_username, imap_password, imap_tls,
		       smtp_host, smtp_port, smtp_username, smtp_password, smtp_tls, enabled, last_sync, created_at, updated_at
		FROM accounts
		WHERE id = $1
	`

	err := db.QueryRow(query, id).Scan(
		&account.ID, &account.UserID, &account.Name, &account.Email,
		&account.IMAPHost, &account.IMAPPort, &account.IMAPUsername, &account.IMAPPassword, &account.IMAPTLS,
		&account.SMTPHost, &account.SMTPPort, &account.SMTPUsername, &account.SMTPPassword, &account.SMTPTLS,
		&account.Enabled, &lastSync, &account.CreatedAt, &account.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("account not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get account: %w", err)
	}

	if lastSync.Valid {
		account.LastSync = lastSync.Time
	}

	return account, nil
}

// UpdateAccount updates an account
func (db *DB) UpdateAccount(account *models.Account) error {
	account.UpdatedAt = time.Now()

	query := `
		UPDATE accounts
		SET name = $1, email = $2, imap_host = $3, imap_port = $4, imap_username = $5, imap_password = $6, imap_tls = $7,
		    smtp_host = $8, smtp_port = $9, smtp_username = $10, smtp_password = $11, smtp_tls = $12,
		    enabled = $13, last_sync = $14, updated_at = $15
		WHERE id = $16
	`

	_, err := db.Exec(
		query,
		account.Name, account.Email,
		account.IMAPHost, account.IMAPPort, account.IMAPUsername, account.IMAPPassword, account.IMAPTLS,
		account.SMTPHost, account.SMTPPort, account.SMTPUsername, account.SMTPPassword, account.SMTPTLS,
		account.Enabled, account.LastSync, account.UpdatedAt, account.ID,
	)

	if err != nil {
		return fmt.Errorf("failed to update account: %w", err)
	}

	return nil
}

// DeleteAccount deletes an account
func (db *DB) DeleteAccount(id int64) error {
	query := `DELETE FROM accounts WHERE id = $1`
	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete account: %w", err)
	}
	return nil
}

// UpdateAccountLastSync updates the last sync time for an account
func (db *DB) UpdateAccountLastSync(accountID int64, lastSync time.Time) error {
	query := `UPDATE accounts SET last_sync = $1, updated_at = $2 WHERE id = $3`
	_, err := db.Exec(query, lastSync, time.Now(), accountID)
	if err != nil {
		return fmt.Errorf("failed to update last sync: %w", err)
	}
	return nil
}

// GetAllEnabledAccounts retrieves all enabled accounts across all users
func (db *DB) GetAllEnabledAccounts() ([]*models.Account, error) {
	query := `
		SELECT id, user_id, name, email, imap_host, imap_port, imap_username, imap_password, imap_tls,
		       smtp_host, smtp_port, smtp_username, smtp_password, smtp_tls, enabled, last_sync, created_at, updated_at
		FROM accounts
		WHERE enabled = true
		ORDER BY last_sync ASC NULLS FIRST
	`

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to get enabled accounts: %w", err)
	}
	defer rows.Close()

	var accounts []*models.Account
	for rows.Next() {
		account := &models.Account{}
		var lastSync sql.NullTime

		err := rows.Scan(
			&account.ID, &account.UserID, &account.Name, &account.Email,
			&account.IMAPHost, &account.IMAPPort, &account.IMAPUsername, &account.IMAPPassword, &account.IMAPTLS,
			&account.SMTPHost, &account.SMTPPort, &account.SMTPUsername, &account.SMTPPassword, &account.SMTPTLS,
			&account.Enabled, &lastSync, &account.CreatedAt, &account.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan account: %w", err)
		}

		if lastSync.Valid {
			account.LastSync = lastSync.Time
		}

		accounts = append(accounts, account)
	}

	return accounts, nil
}
