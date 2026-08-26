package db

import (
	"database/sql"
	"fmt"

	"github.com/yourusername/mailserver/internal/crypto"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// CreateCalendarSource creates a new calendar source
func (db *DB) CreateCalendarSource(source *models.CalendarSource) error {
	return createCalendarSource(db, db.encryptionKey, source)
}

// CreateCalendarSource creates a calendar source inside a transaction. Used by the .mobileconfig
// import, where every payload has to land together or not at all.
func (tx *Tx) CreateCalendarSource(source *models.CalendarSource) error {
	return createCalendarSource(tx, tx.encryptionKey, source)
}

func createCalendarSource(q querier, encryptionKey string, source *models.CalendarSource) error {
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
	if source.CalDAVPassword != "" {
		encryptedPassword, err = crypto.EncryptPassword(source.CalDAVPassword, encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt CalDAV password: %w", err)
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
		INSERT INTO calendar_sources (
			user_id, name, source_type, caldav_url, caldav_username, caldav_password,
			auth_type, oauth_access_token, oauth_refresh_token, oauth_token_expiry,
			sync_enabled, sync_interval, color, created_at, updated_at, ics_url, account_id,
			default_alarm_enabled, default_alarm_before, default_alarm_unit, identity_email
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
		RETURNING id
	`

	err = q.QueryRow(
		query,
		source.UserID, source.Name, source.SourceType,
		source.CalDAVURL, source.CalDAVUsername, encryptedPassword,
		source.AuthType, encryptedAccessToken, encryptedRefreshToken, tokenExpiry,
		source.SyncEnabled, source.SyncInterval, source.Color,
		source.CreatedAt, source.UpdatedAt, source.IcsURL, source.AccountID,
		source.DefaultAlarmEnabled, source.DefaultAlarmBefore, source.DefaultAlarmUnit,
		source.IdentityEmail,
	).Scan(&source.ID)

	if err != nil {
		return fmt.Errorf("failed to create calendar source: %w", err)
	}

	return nil
}

// GetCalendarSourcesByUserID retrieves all calendar sources for a user
func (db *DB) GetCalendarSourcesByUserID(userID int64) ([]*models.CalendarSource, error) {
	query := `
		SELECT cs.id, cs.user_id, cs.name, cs.source_type,
		       COALESCE(cs.caldav_url, ''), COALESCE(cs.caldav_username, ''), COALESCE(cs.caldav_password, ''),
		       COALESCE(cs.auth_type, 'password'), COALESCE(cs.oauth_access_token, ''), COALESCE(cs.oauth_refresh_token, ''), cs.oauth_token_expiry,
		       cs.sync_enabled, cs.sync_interval, cs.last_sync, COALESCE(cs.last_error, ''), COALESCE(cs.sync_token, ''),
		       cs.color, cs.created_at, cs.updated_at, COALESCE(cs.ics_url, ''), cs.account_id, COALESCE(cs.identity_email, ''),
		       COALESCE(a.email, ''),
		       COALESCE(cs.default_alarm_enabled, false), COALESCE(cs.default_alarm_before, 15), COALESCE(cs.default_alarm_unit, 'minutes')
		FROM calendar_sources cs
		LEFT JOIN accounts a ON cs.account_id = a.id
		WHERE cs.user_id = $1
		ORDER BY cs.created_at DESC
	`

	rows, err := db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get calendar sources: %w", err)
	}
	defer rows.Close()

	var sources []*models.CalendarSource
	for rows.Next() {
		source := &models.CalendarSource{}
		var lastSync, tokenExpiry sql.NullInt64
		var accountID sql.NullInt64

		err := rows.Scan(
			&source.ID, &source.UserID, &source.Name, &source.SourceType,
			&source.CalDAVURL, &source.CalDAVUsername, &source.CalDAVPassword,
			&source.AuthType, &source.OAuthAccessToken, &source.OAuthRefreshToken, &tokenExpiry,
			&source.SyncEnabled, &source.SyncInterval, &lastSync, &source.LastError, &source.SyncToken,
			&source.Color, &source.CreatedAt, &source.UpdatedAt, &source.IcsURL, &accountID,
			&source.IdentityEmail,
			&source.AccountEmail,
			&source.DefaultAlarmEnabled, &source.DefaultAlarmBefore, &source.DefaultAlarmUnit,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan calendar source: %w", err)
		}

		if lastSync.Valid {
			source.LastSync = lastSync.Int64
		}
		if tokenExpiry.Valid {
			source.OAuthTokenExpiry = tokenExpiry.Int64
		}
		if accountID.Valid {
			source.AccountID = &accountID.Int64
		}

		// Decrypt secrets
		if err := db.decryptCalendarSourceSecrets(source); err != nil {
			return nil, fmt.Errorf("failed to decrypt secrets for source %d: %w", source.ID, err)
		}

		sources = append(sources, source)
	}

	return sources, nil
}

// GetCalendarSourceByID retrieves a calendar source by ID
func (db *DB) GetCalendarSourceByID(id int64) (*models.CalendarSource, error) {
	source := &models.CalendarSource{}
	var lastSync, tokenExpiry sql.NullInt64
	var accountID sql.NullInt64

	query := `
		SELECT cs.id, cs.user_id, cs.name, cs.source_type,
		       COALESCE(cs.caldav_url, ''), COALESCE(cs.caldav_username, ''), COALESCE(cs.caldav_password, ''),
		       COALESCE(cs.auth_type, 'password'), COALESCE(cs.oauth_access_token, ''), COALESCE(cs.oauth_refresh_token, ''), cs.oauth_token_expiry,
		       cs.sync_enabled, cs.sync_interval, cs.last_sync, COALESCE(cs.last_error, ''), COALESCE(cs.sync_token, ''),
		       cs.color, cs.created_at, cs.updated_at, COALESCE(cs.ics_url, ''), cs.account_id, COALESCE(cs.identity_email, ''),
		       COALESCE(a.email, ''),
		       COALESCE(cs.default_alarm_enabled, false), COALESCE(cs.default_alarm_before, 15), COALESCE(cs.default_alarm_unit, 'minutes')
		FROM calendar_sources cs
		LEFT JOIN accounts a ON cs.account_id = a.id
		WHERE cs.id = $1
	`

	err := db.QueryRow(query, id).Scan(
		&source.ID, &source.UserID, &source.Name, &source.SourceType,
		&source.CalDAVURL, &source.CalDAVUsername, &source.CalDAVPassword,
		&source.AuthType, &source.OAuthAccessToken, &source.OAuthRefreshToken, &tokenExpiry,
		&source.SyncEnabled, &source.SyncInterval, &lastSync, &source.LastError, &source.SyncToken,
		&source.Color, &source.CreatedAt, &source.UpdatedAt, &source.IcsURL, &accountID,
		&source.IdentityEmail,
		&source.AccountEmail,
		&source.DefaultAlarmEnabled, &source.DefaultAlarmBefore, &source.DefaultAlarmUnit,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("calendar source not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get calendar source: %w", err)
	}

	if lastSync.Valid {
		source.LastSync = lastSync.Int64
	}
	if tokenExpiry.Valid {
		source.OAuthTokenExpiry = tokenExpiry.Int64
	}
	if accountID.Valid {
		source.AccountID = &accountID.Int64
	}

	// Decrypt secrets
	if err := db.decryptCalendarSourceSecrets(source); err != nil {
		return nil, fmt.Errorf("failed to decrypt secrets: %w", err)
	}

	return source, nil
}

// UpdateCalendarSource updates a calendar source
func (db *DB) UpdateCalendarSource(source *models.CalendarSource) error {
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
	if source.CalDAVPassword != "" {
		encryptedPassword, err = crypto.EncryptPassword(source.CalDAVPassword, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt CalDAV password: %w", err)
		}
	}

	query := `
		UPDATE calendar_sources
		SET name = $1, caldav_url = $2, caldav_username = $3, caldav_password = $4,
		    sync_enabled = $5, sync_interval = $6, color = $7, updated_at = $8, account_id = $9,
		    default_alarm_enabled = $10, default_alarm_before = $11, default_alarm_unit = $12,
		    identity_email = $13
		WHERE id = $14
	`

	_, err = db.Exec(
		query,
		source.Name, source.CalDAVURL, source.CalDAVUsername, encryptedPassword,
		source.SyncEnabled, source.SyncInterval, source.Color, source.UpdatedAt, source.AccountID,
		source.DefaultAlarmEnabled, source.DefaultAlarmBefore, source.DefaultAlarmUnit,
		source.IdentityEmail, source.ID,
	)

	if err != nil {
		return fmt.Errorf("failed to update calendar source: %w", err)
	}

	return nil
}

// DeleteCalendarSource deletes a calendar source (cascades to calendars and events)
func (db *DB) DeleteCalendarSource(id int64) error {
	query := `DELETE FROM calendar_sources WHERE id = $1`
	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete calendar source: %w", err)
	}
	return nil
}

// UpdateCalendarSourceLastSync updates the last sync time and token
func (db *DB) UpdateCalendarSourceLastSync(sourceID int64, lastSync int64, syncToken string) error {
	query := `UPDATE calendar_sources SET last_sync = $1, sync_token = $2, last_error = '', updated_at = $3 WHERE id = $4`
	_, err := db.Exec(query, lastSync, syncToken, timeutil.Now(), sourceID)
	if err != nil {
		return fmt.Errorf("failed to update last sync: %w", err)
	}
	return nil
}

// UpdateCalendarSourceLastError updates the last error for a calendar source
func (db *DB) UpdateCalendarSourceLastError(sourceID int64, lastError string) error {
	query := `UPDATE calendar_sources SET last_error = $1, updated_at = $2 WHERE id = $3`
	_, err := db.Exec(query, lastError, timeutil.Now(), sourceID)
	if err != nil {
		return fmt.Errorf("failed to update last error: %w", err)
	}
	return nil
}

// GetAllEnabledCalendarSources retrieves all enabled CalDAV and ICS URL sources for sync
func (db *DB) GetAllEnabledCalendarSources() ([]*models.CalendarSource, error) {
	query := `
		SELECT cs.id, cs.user_id, cs.name, cs.source_type,
		       COALESCE(cs.caldav_url, ''), COALESCE(cs.caldav_username, ''), COALESCE(cs.caldav_password, ''),
		       COALESCE(cs.auth_type, 'password'), COALESCE(cs.oauth_access_token, ''), COALESCE(cs.oauth_refresh_token, ''), cs.oauth_token_expiry,
		       cs.sync_enabled, cs.sync_interval, cs.last_sync, COALESCE(cs.last_error, ''), COALESCE(cs.sync_token, ''),
		       cs.color, cs.created_at, cs.updated_at, COALESCE(cs.ics_url, ''), cs.account_id, COALESCE(cs.identity_email, ''),
		       COALESCE(cs.default_alarm_enabled, false), COALESCE(cs.default_alarm_before, 15), COALESCE(cs.default_alarm_unit, 'minutes')
		FROM calendar_sources cs
		WHERE cs.sync_enabled = true AND cs.source_type IN ('caldav', 'ics_url')
		ORDER BY cs.last_sync ASC NULLS FIRST
	`

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to get enabled calendar sources: %w", err)
	}
	defer rows.Close()

	var sources []*models.CalendarSource
	for rows.Next() {
		source := &models.CalendarSource{}
		var lastSync, tokenExpiry sql.NullInt64
		var accountID sql.NullInt64

		err := rows.Scan(
			&source.ID, &source.UserID, &source.Name, &source.SourceType,
			&source.CalDAVURL, &source.CalDAVUsername, &source.CalDAVPassword,
			&source.AuthType, &source.OAuthAccessToken, &source.OAuthRefreshToken, &tokenExpiry,
			&source.SyncEnabled, &source.SyncInterval, &lastSync, &source.LastError, &source.SyncToken,
			&source.Color, &source.CreatedAt, &source.UpdatedAt, &source.IcsURL, &accountID,
			&source.IdentityEmail,
			&source.DefaultAlarmEnabled, &source.DefaultAlarmBefore, &source.DefaultAlarmUnit,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan calendar source: %w", err)
		}

		if lastSync.Valid {
			source.LastSync = lastSync.Int64
		}
		if tokenExpiry.Valid {
			source.OAuthTokenExpiry = tokenExpiry.Int64
		}
		if accountID.Valid {
			source.AccountID = &accountID.Int64
		}

		// Decrypt secrets
		if err := db.decryptCalendarSourceSecrets(source); err != nil {
			return nil, fmt.Errorf("failed to decrypt secrets for source %d: %w", source.ID, err)
		}

		sources = append(sources, source)
	}

	return sources, nil
}

// UpdateCalendarSourceOAuthTokens updates OAuth tokens for a calendar source
func (db *DB) UpdateCalendarSourceOAuthTokens(sourceID int64, accessToken, refreshToken string, expiry int64) error {
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
		UPDATE calendar_sources
		SET oauth_access_token = $1, oauth_refresh_token = COALESCE(NULLIF($2, ''), oauth_refresh_token),
		    oauth_token_expiry = $3, updated_at = $4
		WHERE id = $5
	`

	_, err = db.Exec(query, encryptedAccessToken, encryptedRefreshToken, expiry, timeutil.Now(), sourceID)
	if err != nil {
		return fmt.Errorf("failed to update OAuth tokens: %w", err)
	}

	return nil
}

// decryptCalendarSourceSecrets decrypts password and OAuth tokens
func (db *DB) decryptCalendarSourceSecrets(source *models.CalendarSource) error {
	if source.CalDAVPassword != "" {
		decrypted, err := crypto.DecryptPassword(source.CalDAVPassword, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to decrypt CalDAV password: %w", err)
		}
		source.CalDAVPassword = decrypted
	}

	if source.OAuthAccessToken != "" {
		decrypted, err := crypto.DecryptPassword(source.OAuthAccessToken, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to decrypt OAuth access token: %w", err)
		}
		source.OAuthAccessToken = decrypted
	}

	if source.OAuthRefreshToken != "" {
		decrypted, err := crypto.DecryptPassword(source.OAuthRefreshToken, db.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to decrypt OAuth refresh token: %w", err)
		}
		source.OAuthRefreshToken = decrypted
	}

	return nil
}
