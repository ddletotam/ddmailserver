package models

import "time"

// User represents a user of the mailserver
type User struct {
	ID              int64     `json:"id"`
	Username        string    `json:"username"`
	PasswordHash    string    `json:"-"` // never expose in JSON
	Email           string    `json:"email,omitempty"`
	Language        string    `json:"language,omitempty"`
	RecoveryKeyHash string    `json:"-"` // never expose in JSON
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// IsAdmin returns true if user is an administrator (first registered user)
func (u *User) IsAdmin() bool {
	return u.ID == 1
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

	// OAuth2 fields
	AuthType          string    `json:"auth_type"` // "password" or "oauth2_google"
	OAuthAccessToken  string    `json:"-"`         // Encrypted in DB
	OAuthRefreshToken string    `json:"-"`         // Encrypted in DB
	OAuthTokenExpiry  time.Time `json:"oauth_token_expiry"`
}

// IsOAuth returns true if this account uses OAuth2 authentication
func (a *Account) IsOAuth() bool {
	return a.AuthType == "oauth2_google"
}

// NeedsTokenRefresh returns true if OAuth token needs to be refreshed
func (a *Account) NeedsTokenRefresh() bool {
	if !a.IsOAuth() {
		return false
	}
	// Refresh 5 minutes before expiry
	return time.Now().Add(5 * time.Minute).After(a.OAuthTokenExpiry)
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
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	Data        []byte    `json:"-"` // Store in DB or filesystem
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
