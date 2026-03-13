package parser

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"path/filepath"
	"strings"
	"time"

	gomail "github.com/emersion/go-message/mail"
)

// DangerousExtensions contains file extensions considered dangerous
var DangerousExtensions = []string{
	".exe", ".com", ".bat", ".cmd", ".scr", ".pif",
	".js", ".jse", ".vbs", ".vbe", ".wsf", ".wsh",
	".msi", ".msp", ".dll", ".cpl", ".hta",
	".ps1", ".psm1", ".psd1",
}

// Parser parses email messages
type Parser struct {
	maxDepth int // maximum recursion depth for embedded messages
}

// New creates a new parser
func New() *Parser {
	return &Parser{
		maxDepth: 5, // reasonable limit for nested messages
	}
}

// Parse parses an email from a reader
func (p *Parser) Parse(r io.Reader) (*ParsedMessage, error) {
	return p.parseWithDepth(r, 0)
}

// ParseBytes parses an email from bytes
func (p *Parser) ParseBytes(data []byte) (*ParsedMessage, error) {
	return p.Parse(bytes.NewReader(data))
}

// parseWithDepth parses with recursion depth tracking
func (p *Parser) parseWithDepth(r io.Reader, depth int) (*ParsedMessage, error) {
	if depth > p.maxDepth {
		return nil, fmt.Errorf("maximum nesting depth exceeded")
	}

	// Read all data first (we may need to re-read for CharsetReader)
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read message: %w", err)
	}

	msg := &ParsedMessage{
		RawData:    data,
		RawSize:    int64(len(data)),
		RawHeaders: make(map[string][]string),
		SpamStatus: SpamStatusClean,
	}

	// Create mail reader with charset support
	mr, err := gomail.CreateReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create mail reader: %w", err)
	}

	// Parse headers
	if err := p.parseHeaders(mr, msg); err != nil {
		// Continue even if headers fail - try to extract body
	}

	// Parse body parts
	if err := p.parseParts(mr, msg, depth); err != nil {
		// Continue even if body parsing fails
	}

	return msg, nil
}

// parseHeaders extracts headers from the message
func (p *Parser) parseHeaders(mr *gomail.Reader, msg *ParsedMessage) error {
	header := mr.Header

	// Extract standard headers
	msg.Subject, _ = header.Subject()
	msg.MessageID, _ = header.MessageID()
	msg.Date, _ = header.Date()

	// In-Reply-To and References
	if inReplyTo, err := header.MsgIDList("In-Reply-To"); err == nil && len(inReplyTo) > 0 {
		msg.InReplyTo = inReplyTo[0]
	}
	msg.References, _ = header.MsgIDList("References")

	// From
	if fromList, err := header.AddressList("From"); err == nil && len(fromList) > 0 {
		msg.From = fromList[0]
	}

	// To
	msg.To, _ = header.AddressList("To")

	// Cc
	msg.Cc, _ = header.AddressList("Cc")

	// Bcc
	msg.Bcc, _ = header.AddressList("Bcc")

	// Reply-To
	if replyTo, err := header.AddressList("Reply-To"); err == nil && len(replyTo) > 0 {
		msg.ReplyTo = replyTo[0]
	}

	// Store raw headers
	fields := header.Fields()
	for fields.Next() {
		key := fields.Key()
		value := fields.Value()
		msg.RawHeaders[key] = append(msg.RawHeaders[key], value)
	}

	return nil
}

// parseParts iterates through MIME parts
func (p *Parser) parseParts(mr *gomail.Reader, msg *ParsedMessage, depth int) error {
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("error reading part: %w", err)
		}

		if err := p.handlePart(part, msg, depth); err != nil {
			// Log but continue with other parts
			continue
		}
	}
	return nil
}

// handlePart processes a single MIME part
func (p *Parser) handlePart(part *gomail.Part, msg *ParsedMessage, depth int) error {
	switch h := part.Header.(type) {
	case *gomail.InlineHeader:
		return p.handleInlinePart(h, part.Body, msg, depth)
	case *gomail.AttachmentHeader:
		return p.handleAttachment(h, part.Body, msg)
	default:
		// Unknown header type - try to read content-type directly
		return p.handleUnknownPart(part, msg, depth)
	}
}

// handleInlinePart handles inline parts (body text, html, embedded messages)
func (p *Parser) handleInlinePart(h *gomail.InlineHeader, body io.Reader, msg *ParsedMessage, depth int) error {
	contentType, params, err := h.ContentType()
	if err != nil {
		contentType = "text/plain"
	}

	// Handle message/rfc822 - embedded email
	if strings.HasPrefix(contentType, "message/rfc822") {
		return p.handleEmbeddedMessage(body, msg, depth)
	}

	// Read body content
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("failed to read body: %w", err)
	}

	// Get charset and decode if needed
	charset := params["charset"]
	if charset == "" {
		charset = "utf-8"
	}

	decodedBody, err := DecodeCharset(charset, bodyBytes)
	if err != nil {
		// Use raw bytes if decoding fails
		decodedBody = string(bodyBytes)
	}

	// Store body based on content type
	switch {
	case strings.HasPrefix(contentType, "text/plain"):
		if msg.Body == "" {
			msg.Body = decodedBody
			msg.BodyCharset = charset
		}
	case strings.HasPrefix(contentType, "text/html"):
		if msg.BodyHTML == "" {
			msg.BodyHTML = decodedBody
		}
	}

	return nil
}

// handleAttachment handles attachment parts
func (p *Parser) handleAttachment(h *gomail.AttachmentHeader, body io.Reader, msg *ParsedMessage) error {
	// Get filename
	filename, err := h.Filename()
	if err != nil || filename == "" {
		filename = "unnamed"
	}

	// Get content type
	contentType, _, _ := h.ContentType()
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Check if it's inline (has Content-ID)
	contentID := h.Get("Content-ID")
	isInline := contentID != ""
	if isInline {
		// Clean up Content-ID: remove < and >
		contentID = strings.TrimPrefix(contentID, "<")
		contentID = strings.TrimSuffix(contentID, ">")
	}

	// Read attachment data
	data, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("failed to read attachment: %w", err)
	}

	// Check if extension is dangerous
	ext := strings.ToLower(filepath.Ext(filename))
	isDangerous := p.isDangerousExtension(ext)

	// Also check for double extensions like "file.pdf.exe"
	if !isDangerous {
		// Get the part before the last extension
		baseName := strings.TrimSuffix(filename, ext)
		if strings.Contains(baseName, ".") {
			isDangerous = p.isDangerousExtension(ext)
		}
	}

	attachment := ParsedAttachment{
		Filename:    filename,
		ContentType: contentType,
		Size:        int64(len(data)),
		Data:        data,
		ContentID:   contentID,
		IsInline:    isInline,
		IsDangerous: isDangerous,
	}

	msg.Attachments = append(msg.Attachments, attachment)
	return nil
}

// handleEmbeddedMessage handles message/rfc822 parts (email within email)
func (p *Parser) handleEmbeddedMessage(body io.Reader, msg *ParsedMessage, depth int) error {
	// Parse the embedded message recursively
	embedded, err := p.parseWithDepth(body, depth+1)
	if err != nil {
		return fmt.Errorf("failed to parse embedded message: %w", err)
	}

	msg.EmbeddedMessages = append(msg.EmbeddedMessages, embedded)
	return nil
}

// handleUnknownPart handles parts with unknown header types
func (p *Parser) handleUnknownPart(part *gomail.Part, msg *ParsedMessage, depth int) error {
	// Try to read content-type from the raw header
	contentType := part.Header.Get("Content-Type")
	if contentType == "" {
		return nil
	}

	mediaType, params, _ := mime.ParseMediaType(contentType)

	// Handle message/rfc822
	if strings.HasPrefix(mediaType, "message/rfc822") {
		return p.handleEmbeddedMessage(part.Body, msg, depth)
	}

	// Handle as potential attachment
	contentDisposition := part.Header.Get("Content-Disposition")
	if strings.HasPrefix(contentDisposition, "attachment") {
		// Parse disposition params for filename
		_, dispParams, _ := mime.ParseMediaType(contentDisposition)
		filename := dispParams["filename"]
		if filename == "" {
			filename = params["name"]
		}
		if filename == "" {
			filename = "unnamed"
		}

		data, err := io.ReadAll(part.Body)
		if err != nil {
			return err
		}

		ext := strings.ToLower(filepath.Ext(filename))

		attachment := ParsedAttachment{
			Filename:    filename,
			ContentType: mediaType,
			Size:        int64(len(data)),
			Data:        data,
			IsDangerous: p.isDangerousExtension(ext),
		}
		msg.Attachments = append(msg.Attachments, attachment)
	}

	return nil
}

// isDangerousExtension checks if the file extension is dangerous
func (p *Parser) isDangerousExtension(ext string) bool {
	ext = strings.ToLower(ext)
	for _, dangerous := range DangerousExtensions {
		if ext == dangerous {
			return true
		}
	}
	return false
}

// FormatAddress formats a mail.Address to string
func FormatAddress(addr *mail.Address) string {
	if addr == nil {
		return ""
	}
	if addr.Name != "" {
		return fmt.Sprintf("%s <%s>", addr.Name, addr.Address)
	}
	return addr.Address
}

// FormatAddressList formats a list of addresses
func FormatAddressList(addrs []*mail.Address) string {
	if len(addrs) == 0 {
		return ""
	}
	var result []string
	for _, addr := range addrs {
		result = append(result, FormatAddress(addr))
	}
	return strings.Join(result, ", ")
}

// GetBodyPreview returns a preview of the body text
func (msg *ParsedMessage) GetBodyPreview(maxLen int) string {
	body := msg.Body
	if body == "" {
		// Try to strip HTML if no plain text
		body = stripHTML(msg.BodyHTML)
	}

	// Clean up whitespace
	body = strings.TrimSpace(body)
	body = strings.ReplaceAll(body, "\r\n", " ")
	body = strings.ReplaceAll(body, "\n", " ")

	// Collapse multiple spaces
	for strings.Contains(body, "  ") {
		body = strings.ReplaceAll(body, "  ", " ")
	}

	if len(body) > maxLen {
		return body[:maxLen] + "..."
	}
	return body
}

// stripHTML removes HTML tags from a string (simple implementation)
func stripHTML(html string) string {
	var result strings.Builder
	inTag := false

	for _, r := range html {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			result.WriteRune(r)
		}
	}

	return result.String()
}

// GetDate returns the message date, falling back to current time if not set
func (msg *ParsedMessage) GetDate() time.Time {
	if msg.Date.IsZero() {
		return time.Now().UTC()
	}
	return msg.Date.UTC()
}

// GetMessageID returns the message ID, generating one if not present
func (msg *ParsedMessage) GetMessageID() string {
	if msg.MessageID != "" {
		return msg.MessageID
	}
	return fmt.Sprintf("<%d@generated.local>", time.Now().UnixNano())
}
