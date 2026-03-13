package models

import (
	"database/sql"
	"time"
)

// ContactSource represents a source of contacts (local, CardDAV, Google, Microsoft)
type ContactSource struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	Name       string `json:"name"`
	SourceType string `json:"source_type"` // "local", "carddav", "google", "microsoft"

	// CardDAV fields
	CardDAVURL      string `json:"carddav_url,omitempty"`
	CardDAVUsername string `json:"carddav_username,omitempty"`
	CardDAVPassword string `json:"-"` // encrypted

	// OAuth fields
	AuthType          string    `json:"auth_type"` // "password", "oauth2_google", "oauth2_microsoft"
	OAuthAccessToken  string    `json:"-"`
	OAuthRefreshToken string    `json:"-"`
	OAuthTokenExpiry  time.Time `json:"oauth_token_expiry,omitempty"`

	// Sync settings
	SyncEnabled  bool      `json:"sync_enabled"`
	SyncInterval int       `json:"sync_interval"` // seconds
	LastSync     time.Time `json:"last_sync,omitempty"`
	LastError    string    `json:"last_error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NeedsSync returns true if the source needs synchronization
func (s *ContactSource) NeedsSync() bool {
	// Local sources don't sync
	if !s.SyncEnabled || s.SourceType == "local" {
		return false
	}
	if s.LastSync.IsZero() {
		return true
	}
	return time.Since(s.LastSync) >= time.Duration(s.SyncInterval)*time.Second
}

// IsOAuth returns true if the source uses OAuth authentication
func (s *ContactSource) IsOAuth() bool {
	return s.AuthType == "oauth2_google" || s.AuthType == "oauth2_microsoft"
}

// IsGoogleOAuth returns true if the source uses Google OAuth
func (s *ContactSource) IsGoogleOAuth() bool {
	return s.AuthType == "oauth2_google"
}

// IsMicrosoftOAuth returns true if the source uses Microsoft OAuth
func (s *ContactSource) IsMicrosoftOAuth() bool {
	return s.AuthType == "oauth2_microsoft"
}

// AddressBook represents an address book (container for contacts)
type AddressBook struct {
	ID       int64  `json:"id"`
	UserID   int64  `json:"user_id"`
	SourceID int64  `json:"source_id"`
	RemoteID string `json:"remote_id,omitempty"` // ID on remote server

	Name        string `json:"name"`
	Description string `json:"description"`
	CTag        string `json:"-"` // for sync

	CanWrite bool `json:"can_write"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Joined field (not stored in DB)
	SourceType string `json:"source_type,omitempty"`
}

// Contact represents a contact entry
type Contact struct {
	ID            int64  `json:"id"`
	UserID        int64  `json:"user_id"`
	AddressBookID int64  `json:"address_book_id"`
	UID           string `json:"uid"` // vCard UID
	RemoteID      string `json:"-"`   // ID on remote server
	VCardData     string `json:"-"`   // raw vCard data

	// Parsed fields for display and search
	FullName   string `json:"full_name"`
	GivenName  string `json:"given_name"`
	FamilyName string `json:"family_name"`
	Nickname   string `json:"nickname,omitempty"`

	// Multiple emails
	Email  string `json:"email,omitempty"`
	Email2 string `json:"email2,omitempty"`
	Email3 string `json:"email3,omitempty"`

	// Multiple phones
	Phone  string `json:"phone,omitempty"`
	Phone2 string `json:"phone2,omitempty"`
	Phone3 string `json:"phone3,omitempty"`

	// Organization info
	Organization string `json:"organization,omitempty"`
	Title        string `json:"title,omitempty"`
	Department   string `json:"department,omitempty"`

	// Other fields
	Address  string       `json:"address,omitempty"`
	Notes    string       `json:"notes,omitempty"`
	PhotoURL string       `json:"photo_url,omitempty"`
	Birthday sql.NullTime `json:"birthday,omitempty"`

	// Sync fields
	ETag          string `json:"-"`
	LocalModified bool   `json:"-"` // modified locally, needs push to remote

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DisplayName returns the best available display name for the contact
func (c *Contact) DisplayName() string {
	if c.FullName != "" {
		return c.FullName
	}
	if c.GivenName != "" || c.FamilyName != "" {
		name := c.GivenName
		if name != "" && c.FamilyName != "" {
			name += " "
		}
		name += c.FamilyName
		return name
	}
	if c.Nickname != "" {
		return c.Nickname
	}
	if c.Email != "" {
		return c.Email
	}
	return "Unknown"
}

// PrimaryEmail returns the primary email address
func (c *Contact) PrimaryEmail() string {
	if c.Email != "" {
		return c.Email
	}
	if c.Email2 != "" {
		return c.Email2
	}
	return c.Email3
}

// AllEmails returns all non-empty email addresses
func (c *Contact) AllEmails() []string {
	var emails []string
	if c.Email != "" {
		emails = append(emails, c.Email)
	}
	if c.Email2 != "" {
		emails = append(emails, c.Email2)
	}
	if c.Email3 != "" {
		emails = append(emails, c.Email3)
	}
	return emails
}

// PrimaryPhone returns the primary phone number
func (c *Contact) PrimaryPhone() string {
	if c.Phone != "" {
		return c.Phone
	}
	if c.Phone2 != "" {
		return c.Phone2
	}
	return c.Phone3
}
