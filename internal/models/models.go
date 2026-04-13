package models

import (
	"strings"
	"time"
)

// User represents a user of the mailserver
type User struct {
	ID              int64     `json:"id"`
	Username        string    `json:"username"`
	PasswordHash    string    `json:"-"` // never expose in JSON
	Email           string    `json:"email,omitempty"`
	Language        string    `json:"language,omitempty"`
	RecoveryKeyHash string    `json:"-"` // never expose in JSON
	IsAdminFlag     bool      `json:"is_admin"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// IsAdmin returns true if user is an administrator
func (u *User) IsAdmin() bool {
	return u.IsAdminFlag
}

// Account represents an external email account (Gmail, Outlook, etc.)
type Account struct {
	ID           int64     `json:"id"`
	UserID       int64     `json:"user_id"`
	Name         string    `json:"name"`          // Friendly name for the account
	Email        string    `json:"email"`         // Email address
	IMAPHost     string    `json:"imap_host"`     // imap.gmail.com
	IMAPPort     int       `json:"imap_port"`     // 993
	IMAPUsername string    `json:"imap_username"` // Usually same as email
	IMAPPassword string    `json:"-"`             // Encrypted in DB
	IMAPTLS      bool      `json:"imap_tls"`
	SMTPHost     string    `json:"smtp_host"`     // smtp.gmail.com
	SMTPPort     int       `json:"smtp_port"`     // 587
	SMTPUsername string    `json:"smtp_username"` // Usually same as email
	SMTPPassword string    `json:"-"`             // Encrypted in DB
	SMTPTLS      bool      `json:"smtp_tls"`
	Enabled      bool      `json:"enabled"`
	LastSync     time.Time `json:"last_sync"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`

	// Sync settings
	SyncMode     string `json:"sync_mode"`     // "idle" or "poll"
	PollInterval int    `json:"poll_interval"` // Polling interval in seconds (min 120, default 300)

	// Recipient validation: addresses (one per line) considered valid
	// recipients for this account in addition to Email itself.
	Aliases string `json:"aliases"`

	// Sync status
	LastSyncError     string `json:"last_sync_error,omitempty"`
	ConsecutiveErrors int    `json:"consecutive_errors"`

	// OAuth2 fields
	AuthType          string    `json:"auth_type"` // "password" or "oauth2_google"
	OAuthAccessToken  string    `json:"-"`         // Encrypted in DB
	OAuthRefreshToken string    `json:"-"`         // Encrypted in DB
	OAuthTokenExpiry  time.Time `json:"oauth_token_expiry"`
}

// HasSynced returns true if the account has been synced at least once
func (a *Account) HasSynced() bool {
	return !a.LastSync.IsZero()
}

// HasSyncError returns true if the last sync failed
func (a *Account) HasSyncError() bool {
	return a.LastSyncError != ""
}

// AccountLog represents a log entry for an account
type AccountLog struct {
	ID        int64     `json:"id"`
	AccountID int64     `json:"account_id"`
	Level     string    `json:"level"` // "info" or "error"
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// IsOAuth returns true if this account uses OAuth2 authentication
func (a *Account) IsOAuth() bool {
	return a.AuthType == "oauth2_google" || a.AuthType == "oauth2_microsoft"
}

// IsGoogleOAuth returns true if this account uses Google OAuth2
func (a *Account) IsGoogleOAuth() bool {
	return a.AuthType == "oauth2_google"
}

// IsMicrosoftOAuth returns true if this account uses Microsoft OAuth2
func (a *Account) IsMicrosoftOAuth() bool {
	return a.AuthType == "oauth2_microsoft"
}

// NeedsTokenRefresh returns true if OAuth token needs to be refreshed
func (a *Account) NeedsTokenRefresh() bool {
	if !a.IsOAuth() {
		return false
	}
	// Refresh 5 minutes before expiry
	return time.Now().Add(5 * time.Minute).After(a.OAuthTokenExpiry)
}

// GetAliases returns lowercased, trimmed alias addresses split from the Aliases
// field (separated by newlines, commas, or whitespace).
func (a *Account) GetAliases() []string {
	if a.Aliases == "" {
		return nil
	}
	// Split on common separators
	raw := strings.FieldsFunc(a.Aliases, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ';' || r == ' ' || r == '\t'
	})
	var out []string
	for _, s := range raw {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// IsKnownRecipient returns true if the given address matches the account's
// primary email or any of its aliases (case-insensitive).
func (a *Account) IsKnownRecipient(addr string) bool {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if addr == "" {
		return false
	}
	if addr == strings.ToLower(strings.TrimSpace(a.Email)) {
		return true
	}
	for _, alias := range a.GetAliases() {
		if addr == alias {
			return true
		}
	}
	return false
}

// Message represents an email message
type Message struct {
	ID                int64     `json:"id"`
	AccountID         int64     `json:"account_id"`
	UserID            int64     `json:"user_id"`
	MessageID         string    `json:"message_id"` // RFC 5322 Message-ID
	Subject           string    `json:"subject"`
	From              string    `json:"from"`
	To                string    `json:"to"`
	Cc                string    `json:"cc"`
	Bcc               string    `json:"bcc"`
	ReplyTo           string    `json:"reply_to"`
	Date              time.Time `json:"date"`
	Body              string    `json:"body"`        // Plain text body
	BodyHTML          string    `json:"body_html"`   // HTML body
	Attachments       int       `json:"attachments"` // Number of attachments
	Size              int64     `json:"size"`        // Size in bytes
	UID               uint32    `json:"uid"`         // IMAP UID
	FolderID          int64     `json:"folder_id"`
	Seen              bool      `json:"seen"`
	Flagged           bool      `json:"flagged"`
	Answered          bool      `json:"answered"`
	Draft             bool      `json:"draft"`
	Deleted           bool      `json:"deleted"`
	InReplyTo         string    `json:"in_reply_to"`        // Message-ID of parent message
	MessageReferences string    `json:"message_references"` // Thread references
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`

	// Spam filtering fields
	SpamScore   float64 `json:"spam_score"`
	SpamStatus  string  `json:"spam_status"`  // clean, suspicious, spam
	SpamReasons string  `json:"spam_reasons"` // JSON array of reasons
	IsSpam      bool    `json:"is_spam"`      // Message is in spam section
	SpamRuleID  *int64  `json:"spam_rule_id"` // Which user rule triggered spam

	// Soft delete (vault)
	SoftDeleted      bool       `json:"soft_deleted"`
	SoftDeletedAt    *time.Time `json:"soft_deleted_at,omitempty"`
	OriginalFolderID *int64     `json:"original_folder_id,omitempty"`

	// Calendar event link (for fake emails)
	CalendarEventID *int64 `json:"calendar_event_id,omitempty"`

	// Remote IMAP tracking (for bidirectional sync)
	RemoteUID    uint32 `json:"remote_uid"`    // UID on source IMAP server
	RemoteFolder string `json:"remote_folder"` // Folder path on source server (e.g., "INBOX")
}

// Folder represents a mail folder (INBOX, Sent, Drafts, etc.)
type Folder struct {
	ID          int64     `json:"id"`
	UserID      int64     `json:"user_id"`
	AccountID   int64     `json:"account_id"`   // 0 for virtual folders
	Name        string    `json:"name"`         // INBOX, Sent, Drafts
	Path        string    `json:"path"`         // Full IMAP path
	Type        string    `json:"type"`         // inbox, sent, drafts, trash, junk, archive, custom
	ParentID    int64     `json:"parent_id"`    // For hierarchical folders
	UIDNext     uint32    `json:"uid_next"`     // Next UID to assign
	UIDValidity uint32    `json:"uid_validity"` // IMAP UIDVALIDITY for incremental sync
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Attachment represents an email attachment
type Attachment struct {
	ID          int64     `json:"id"`
	MessageID   int64     `json:"message_id"`
	ContentID   string    `json:"content_id,omitempty"` // For inline images (cid:xxx)
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	Size        int       `json:"size"`
	IsInline    bool      `json:"is_inline"` // Embedded in HTML
	Data        []byte    `json:"-"`         // Binary content
	CreatedAt   time.Time `json:"created_at"`
}

// SyncStatus tracks synchronization status for each account
type SyncStatus struct {
	ID        int64     `json:"id"`
	AccountID int64     `json:"account_id"`
	LastSync  time.Time `json:"last_sync"`
	LastError string    `json:"last_error"`
	Status    string    `json:"status"` // idle, syncing, error
	UpdatedAt time.Time `json:"updated_at"`
}

// OutboxMessage represents a message waiting to be sent
type OutboxMessage struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	AccountID int64     `json:"account_id"` // Which account to send from
	From      string    `json:"from"`
	To        string    `json:"to"`  // Comma-separated
	Cc        string    `json:"cc"`  // Comma-separated
	Bcc       string    `json:"bcc"` // Comma-separated
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	BodyHTML  string    `json:"body_html"`
	RawEmail  []byte    `json:"-"`      // RFC 5322 formatted email
	Status    string    `json:"status"` // pending, sending, sent, failed
	Retries   int       `json:"retries"`
	LastError string    `json:"last_error"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	SentAt    time.Time `json:"sent_at"`
}

// Domain represents a local domain that the MX server accepts mail for
type Domain struct {
	ID        int64     `json:"id"`
	Domain    string    `json:"domain"`  // e.g. "example.com"
	UserID    int64     `json:"user_id"` // Owner of the domain
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// Mailbox represents a mailbox on a local domain
type Mailbox struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`    // User who receives mail
	DomainID  int64     `json:"domain_id"`  // Domain this mailbox belongs to
	LocalPart string    `json:"local_part"` // Part before @ (e.g. "info" for info@example.com)
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// FlagSyncEntry represents a pending flag change to sync to remote IMAP server
type FlagSyncEntry struct {
	ID           int64     `json:"id"`
	MessageID    int64     `json:"message_id"`
	AccountID    int64     `json:"account_id"`
	RemoteFolder string    `json:"remote_folder"`
	RemoteUID    uint32    `json:"remote_uid"`
	Seen         bool      `json:"seen"`
	Flagged      bool      `json:"flagged"`
	Answered     bool      `json:"answered"`
	Deleted      bool      `json:"deleted"`
	CreatedAt    time.Time `json:"created_at"`
}

// ContactSyncEntry represents a pending contact change to sync to remote CardDAV server
type ContactSyncEntry struct {
	ID            int64     `json:"id"`
	ContactID     int64     `json:"contact_id"`
	AddressBookID int64     `json:"address_book_id"`
	SourceID      int64     `json:"source_id"`
	UID           string    `json:"uid"`
	RemoteID      string    `json:"remote_id"`
	VCardData     string    `json:"-"`
	Operation     string    `json:"operation"` // create, update, delete
	CreatedAt     time.Time `json:"created_at"`
}

// CalendarEventSyncEntry represents a pending event change to sync to remote CalDAV server
type CalendarEventSyncEntry struct {
	ID         int64     `json:"id"`
	EventID    int64     `json:"event_id"`
	CalendarID int64     `json:"calendar_id"`
	SourceID   int64     `json:"source_id"`
	UID        string    `json:"uid"`
	RemoteID   string    `json:"remote_id"`
	ICalData   string    `json:"-"`
	Operation  string    `json:"operation"` // create, update, delete
	CreatedAt  time.Time `json:"created_at"`
}
