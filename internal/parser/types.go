package parser

import (
	"net/mail"
	"time"
)

// SpamStatus represents the spam classification of a message
type SpamStatus string

const (
	SpamStatusClean      SpamStatus = "clean"
	SpamStatusSuspicious SpamStatus = "suspicious"
	SpamStatusSpam       SpamStatus = "spam"
	SpamStatusQuarantine SpamStatus = "quarantine"
)

// AuthResult represents the result of an email authentication check
type AuthResult string

const (
	AuthResultPass     AuthResult = "pass"
	AuthResultFail     AuthResult = "fail"
	AuthResultSoftfail AuthResult = "softfail"
	AuthResultNeutral  AuthResult = "neutral"
	AuthResultNone     AuthResult = "none"
)

// AuthResults contains the results of SPF/DKIM/DMARC checks
type AuthResults struct {
	SPF      AuthResult
	DKIM     AuthResult
	DMARC    AuthResult
	SenderIP string
}

// ParsedAttachment represents an email attachment
type ParsedAttachment struct {
	Filename    string
	ContentType string
	Size        int64
	Data        []byte
	ContentID   string // for inline images (cid:)
	IsInline    bool
	IsDangerous bool // .exe, .js, etc.
}

// ParsedMessage represents a fully parsed email message
type ParsedMessage struct {
	// Headers
	MessageID  string
	Subject    string
	From       *mail.Address
	To         []*mail.Address
	Cc         []*mail.Address
	Bcc        []*mail.Address
	ReplyTo    *mail.Address
	Date       time.Time
	InReplyTo  string
	References []string

	// Body
	Body        string // text/plain (decoded)
	BodyHTML    string // text/html (decoded)
	BodyCharset string // original charset

	// Attachments
	Attachments []ParsedAttachment

	// Embedded messages (message/rfc822)
	EmbeddedMessages []*ParsedMessage

	// Spam analysis
	SpamScore   float64
	SpamReasons []string
	SpamStatus  SpamStatus

	// Authentication (for MX)
	AuthResults *AuthResults

	// Raw data
	RawHeaders map[string][]string
	RawSize    int64
	RawData    []byte // original message bytes
}

// HasAttachments returns true if the message has attachments
func (m *ParsedMessage) HasAttachments() bool {
	return len(m.Attachments) > 0
}

// HasEmbeddedMessages returns true if the message contains embedded messages
func (m *ParsedMessage) HasEmbeddedMessages() bool {
	return len(m.EmbeddedMessages) > 0
}

// GetDangerousAttachments returns attachments that are potentially dangerous
func (m *ParsedMessage) GetDangerousAttachments() []ParsedAttachment {
	var dangerous []ParsedAttachment
	for _, att := range m.Attachments {
		if att.IsDangerous {
			dangerous = append(dangerous, att)
		}
	}
	return dangerous
}

// TotalAttachmentSize returns the total size of all attachments
func (m *ParsedMessage) TotalAttachmentSize() int64 {
	var total int64
	for _, att := range m.Attachments {
		total += att.Size
	}
	return total
}
