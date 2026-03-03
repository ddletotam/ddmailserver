package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/crypto"
)

// System settings keys
const (
	SettingGoogleOAuthClientID     = "google_oauth_client_id"
	SettingGoogleOAuthClientSecret = "google_oauth_client_secret"
	SettingGoogleOAuthRedirectURI  = "google_oauth_redirect_uri"
)

// GetSetting retrieves a setting value by key
func (db *DB) GetSetting(key string) (string, error) {
	var value string
	query := `SELECT value FROM system_settings WHERE key = $1`
	err := db.QueryRow(query, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to get setting %s: %w", key, err)
	}
	return value, nil
}

// SetSetting sets a setting value
func (db *DB) SetSetting(key, value string) error {
	query := `
		INSERT INTO system_settings (key, value, updated_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = $3
	`
	_, err := db.Exec(query, key, value, time.Now())
	if err != nil {
		return fmt.Errorf("failed to set setting %s: %w", key, err)
	}
	return nil
}

// GetSecretSetting retrieves and decrypts a secret setting
func (db *DB) GetSecretSetting(key string) (string, error) {
	encrypted, err := db.GetSetting(key)
	if err != nil {
		return "", err
	}
	if encrypted == "" {
		return "", nil
	}

	decrypted, err := crypto.DecryptPassword(encrypted, db.encryptionKey)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt setting %s: %w", key, err)
	}
	return decrypted, nil
}

// SetSecretSetting encrypts and stores a secret setting
func (db *DB) SetSecretSetting(key, value string) error {
	if value == "" {
		return db.SetSetting(key, "")
	}

	encrypted, err := crypto.EncryptPassword(value, db.encryptionKey)
	if err != nil {
		return fmt.Errorf("failed to encrypt setting %s: %w", key, err)
	}
	return db.SetSetting(key, encrypted)
}

// GoogleOAuthSettings holds Google OAuth configuration
type GoogleOAuthSettings struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

// GetGoogleOAuthSettings retrieves all Google OAuth settings
func (db *DB) GetGoogleOAuthSettings() (*GoogleOAuthSettings, error) {
	clientID, err := db.GetSetting(SettingGoogleOAuthClientID)
	if err != nil {
		return nil, err
	}

	clientSecret, err := db.GetSecretSetting(SettingGoogleOAuthClientSecret)
	if err != nil {
		return nil, err
	}

	redirectURI, err := db.GetSetting(SettingGoogleOAuthRedirectURI)
	if err != nil {
		return nil, err
	}

	return &GoogleOAuthSettings{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURI:  redirectURI,
	}, nil
}

// SetGoogleOAuthSettings saves all Google OAuth settings
func (db *DB) SetGoogleOAuthSettings(settings *GoogleOAuthSettings) error {
	if err := db.SetSetting(SettingGoogleOAuthClientID, settings.ClientID); err != nil {
		return err
	}
	if err := db.SetSecretSetting(SettingGoogleOAuthClientSecret, settings.ClientSecret); err != nil {
		return err
	}
	if err := db.SetSetting(SettingGoogleOAuthRedirectURI, settings.RedirectURI); err != nil {
		return err
	}
	return nil
}

// IsGoogleOAuthConfigured checks if Google OAuth is configured in DB
func (db *DB) IsGoogleOAuthConfigured() bool {
	settings, err := db.GetGoogleOAuthSettings()
	if err != nil {
		return false
	}
	return settings.ClientID != "" && settings.ClientSecret != ""
}
