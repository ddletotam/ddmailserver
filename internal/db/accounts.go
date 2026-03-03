package db

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/yourusername/mailserver/internal/crypto"
	"github.com/yourusername/mailserver/internal/models"
)

// CreateAccount creates a new external email account
func (db *DB) CreateAccount(account *models.Account) error {
	account.CreatedAt = time.Now()
	account.UpdatedAt = time.Now()

	// Set default auth type if not specified
	if account.AuthType == "" {
		account.AuthType = "password"
	}

	// Encrypt passwords before storing
	encryptedIMAPPassword, err := crypto.EncryptPassword(account.IMAPPassword, db.encryptionKey)
	if err != nil {
		return fmt.Errorf("failed to encrypt IMAP password: %w", err)
	}

	encryptedSMTPPassword, err := crypto.EncryptPassword(account.SMTPPassword, db.encryptionKey)
	if err != nil {
		return fmt.Errorf("failed to encrypt SMTP password: %w", err)
	}

	// Encrypt OAuth tokens if present
	var encryptedAccessToken, encryptedRefreshToken string
	if account.OAuthAccessToken != "" {
		encryptedAccessToken, err = crypto.EncryptPassword(account.OAuthAccessToken, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt OAuth access token: %w", err)
		}
	}
	if account.OAuthRefreshToken != "" {
		encryptedRefreshToken, err = crypto.EncryptPassword(account.OAuthRefreshToken, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt OAuth refresh token: %w", err)
		}
	}

	query := `
		INSERT INTO accounts (
			user_id, name, email, imap_host, imap_port, imap_username, imap_password, imap_tls,
			smtp_host, smtp_port, smtp_username, smtp_password, smtp_tls, enabled,
			auth_type, oauth_access_token, oauth_refresh_token, oauth_token_expiry,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
		RETURNING id
	`

	var tokenExpiry sql.NullTime
	if !account.OAuthTokenExpiry.IsZero() {
		tokenExpiry = sql.NullTime{Time: account.OAuthTokenExpiry, Valid: true}
	}

	err = db.QueryRow(
		query,
		account.UserID, account.Name, account.Email,
		account.IMAPHost, account.IMAPPort, account.IMAPUsername, encryptedIMAPPassword, account.IMAPTLS,
		account.SMTPHost, account.SMTPPort, account.SMTPUsername, encryptedSMTPPassword, account.SMTPTLS,
		account.Enabled, account.AuthType, encryptedAccessToken, encryptedRefreshToken, tokenExpiry,
		account.CreatedAt, account.UpdatedAt,
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
		       smtp_host, smtp_port, smtp_username, smtp_password, smtp_tls, enabled, last_sync,
		       COALESCE(auth_type, 'password'), COALESCE(oauth_access_token, ''), COALESCE(oauth_refresh_token, ''), oauth_token_expiry,
		       created_at, updated_at
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
		var lastSync, tokenExpiry sql.NullTime

		err := rows.Scan(
			&account.ID, &account.UserID, &account.Name, &account.Email,
			&account.IMAPHost, &account.IMAPPort, &account.IMAPUsername, &account.IMAPPassword, &account.IMAPTLS,
			&account.SMTPHost, &account.SMTPPort, &account.SMTPUsername, &account.SMTPPassword, &account.SMTPTLS,
			&account.Enabled, &lastSync,
			&account.AuthType, &account.OAuthAccessToken, &account.OAuthRefreshToken, &tokenExpiry,
			&account.CreatedAt, &account.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan account: %w", err)
		}

		if lastSync.Valid {
			account.LastSync = lastSync.Time
		}
		if tokenExpiry.Valid {
			account.OAuthTokenExpiry = tokenExpiry.Time
		}

		// Decrypt passwords and tokens
		if err := db.decryptAccountSecrets(account); err != nil {
			return nil, fmt.Errorf("failed to decrypt secrets for account %d: %w", account.ID, err)
		}

		accounts = append(accounts, account)
	}

	return accounts, nil
}

// GetAccountByID retrieves an account by ID
func (db *DB) GetAccountByID(id int64) (*models.Account, error) {
	account := &models.Account{}
	var lastSync, tokenExpiry sql.NullTime

	query := `
		SELECT id, user_id, name, email, imap_host, imap_port, imap_username, imap_password, imap_tls,
		       smtp_host, smtp_port, smtp_username, smtp_password, smtp_tls, enabled, last_sync,
		       COALESCE(auth_type, 'password'), COALESCE(oauth_access_token, ''), COALESCE(oauth_refresh_token, ''), oauth_token_expiry,
		       created_at, updated_at
		FROM accounts
		WHERE id = $1
	`

	err := db.QueryRow(query, id).Scan(
		&account.ID, &account.UserID, &account.Name, &account.Email,
		&account.IMAPHost, &account.IMAPPort, &account.IMAPUsername, &account.IMAPPassword, &account.IMAPTLS,
		&account.SMTPHost, &account.SMTPPort, &account.SMTPUsername, &account.SMTPPassword, &account.SMTPTLS,
		&account.Enabled, &lastSync,
		&account.AuthType, &account.OAuthAccessToken, &account.OAuthRefreshToken, &tokenExpiry,
		&account.CreatedAt, &account.UpdatedAt,
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
	if tokenExpiry.Valid {
		account.OAuthTokenExpiry = tokenExpiry.Time
	}

	// Decrypt passwords and tokens
	if err := db.decryptAccountSecrets(account); err != nil {
		return nil, fmt.Errorf("failed to decrypt secrets: %w", err)
	}

	return account, nil
}

// UpdateAccount updates an account
func (db *DB) UpdateAccount(account *models.Account) error {
	account.UpdatedAt = time.Now()

	// Encrypt passwords before storing
	encryptedIMAPPassword, err := crypto.EncryptPassword(account.IMAPPassword, db.encryptionKey)
	if err != nil {
		return fmt.Errorf("failed to encrypt IMAP password: %w", err)
	}

	encryptedSMTPPassword, err := crypto.EncryptPassword(account.SMTPPassword, db.encryptionKey)
	if err != nil {
		return fmt.Errorf("failed to encrypt SMTP password: %w", err)
	}

	query := `
		UPDATE accounts
		SET name = $1, email = $2, imap_host = $3, imap_port = $4, imap_username = $5, imap_password = $6, imap_tls = $7,
		    smtp_host = $8, smtp_port = $9, smtp_username = $10, smtp_password = $11, smtp_tls = $12,
		    enabled = $13, last_sync = $14, updated_at = $15
		WHERE id = $16
	`

	_, err = db.Exec(
		query,
		account.Name, account.Email,
		account.IMAPHost, account.IMAPPort, account.IMAPUsername, encryptedIMAPPassword, account.IMAPTLS,
		account.SMTPHost, account.SMTPPort, account.SMTPUsername, encryptedSMTPPassword, account.SMTPTLS,
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
		       smtp_host, smtp_port, smtp_username, smtp_password, smtp_tls, enabled, last_sync,
		       COALESCE(auth_type, 'password'), COALESCE(oauth_access_token, ''), COALESCE(oauth_refresh_token, ''), oauth_token_expiry,
		       created_at, updated_at
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
		var lastSync, tokenExpiry sql.NullTime

		err := rows.Scan(
			&account.ID, &account.UserID, &account.Name, &account.Email,
			&account.IMAPHost, &account.IMAPPort, &account.IMAPUsername, &account.IMAPPassword, &account.IMAPTLS,
			&account.SMTPHost, &account.SMTPPort, &account.SMTPUsername, &account.SMTPPassword, &account.SMTPTLS,
			&account.Enabled, &lastSync,
			&account.AuthType, &account.OAuthAccessToken, &account.OAuthRefreshToken, &tokenExpiry,
			&account.CreatedAt, &account.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan account: %w", err)
		}

		if lastSync.Valid {
			account.LastSync = lastSync.Time
		}
		if tokenExpiry.Valid {
			account.OAuthTokenExpiry = tokenExpiry.Time
		}

		// Decrypt passwords and tokens
		if err := db.decryptAccountSecrets(account); err != nil {
			return nil, fmt.Errorf("failed to decrypt secrets for account %d: %w", account.ID, err)
		}

		accounts = append(accounts, account)
	}

	return accounts, nil
}

// decryptAccountSecrets decrypts passwords and OAuth tokens in the account
func (db *DB) decryptAccountSecrets(account *models.Account) error {
	// Decrypt IMAP password (if not OAuth)
	if account.IMAPPassword != "" {
		decryptedIMAP, err := crypto.DecryptPassword(account.IMAPPassword, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to decrypt IMAP password: %w", err)
		}
		account.IMAPPassword = decryptedIMAP
	}

	// Decrypt SMTP password (if not OAuth)
	if account.SMTPPassword != "" {
		decryptedSMTP, err := crypto.DecryptPassword(account.SMTPPassword, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to decrypt SMTP password: %w", err)
		}
		account.SMTPPassword = decryptedSMTP
	}

	// Decrypt OAuth access token
	if account.OAuthAccessToken != "" {
		decrypted, err := crypto.DecryptPassword(account.OAuthAccessToken, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to decrypt OAuth access token: %w", err)
		}
		account.OAuthAccessToken = decrypted
	}

	// Decrypt OAuth refresh token
	if account.OAuthRefreshToken != "" {
		decrypted, err := crypto.DecryptPassword(account.OAuthRefreshToken, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to decrypt OAuth refresh token: %w", err)
		}
		account.OAuthRefreshToken = decrypted
	}

	return nil
}

// decryptAccountPasswords is kept for backwards compatibility
func (db *DB) decryptAccountPasswords(account *models.Account) error {
	return db.decryptAccountSecrets(account)
}

// UpdateAccountOAuthTokens updates OAuth tokens for an account
func (db *DB) UpdateAccountOAuthTokens(accountID int64, accessToken, refreshToken string, expiry time.Time) error {
	// Encrypt tokens before storing
	encryptedAccessToken, err := crypto.EncryptPassword(accessToken, db.encryptionKey)
	if err != nil {
		return fmt.Errorf("failed to encrypt access token: %w", err)
	}

	var encryptedRefreshToken string
	if refreshToken != "" {
		encryptedRefreshToken, err = crypto.EncryptPassword(refreshToken, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt refresh token: %w", err)
		}
	}

	query := `
		UPDATE accounts
		SET oauth_access_token = $1, oauth_refresh_token = COALESCE(NULLIF($2, ''), oauth_refresh_token),
		    oauth_token_expiry = $3, updated_at = $4
		WHERE id = $5
	`

	_, err = db.Exec(query, encryptedAccessToken, encryptedRefreshToken, expiry, time.Now(), accountID)
	if err != nil {
		return fmt.Errorf("failed to update OAuth tokens: %w", err)
	}

	return nil
}

// MigrateUnencryptedPasswords finds and encrypts any plaintext passwords in the database
func (db *DB) MigrateUnencryptedPasswords() error {
	query := `SELECT id, imap_password, smtp_password FROM accounts`

	rows, err := db.Query(query)
	if err != nil {
		return fmt.Errorf("failed to query accounts: %w", err)
	}
	defer rows.Close()

	var migratedCount int
	for rows.Next() {
		var id int64
		var imapPassword, smtpPassword string

		if err := rows.Scan(&id, &imapPassword, &smtpPassword); err != nil {
			return fmt.Errorf("failed to scan account: %w", err)
		}

		needsUpdate := false
		var newIMAPPassword, newSMTPPassword string

		// Check if IMAP password needs encryption
		if !crypto.IsEncrypted(imapPassword) {
			encrypted, err := crypto.EncryptPassword(imapPassword, db.encryptionKey)
			if err != nil {
				return fmt.Errorf("failed to encrypt IMAP password for account %d: %w", id, err)
			}
			newIMAPPassword = encrypted
			needsUpdate = true
		} else {
			newIMAPPassword = imapPassword
		}

		// Check if SMTP password needs encryption
		if !crypto.IsEncrypted(smtpPassword) {
			encrypted, err := crypto.EncryptPassword(smtpPassword, db.encryptionKey)
			if err != nil {
				return fmt.Errorf("failed to encrypt SMTP password for account %d: %w", id, err)
			}
			newSMTPPassword = encrypted
			needsUpdate = true
		} else {
			newSMTPPassword = smtpPassword
		}

		// Update if needed
		if needsUpdate {
			updateQuery := `UPDATE accounts SET imap_password = $1, smtp_password = $2 WHERE id = $3`
			if _, err := db.Exec(updateQuery, newIMAPPassword, newSMTPPassword, id); err != nil {
				return fmt.Errorf("failed to update account %d: %w", id, err)
			}
			migratedCount++
		}
	}

	if migratedCount > 0 {
		log.Printf("Migrated %d accounts with unencrypted passwords", migratedCount)
	}

	return nil
}
