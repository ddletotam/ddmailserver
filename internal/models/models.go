package models

import (
	"strings"
	"time"

	"github.com/yourusername/mailserver/internal/timeutil"
)

// User represents a user of the mailserver
type User struct {
	ID              int64  `json:"id"`
	Username        string `json:"username"`
	PasswordHash    string `json:"-"` // never expose in JSON
	Email           string `json:"email,omitempty"`
	Language        string `json:"language,omitempty"`
	RecoveryKeyHash string `json:"-"` // never expose in JSON
	IsAdminFlag     bool   `json:"is_admin"`
	IsBannedFlag    bool   `json:"is_banned"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

// IsAdmin returns true if user is an administrator
func (u *User) IsAdmin() bool {
	return u.IsAdminFlag
}

// IsBanned returns true if user is banned from logging in
func (u *User) IsBanned() bool {
	return u.IsBannedFlag
}

// Account represents an external email account (Gmail, Outlook, etc.)
type Account struct {
	ID           int64  `json:"id"`
	UserID       int64  `json:"user_id"`
	Name         string `json:"name"`          // Friendly name for the account
	Email        string `json:"email"`         // Email address
	IMAPHost     string `json:"imap_host"`     // imap.gmail.com
	IMAPPort     int    `json:"imap_port"`     // 993
	IMAPUsername string `json:"imap_username"` // Usually same as email
	IMAPPassword string `json:"-"`             // Encrypted in DB
	IMAPTLS      bool   `json:"imap_tls"`
	SMTPHost     string `json:"smtp_host"`     // smtp.gmail.com
	SMTPPort     int    `json:"smtp_port"`     // 587
	SMTPUsername string `json:"smtp_username"` // Usually same as email
	SMTPPassword string `json:"-"`             // Encrypted in DB
	SMTPTLS      bool   `json:"smtp_tls"`
	Enabled      bool   `json:"enabled"`
	LastSync     int64  `json:"last_sync"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`

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
	AuthType          string `json:"auth_type"` // "password" or "oauth2_google"
	OAuthAccessToken  string `json:"-"`         // Encrypted in DB
	OAuthRefreshToken string `json:"-"`         // Encrypted in DB
	OAuthTokenExpiry  int64  `json:"oauth_token_expiry"`
}

// HasSynced returns true if the account has been synced at least once
func (a *Account) HasSynced() bool {
	return a.LastSync != 0
}

// HasSyncError returns true if the last sync failed
func (a *Account) HasSyncError() bool {
	return a.LastSyncError != ""
}

// OutboxAttachment represents a file attachment for an outbox message
type OutboxAttachment struct {
	ID              int64  `json:"id"`
	OutboxMessageID int64  `json:"outbox_message_id"`
	Filename        string `json:"filename"`
	ContentType     string `json:"content_type"`
	Size            int    `json:"size"`
	Data            []byte `json:"-"`
	ContentID       string `json:"content_id,omitempty"` // non-empty = inline image (cid:xxx)
	CreatedAt       int64  `json:"created_at"`
}

// AccountLog represents a log entry for an account
type AccountLog struct {
	ID        int64  `json:"id"`
	AccountID int64  `json:"account_id"`
	Level     string `json:"level"` // "info" or "error"
	Message   string `json:"message"`
	CreatedAt int64  `json:"created_at"`
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
	return timeutil.Now()+5*60*1000 > a.OAuthTokenExpiry
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
	ID                int64  `json:"id"`
	AccountID         int64  `json:"account_id"`
	UserID            int64  `json:"user_id"`
	MessageID         string `json:"message_id"`
	Subject           string `json:"subject"`
	From              string `json:"from"`
	To                string `json:"to"`
	Cc                string `json:"cc"`
	Bcc               string `json:"bcc"`
	ReplyTo           string `json:"reply_to"`
	Date              int64  `json:"date"`
	DateTZ            int16  `json:"date_tz"`
	Body              string `json:"body"`
	BodyHTML          string `json:"body_html"`
	RawEmail          []byte `json:"-"` // Original RFC-822 bytes
	Attachments       int    `json:"attachments"`
	Size              int64  `json:"size"`
	UID               uint32 `json:"uid"`
	FolderID          int64  `json:"folder_id"`
	Seen              bool   `json:"seen"`
	Flagged           bool   `json:"flagged"`
	Answered          bool   `json:"answered"`
	Draft             bool   `json:"draft"`
	Deleted           bool   `json:"deleted"`
	InReplyTo         string `json:"in_reply_to"`
	MessageReferences string `json:"message_references"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`

	// Spam filtering fields
	SpamScore   float64 `json:"spam_score"`
	SpamStatus  string  `json:"spam_status"`
	SpamReasons string  `json:"spam_reasons"`
	IsSpam      bool    `json:"is_spam"`
	SpamRuleID  *int64  `json:"spam_rule_id"`

	// Soft delete (vault)
	SoftDeleted      bool   `json:"soft_deleted"`
	SoftDeletedAt    *int64 `json:"soft_deleted_at,omitempty"`
	OriginalFolderID *int64 `json:"original_folder_id,omitempty"`

	// Calendar event link
	CalendarEventID *int64 `json:"calendar_event_id,omitempty"`

	// Remote IMAP tracking
	RemoteUID    uint32 `json:"remote_uid"`
	RemoteFolder string `json:"remote_folder"`
}

// DateAsTime returns the message date as time.Time (for IMAP/CalDAV compatibility).
func (m *Message) DateAsTime() time.Time {
	return timeutil.FromMs(m.Date)
}

// Folder represents a mail folder (INBOX, Sent, Drafts, etc.)
type Folder struct {
	ID          int64  `json:"id"`
	UserID      int64  `json:"user_id"`
	AccountID   int64  `json:"account_id"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"`
	ParentID    int64  `json:"parent_id"`
	UIDNext     uint32 `json:"uid_next"`
	UIDValidity uint32 `json:"uid_validity"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// Attachment represents an email attachment
type Attachment struct {
	ID          int64  `json:"id"`
	MessageID   int64  `json:"message_id"`
	ContentID   string `json:"content_id,omitempty"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	IsInline    bool   `json:"is_inline"`
	Data        []byte `json:"-"`
	CreatedAt   int64  `json:"created_at"`
}

// SyncStatus tracks synchronization status for each account
type SyncStatus struct {
	ID        int64  `json:"id"`
	AccountID int64  `json:"account_id"`
	LastSync  int64  `json:"last_sync"`
	LastError string `json:"last_error"`
	Status    string `json:"status"`
	UpdatedAt int64  `json:"updated_at"`
}

// OutboxMessage represents a message waiting to be sent
type OutboxMessage struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	AccountID int64  `json:"account_id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Cc        string `json:"cc"`
	Bcc       string `json:"bcc"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	BodyHTML  string `json:"body_html"`
	RawEmail  []byte `json:"-"`
	Status    string `json:"status"`
	Retries   int    `json:"retries"`
	LastError string `json:"last_error"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	SentAt    int64  `json:"sent_at"`
}

// Domain represents a local domain that the MX server accepts mail for
type Domain struct {
	ID        int64  `json:"id"`
	Domain    string `json:"domain"`
	UserID    int64  `json:"user_id"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
}

// Mailbox represents a mailbox on a local domain
type Mailbox struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	DomainID  int64  `json:"domain_id"`
	LocalPart string `json:"local_part"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
}

// FlagSyncEntry represents a pending flag change to sync to remote IMAP server
type FlagSyncEntry struct {
	ID           int64  `json:"id"`
	MessageID    int64  `json:"message_id"`
	AccountID    int64  `json:"account_id"`
	RemoteFolder string `json:"remote_folder"`
	RemoteUID    uint32 `json:"remote_uid"`
	Seen         bool   `json:"seen"`
	Flagged      bool   `json:"flagged"`
	Answered     bool   `json:"answered"`
	Deleted      bool   `json:"deleted"`
	CreatedAt    int64  `json:"created_at"`
}

// ContactSyncEntry represents a pending contact change to sync to remote CardDAV server
type ContactSyncEntry struct {
	ID            int64  `json:"id"`
	ContactID     int64  `json:"contact_id"`
	AddressBookID int64  `json:"address_book_id"`
	SourceID      int64  `json:"source_id"`
	UID           string `json:"uid"`
	RemoteID      string `json:"remote_id"`
	VCardData     string `json:"-"`
	Operation     string `json:"operation"`
	CreatedAt     int64  `json:"created_at"`
}

// CalendarEventSyncEntry represents a pending event change to sync to remote CalDAV server
type CalendarEventSyncEntry struct {
	ID         int64  `json:"id"`
	EventID    int64  `json:"event_id"`
	CalendarID int64  `json:"calendar_id"`
	SourceID   int64  `json:"source_id"`
	UID        string `json:"uid"`
	RemoteID   string `json:"remote_id"`
	ICalData   string `json:"-"`
	Operation  string `json:"operation"`
	CreatedAt  int64  `json:"created_at"`
	RetryCount int    `json:"retry_count"`
	LastError  string `json:"last_error,omitempty"`
}
