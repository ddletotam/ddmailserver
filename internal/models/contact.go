package models

import (
	"github.com/yourusername/mailserver/internal/timeutil"
)

// ContactSource represents a source of contacts (local, CardDAV, Google, Microsoft)
type ContactSource struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	Name       string `json:"name"`
	SourceType string `json:"source_type"` // "local", "carddav", "google", "microsoft", "ldap"

	// Identity (email address) this source belongs to. Mandatory — the
	// desktop client is identity-keyed and there are no orphan sources.
	IdentityEmail string `json:"identity_email"`

	// CardDAV fields
	CardDAVURL      string `json:"carddav_url,omitempty"`
	CardDAVUsername string `json:"carddav_username,omitempty"`
	CardDAVPassword string `json:"-"` // encrypted

	// OAuth fields
	AuthType          string `json:"auth_type"` // "password", "oauth2_google", "oauth2_microsoft"
	OAuthAccessToken  string `json:"-"`
	OAuthRefreshToken string `json:"-"`
	OAuthTokenExpiry  int64  `json:"oauth_token_expiry,omitempty"`

	// Sync settings
	SyncEnabled  bool   `json:"sync_enabled"`
	SyncInterval int    `json:"sync_interval"` // seconds
	LastSync     int64  `json:"last_sync,omitempty"`
	LastError    string `json:"last_error,omitempty"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// NeedsSync returns true if the source needs synchronization
func (s *ContactSource) NeedsSync() bool {
	if !s.SyncEnabled || s.SourceType == "local" {
		return false
	}
	if s.LastSync == 0 {
		return true
	}
	elapsed := timeutil.Now() - s.LastSync
	return elapsed >= int64(s.SyncInterval)*1000
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
	RemoteID string `json:"remote_id,omitempty"`

	Name        string `json:"name"`
	Description string `json:"description"`
	CTag        string `json:"-"`

	CanWrite    bool `json:"can_write"`
	ReverseSync bool `json:"reverse_sync"`
	Enabled     bool `json:"enabled"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`

	// Joined field
	SourceType string `json:"source_type,omitempty"`
}

// Contact represents a contact entry
type Contact struct {
	ID            int64  `json:"id"`
	UserID        int64  `json:"user_id"`
	AddressBookID int64  `json:"address_book_id"`
	UID           string `json:"uid"`
	RemoteID      string `json:"-"`
	VCardData     string `json:"-"`

	// Parsed fields
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
	Address  string `json:"address,omitempty"`
	Notes    string `json:"notes,omitempty"`
	PhotoURL string `json:"photo_url,omitempty"`
	Birthday *int64 `json:"birthday,omitempty"` // ms since epoch, nullable

	// Sync fields
	ETag          string `json:"-"`
	LocalModified bool   `json:"-"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`

	// Soft delete
	SoftDeletedAt *int64 `json:"soft_deleted_at,omitempty"`
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
