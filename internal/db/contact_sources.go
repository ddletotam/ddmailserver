package db

import (
	"database/sql"
	"fmt"

	"github.com/yourusername/mailserver/internal/crypto"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// CreateContactSource creates a new contact source
func (db *DB) CreateContactSource(source *models.ContactSource) error {
	return createContactSource(db, db.encryptionKey, source)
}

// CreateContactSource creates a contact source inside a transaction. Used by the .mobileconfig
// import, where every payload has to land together or not at all.
func (tx *Tx) CreateContactSource(source *models.ContactSource) error {
	return createContactSource(tx, tx.encryptionKey, source)
}

func createContactSource(q querier, encryptionKey string, source *models.ContactSource) error {
	source.CreatedAt = timeutil.Now()
	source.UpdatedAt = timeutil.Now()

	if source.AuthType == "" {
		source.AuthType = "password"
	}

	// No orphan sources: fall back to the user's default identity.
	if source.IdentityEmail == "" {
		email, err := defaultIdentityEmail(q, source.UserID)
		if err != nil {
			return err
		}
		source.IdentityEmail = email
	}

	// Encrypt password
	var encryptedPassword string
	var err error
	if source.CardDAVPassword != "" {
		encryptedPassword, err = crypto.EncryptPassword(source.CardDAVPassword, encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt CardDAV password: %w", err)
		}
	}

	// Encrypt OAuth tokens
	var encryptedAccessToken, encryptedRefreshToken string
	if source.OAuthAccessToken != "" {
		encryptedAccessToken, err = crypto.EncryptPassword(source.OAuthAccessToken, encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt OAuth access token: %w", err)
		}
	}
	if source.OAuthRefreshToken != "" {
		encryptedRefreshToken, err = crypto.EncryptPassword(source.OAuthRefreshToken, encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt OAuth refresh token: %w", err)
		}
	}

	var tokenExpiry sql.NullInt64
	if source.OAuthTokenExpiry != 0 {
		tokenExpiry = sql.NullInt64{Int64: source.OAuthTokenExpiry, Valid: true}
	}

	query := `
		INSERT INTO contact_sources (
			user_id, name, source_type, carddav_url, carddav_username, carddav_password,
			auth_type, oauth_access_token, oauth_refresh_token, oauth_token_expiry,
			sync_enabled, sync_interval, created_at, updated_at, identity_email
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING id
	`

	err = q.QueryRow(
		query,
		source.UserID, source.Name, source.SourceType,
		source.CardDAVURL, source.CardDAVUsername, encryptedPassword,
		source.AuthType, encryptedAccessToken, encryptedRefreshToken, tokenExpiry,
		source.SyncEnabled, source.SyncInterval,
		source.CreatedAt, source.UpdatedAt, source.IdentityEmail,
	).Scan(&source.ID)

	if err != nil {
		return fmt.Errorf("failed to create contact source: %w", err)
	}

	return nil
}

// GetContactSourcesByUserID retrieves all contact sources for a user
func (db *DB) GetContactSourcesByUserID(userID int64) ([]*models.ContactSource, error) {
	query := `
		SELECT id, user_id, name, source_type, COALESCE(identity_email, ''),
		       COALESCE(carddav_url, ''), COALESCE(carddav_username, ''), COALESCE(carddav_password, ''),
		       COALESCE(auth_type, 'password'), COALESCE(oauth_access_token, ''), COALESCE(oauth_refresh_token, ''), oauth_token_expiry,
		       sync_enabled, sync_interval, last_sync, COALESCE(last_error, ''),
		       created_at, updated_at
		FROM contact_sources
		WHERE user_id = $1
		ORDER BY created_at DESC
	`

	rows, err := db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get contact sources: %w", err)
	}
	defer rows.Close()

	var sources []*models.ContactSource
	for rows.Next() {
		source := &models.ContactSource{}
		var lastSync, tokenExpiry sql.NullInt64

		err := rows.Scan(
			&source.ID, &source.UserID, &source.Name, &source.SourceType,
			&source.IdentityEmail,
			&source.CardDAVURL, &source.CardDAVUsername, &source.CardDAVPassword,
			&source.AuthType, &source.OAuthAccessToken, &source.OAuthRefreshToken, &tokenExpiry,
			&source.SyncEnabled, &source.SyncInterval, &lastSync, &source.LastError,
			&source.CreatedAt, &source.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan contact source: %w", err)
		}

		if lastSync.Valid {
			source.LastSync = lastSync.Int64
		}
		if tokenExpiry.Valid {
			source.OAuthTokenExpiry = tokenExpiry.Int64
		}

		// Decrypt secrets
		if err := db.decryptContactSourceSecrets(source); err != nil {
			return nil, fmt.Errorf("failed to decrypt secrets for source %d: %w", source.ID, err)
		}

		sources = append(sources, source)
	}

	return sources, nil
}

// GetContactSourceByID retrieves a contact source by ID
func (db *DB) GetContactSourceByID(id int64) (*models.ContactSource, error) {
	source := &models.ContactSource{}
	var lastSync, tokenExpiry sql.NullInt64

	query := `
		SELECT id, user_id, name, source_type, COALESCE(identity_email, ''),
		       COALESCE(carddav_url, ''), COALESCE(carddav_username, ''), COALESCE(carddav_password, ''),
		       COALESCE(auth_type, 'password'), COALESCE(oauth_access_token, ''), COALESCE(oauth_refresh_token, ''), oauth_token_expiry,
		       sync_enabled, sync_interval, last_sync, COALESCE(last_error, ''),
		       created_at, updated_at
		FROM contact_sources
		WHERE id = $1
	`

	err := db.QueryRow(query, id).Scan(
		&source.ID, &source.UserID, &source.Name, &source.SourceType,
		&source.IdentityEmail,
		&source.CardDAVURL, &source.CardDAVUsername, &source.CardDAVPassword,
		&source.AuthType, &source.OAuthAccessToken, &source.OAuthRefreshToken, &tokenExpiry,
		&source.SyncEnabled, &source.SyncInterval, &lastSync, &source.LastError,
		&source.CreatedAt, &source.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("contact source not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get contact source: %w", err)
	}

	if lastSync.Valid {
		source.LastSync = lastSync.Int64
	}
	if tokenExpiry.Valid {
		source.OAuthTokenExpiry = tokenExpiry.Int64
	}

	// Decrypt secrets
	if err := db.decryptContactSourceSecrets(source); err != nil {
		return nil, fmt.Errorf("failed to decrypt secrets: %w", err)
	}

	return source, nil
}

// UpdateContactSource updates an existing contact source
func (db *DB) UpdateContactSource(source *models.ContactSource) error {
	source.UpdatedAt = timeutil.Now()

	if source.IdentityEmail == "" {
		email, err := db.DefaultIdentityEmail(source.UserID)
		if err != nil {
			return err
		}
		source.IdentityEmail = email
	}

	// Encrypt password
	var encryptedPassword string
	var err error
	if source.CardDAVPassword != "" {
		encryptedPassword, err = crypto.EncryptPassword(source.CardDAVPassword, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt CardDAV password: %w", err)
		}
	}

	// Encrypt OAuth tokens
	var encryptedAccessToken, encryptedRefreshToken string
	if source.OAuthAccessToken != "" {
		encryptedAccessToken, err = crypto.EncryptPassword(source.OAuthAccessToken, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt OAuth access token: %w", err)
		}
	}
	if source.OAuthRefreshToken != "" {
		encryptedRefreshToken, err = crypto.EncryptPassword(source.OAuthRefreshToken, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt OAuth refresh token: %w", err)
		}
	}

	var tokenExpiry sql.NullInt64
	if source.OAuthTokenExpiry != 0 {
		tokenExpiry = sql.NullInt64{Int64: source.OAuthTokenExpiry, Valid: true}
	}

	query := `
		UPDATE contact_sources SET
			name = $1, source_type = $2, carddav_url = $3, carddav_username = $4, carddav_password = $5,
			auth_type = $6, oauth_access_token = $7, oauth_refresh_token = $8, oauth_token_expiry = $9,
			sync_enabled = $10, sync_interval = $11, updated_at = $12, identity_email = $13
		WHERE id = $14
	`

	_, err = db.Exec(
		query,
		source.Name, source.SourceType, source.CardDAVURL, source.CardDAVUsername, encryptedPassword,
		source.AuthType, encryptedAccessToken, encryptedRefreshToken, tokenExpiry,
		source.SyncEnabled, source.SyncInterval, source.UpdatedAt, source.IdentityEmail,
		source.ID,
	)

	if err != nil {
		return fmt.Errorf("failed to update contact source: %w", err)
	}

	return nil
}

// DeleteContactSource deletes a contact source and all its address books/contacts
func (db *DB) DeleteContactSource(id int64) error {
	query := `DELETE FROM contact_sources WHERE id = $1`
	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete contact source: %w", err)
	}
	return nil
}

// UpdateContactSourceLastSync updates the last sync time for a contact source
func (db *DB) UpdateContactSourceLastSync(id int64, lastSync int64) error {
	query := `UPDATE contact_sources SET last_sync = $1, last_error = NULL, updated_at = $2 WHERE id = $3`
	_, err := db.Exec(query, lastSync, timeutil.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to update last sync: %w", err)
	}
	return nil
}

// UpdateContactSourceLastError updates the last error for a contact source
func (db *DB) UpdateContactSourceLastError(id int64, lastError string) error {
	query := `UPDATE contact_sources SET last_error = $1, updated_at = $2 WHERE id = $3`
	_, err := db.Exec(query, lastError, timeutil.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to update last error: %w", err)
	}
	return nil
}

// GetAllEnabledContactSources retrieves all enabled contact sources for syncing
func (db *DB) GetAllEnabledContactSources() ([]*models.ContactSource, error) {
	query := `
		SELECT id, user_id, name, source_type, COALESCE(identity_email, ''),
		       COALESCE(carddav_url, ''), COALESCE(carddav_username, ''), COALESCE(carddav_password, ''),
		       COALESCE(auth_type, 'password'), COALESCE(oauth_access_token, ''), COALESCE(oauth_refresh_token, ''), oauth_token_expiry,
		       sync_enabled, sync_interval, last_sync, COALESCE(last_error, ''),
		       created_at, updated_at
		FROM contact_sources
		WHERE sync_enabled = true AND source_type != 'local'
		ORDER BY last_sync ASC NULLS FIRST
	`

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to get enabled contact sources: %w", err)
	}
	defer rows.Close()

	var sources []*models.ContactSource
	for rows.Next() {
		source := &models.ContactSource{}
		var lastSync, tokenExpiry sql.NullInt64

		err := rows.Scan(
			&source.ID, &source.UserID, &source.Name, &source.SourceType,
			&source.IdentityEmail,
			&source.CardDAVURL, &source.CardDAVUsername, &source.CardDAVPassword,
			&source.AuthType, &source.OAuthAccessToken, &source.OAuthRefreshToken, &tokenExpiry,
			&source.SyncEnabled, &source.SyncInterval, &lastSync, &source.LastError,
			&source.CreatedAt, &source.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan contact source: %w", err)
		}

		if lastSync.Valid {
			source.LastSync = lastSync.Int64
		}
		if tokenExpiry.Valid {
			source.OAuthTokenExpiry = tokenExpiry.Int64
		}

		// Decrypt secrets
		if err := db.decryptContactSourceSecrets(source); err != nil {
			return nil, fmt.Errorf("failed to decrypt secrets for source %d: %w", source.ID, err)
		}

		sources = append(sources, source)
	}

	return sources, nil
}

// UpdateContactSourceOAuthTokens updates the OAuth tokens for a contact source
func (db *DB) UpdateContactSourceOAuthTokens(id int64, accessToken, refreshToken string, expiry int64) error {
	var encryptedAccessToken, encryptedRefreshToken string
	var err error

	if accessToken != "" {
		encryptedAccessToken, err = crypto.EncryptPassword(accessToken, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt access token: %w", err)
		}
	}
	if refreshToken != "" {
		encryptedRefreshToken, err = crypto.EncryptPassword(refreshToken, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt refresh token: %w", err)
		}
	}

	query := `
		UPDATE contact_sources SET
			oauth_access_token = $1, oauth_refresh_token = $2, oauth_token_expiry = $3, updated_at = $4
		WHERE id = $5
	`
	_, err = db.Exec(query, encryptedAccessToken, encryptedRefreshToken, expiry, timeutil.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to update OAuth tokens: %w", err)
	}
	return nil
}

// decryptContactSourceSecrets decrypts password and OAuth tokens for a contact source
func (db *DB) decryptContactSourceSecrets(source *models.ContactSource) error {
	var err error

	if source.CardDAVPassword != "" {
		source.CardDAVPassword, err = crypto.DecryptPassword(source.CardDAVPassword, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to decrypt CardDAV password: %w", err)
		}
	}
	if source.OAuthAccessToken != "" {
		source.OAuthAccessToken, err = crypto.DecryptPassword(source.OAuthAccessToken, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to decrypt OAuth access token: %w", err)
		}
	}
	if source.OAuthRefreshToken != "" {
		source.OAuthRefreshToken, err = crypto.DecryptPassword(source.OAuthRefreshToken, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to decrypt OAuth refresh token: %w", err)
		}
	}

	return nil
}
