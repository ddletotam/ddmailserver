package server

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"mime"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/parser"
	"github.com/yourusername/mailserver/internal/search"
)

// Mailbox represents an IMAP mailbox
type Mailbox struct {
	name          string
	user          *User
	database      *db.DB
	folderID      int64 // Local folder ID
	searchIndexer *search.Indexer
}

// Name returns the mailbox name
func (m *Mailbox) Name() string {
	return m.name
}

// Info returns mailbox information
func (m *Mailbox) Info() (*imap.MailboxInfo, error) {
	log.Printf("Getting info for mailbox %s", m.name)

	info := &imap.MailboxInfo{
		Attributes: []string{},
		Delimiter:  "/",
		Name:       m.name,
	}

	return info, nil
}

// Status returns mailbox status
func (m *Mailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	log.Printf("Getting status for mailbox %s (folder %d)", m.name, m.folderID)

	status := imap.NewMailboxStatus(m.name, items)

	// Get messages from folder
	messages, err := m.database.GetMessagesByFolder(m.folderID, 10000, 0)
	if err != nil {
		log.Printf("Failed to get messages: %v", err)
		return nil, err
	}

	// Get folder info for UIDNEXT and UIDVALIDITY
	folder, err := m.database.GetFolderByID(m.folderID)
	if err != nil {
		log.Printf("Failed to get folder: %v", err)
		// Fallback to calculated values
		status.UidNext = uint32(len(messages) + 1)
		status.UidValidity = 1
	} else {
		status.UidNext = folder.UIDNext
		status.UidValidity = folder.UIDValidity
		// If UIDVALIDITY is 0, set it to 1 (must be non-zero)
		if status.UidValidity == 0 {
			status.UidValidity = 1
		}
	}

	status.Messages = uint32(len(messages))

	// Count unseen messages
	unseenCount := uint32(0)
	for _, msg := range messages {
		if !msg.Seen {
			unseenCount++
		}
	}
	status.Unseen = unseenCount

	// Count recent messages (last 24 hours)
	recentCount := uint32(0)
	yesterday := time.Now().Add(-24 * time.Hour)
	for _, msg := range messages {
		if msg.CreatedAt.After(yesterday) {
			recentCount++
		}
	}
	status.Recent = recentCount

	// Set flags that this mailbox supports
	status.Flags = []string{imap.SeenFlag, imap.AnsweredFlag, imap.FlaggedFlag, imap.DeletedFlag, imap.DraftFlag}
	// Set permanent flags - tells client which flags can be changed permanently
	// \* means client can create custom flags (we don't support this, so we omit it)
	status.PermanentFlags = []string{imap.SeenFlag, imap.AnsweredFlag, imap.FlaggedFlag, imap.DeletedFlag, imap.DraftFlag}

	log.Printf("Mailbox %s status: %d messages, %d unseen, %d recent, uidnext=%d, uidvalidity=%d, permanentflags=%v",
		m.name, status.Messages, status.Unseen, status.Recent, status.UidNext, status.UidValidity, status.PermanentFlags)

	return status, nil
}

// SetSubscribed sets the mailbox subscription status
func (m *Mailbox) SetSubscribed(subscribed bool) error {
	log.Printf("SetSubscribed not implemented for mailbox %s", m.name)
	// TODO: Implement subscription tracking if needed
	return nil
}

// Check performs a checkpoint of the mailbox
func (m *Mailbox) Check() error {
	log.Printf("Check called for mailbox %s", m.name)
	// Nothing to do for now
	return nil
}

// ListMessages returns a list of messages
func (m *Mailbox) ListMessages(uid bool, seqSet *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	defer close(ch)

	log.Printf("Listing messages for mailbox %s (uid: %v, seqset: %v)", m.name, uid, seqSet)

	// Get messages from folder
	messages, err := m.database.GetMessagesByFolder(m.folderID, 10000, 0)
	if err != nil {
		log.Printf("Failed to get messages: %v", err)
		return err
	}

	log.Printf("Found %d messages in mailbox %s (folder %d)", len(messages), m.name, m.folderID)

	// Convert to IMAP messages and send to channel
	for seqNum, msg := range messages {
		// Check if this message matches the sequence set
		id := uint32(seqNum + 1)
		if uid {
			id = msg.UID
		}

		if !seqSet.Contains(id) {
			continue
		}

		imapMsg := m.convertToIMAPMessage(msg, uint32(seqNum+1), items)
		ch <- imapMsg
	}

	return nil
}

// SearchMessages searches for messages
func (m *Mailbox) SearchMessages(uid bool, criteria *imap.SearchCriteria) ([]uint32, error) {
	log.Printf("Searching messages in mailbox %s (uid: %v)", m.name, uid)

	// Extract text query from criteria for Meilisearch
	textQuery := m.extractTextQuery(criteria)

	var messages []*models.Message
	var err error

	// If we have a text query and Meilisearch is available, use it
	if textQuery != "" && m.searchIndexer != nil {
		log.Printf("Using Meilisearch for text query: %s", textQuery)
		searchResult, searchErr := m.searchIndexer.SearchInFolder(m.user.userID, m.folderID, textQuery, 10000, 0)
		if searchErr == nil && searchResult != nil {
			// Convert search results to message IDs and fetch from DB
			ids := make([]int64, 0, len(searchResult.Hits))
			for _, hit := range searchResult.Hits {
				ids = append(ids, hit.ID)
			}
			messages, err = m.database.GetMessagesByIDs(ids)
			if err != nil {
				log.Printf("Failed to get messages by IDs, falling back to DB search: %v", err)
				messages, err = m.database.GetMessagesByFolder(m.folderID, 10000, 0)
			}
		} else {
			log.Printf("Meilisearch failed, falling back to DB: %v", searchErr)
			messages, err = m.database.GetMessagesByFolder(m.folderID, 10000, 0)
		}
	} else {
		// No text query or no Meilisearch - use database
		messages, err = m.database.GetMessagesByFolder(m.folderID, 10000, 0)
	}

	if err != nil {
		return nil, err
	}

	// Apply non-text criteria filters
	var results []uint32
	for seqNum, msg := range messages {
		if m.matchesCriteria(msg, criteria) {
			if uid {
				results = append(results, msg.UID)
			} else {
				results = append(results, uint32(seqNum+1))
			}
		}
	}

	log.Printf("Search found %d messages", len(results))
	return results, nil
}

// extractTextQuery extracts text search terms from criteria
func (m *Mailbox) extractTextQuery(criteria *imap.SearchCriteria) string {
	var parts []string

	// Check for TEXT criterion
	for _, text := range criteria.Text {
		if text != "" {
			parts = append(parts, text)
		}
	}

	// Check for BODY criterion
	for _, body := range criteria.Body {
		if body != "" {
			parts = append(parts, body)
		}
	}

	// Check Header criteria (From, To, Subject, etc.)
	for key, values := range criteria.Header {
		for _, v := range values {
			if v != "" {
				switch strings.ToUpper(key) {
				case "FROM", "TO", "CC", "SUBJECT":
					parts = append(parts, v)
				}
			}
		}
	}

	return strings.Join(parts, " ")
}

// CreateMessage creates a new message (APPEND command)
func (m *Mailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	log.Printf("CreateMessage called for mailbox %s with %d flags", m.name, len(flags))

	// Read the message body
	data, err := io.ReadAll(body)
	if err != nil {
		log.Printf("CreateMessage: failed to read body: %v", err)
		return fmt.Errorf("failed to read message body: %w", err)
	}

	// Parse the message
	p := parser.New()
	parsed, err := p.ParseBytes(data)
	if err != nil {
		log.Printf("CreateMessage: failed to parse message: %v", err)
		// Continue with minimal info even if parsing fails
		parsed = &parser.ParsedMessage{}
	}

	// Get next UID
	nextUID, err := m.database.GetNextUIDForFolder(m.folderID)
	if err != nil {
		log.Printf("CreateMessage: failed to get next UID: %v", err)
		return fmt.Errorf("failed to get next UID: %w", err)
	}

	// Extract flags
	seen := false
	flagged := false
	answered := false
	draft := false
	deleted := false
	for _, flag := range flags {
		switch flag {
		case imap.SeenFlag:
			seen = true
		case imap.FlaggedFlag:
			flagged = true
		case imap.AnsweredFlag:
			answered = true
		case imap.DraftFlag:
			draft = true
		case imap.DeletedFlag:
			deleted = true
		}
	}

	// Use provided date or parsed date
	msgDate := date
	if msgDate.IsZero() {
		msgDate = parsed.GetDate()
	}
	if msgDate.IsZero() {
		msgDate = time.Now()
	}

	// Create message
	msg := &models.Message{
		AccountID: 0, // Local message
		UserID:    m.user.userID,
		FolderID:  m.folderID,
		MessageID: parsed.GetMessageID(),
		Subject:   parser.SanitizeUTF8(parsed.Subject),
		From:      parser.SanitizeUTF8(parser.FormatAddress(parsed.From)),
		To:        parser.SanitizeUTF8(parser.FormatAddressList(parsed.To)),
		Cc:        parser.SanitizeUTF8(parser.FormatAddressList(parsed.Cc)),
		ReplyTo:   parser.SanitizeUTF8(parser.FormatAddress(parsed.ReplyTo)),
		Date:      msgDate,
		Body:      parser.SanitizeUTF8(parsed.Body),
		BodyHTML:  parser.SanitizeUTF8(parsed.BodyHTML),
		Size:      int64(len(data)),
		UID:       nextUID,
		Seen:      seen,
		Flagged:   flagged,
		Answered:  answered,
		Draft:     draft,
		Deleted:   deleted,
		InReplyTo: parser.SanitizeUTF8(parsed.InReplyTo),
	}

	if err := m.database.CreateMessage(msg); err != nil {
		log.Printf("CreateMessage: failed to save message: %v", err)
		return fmt.Errorf("failed to save message: %w", err)
	}

	log.Printf("CreateMessage: saved message %d with UID %d to mailbox %s", msg.ID, msg.UID, m.name)
	return nil
}

// UpdateMessagesFlags updates message flags
func (m *Mailbox) UpdateMessagesFlags(uid bool, seqSet *imap.SeqSet, operation imap.FlagsOp, flags []string) error {
	log.Printf("UpdateMessagesFlags called: mailbox=%s, uid=%v, seqSet=%v, operation=%v, flags=%v",
		m.name, uid, seqSet, operation, flags)

	// Get messages from folder
	messages, err := m.database.GetMessagesByFolder(m.folderID, 10000, 0)
	if err != nil {
		return err
	}

	// Update matching messages
	for seqNum, msg := range messages {
		id := uint32(seqNum + 1)
		if uid {
			id = msg.UID
		}

		if !seqSet.Contains(id) {
			continue
		}

		// SetFlags replaces all flags, AddFlags/RemoveFlags modify existing
		var seen, flagged, answered, deleted bool
		if operation == imap.SetFlags {
			seen, flagged, answered, deleted = false, false, false, false
		} else {
			seen = msg.Seen
			flagged = msg.Flagged
			answered = msg.Answered
			deleted = msg.Deleted
		}

		for _, flag := range flags {
			switch flag {
			case imap.SeenFlag:
				if operation == imap.RemoveFlags {
					seen = false
				} else {
					seen = true
				}
			case imap.FlaggedFlag:
				if operation == imap.RemoveFlags {
					flagged = false
				} else {
					flagged = true
				}
			case imap.AnsweredFlag:
				if operation == imap.RemoveFlags {
					answered = false
				} else {
					answered = true
				}
			case imap.DeletedFlag:
				if operation == imap.RemoveFlags {
					deleted = false
				} else {
					deleted = true
				}
			}
		}

		err := m.database.UpdateMessageFlags(msg.ID, seen, flagged, answered, deleted)
		if err != nil {
			log.Printf("Failed to update flags for message %d: %v", msg.ID, err)
		} else {
			log.Printf("Updated flags for message %d: seen=%v, flagged=%v, answered=%v, deleted=%v",
				msg.ID, seen, flagged, answered, deleted)

			// Queue for reverse sync to external IMAP server (if applicable)
			// Only queue if message has remote UID (external account, not local delivery)
			if msg.AccountID > 0 && msg.RemoteUID > 0 {
				if err := m.database.QueueFlagSync(msg.ID, msg.AccountID, msg.RemoteFolder, msg.RemoteUID, seen, flagged, answered, deleted); err != nil {
					log.Printf("Failed to queue flag sync for message %d: %v", msg.ID, err)
				}
			}
		}
	}

	return nil
}

// CopyMessages copies messages to another mailbox
func (m *Mailbox) CopyMessages(uid bool, seqSet *imap.SeqSet, destName string) error {
	log.Printf("CopyMessages called: uid=%v, seqSet=%v, destName=%s", uid, seqSet, destName)

	// Get or create destination folder
	destFolder, err := m.database.GetOrCreateFolderByNameAndUser(m.user.userID, destName, "custom")
	if err != nil {
		log.Printf("CopyMessages: failed to get/create destination folder %s: %v", destName, err)
		return fmt.Errorf("failed to get destination folder: %w", err)
	}

	// Get messages from source folder
	messages, err := m.database.GetMessagesByFolder(m.folderID, 10000, 0)
	if err != nil {
		log.Printf("CopyMessages: failed to get messages from source folder: %v", err)
		return err
	}

	// Copy matching messages
	copiedCount := 0
	for seqNum, msg := range messages {
		id := uint32(seqNum + 1)
		if uid {
			id = msg.UID
		}

		if !seqSet.Contains(id) {
			continue
		}

		// Copy message to destination folder
		newUID, err := m.database.CopyMessageToFolder(msg.ID, destFolder.ID)
		if err != nil {
			log.Printf("CopyMessages: failed to copy message %d to folder %s: %v", msg.ID, destName, err)
			// Continue with other messages even if one fails
			continue
		}

		copiedCount++
		log.Printf("CopyMessages: copied message %d (UID %d) to folder %s with new UID %d",
			msg.ID, msg.UID, destName, newUID)
	}

	log.Printf("CopyMessages: copied %d messages to %s", copiedCount, destName)
	return nil
}

// MoveMessages moves messages to another mailbox (MOVE extension)
func (m *Mailbox) MoveMessages(uid bool, seqSet *imap.SeqSet, destName string) error {
	log.Printf("MoveMessages called: uid=%v, seqSet=%v, destName=%s", uid, seqSet, destName)

	// Get or create destination folder
	destFolder, err := m.database.GetOrCreateFolderByNameAndUser(m.user.userID, destName, "custom")
	if err != nil {
		log.Printf("MoveMessages: failed to get/create destination folder %s: %v", destName, err)
		return fmt.Errorf("failed to get destination folder: %w", err)
	}

	// Get messages from source folder
	messages, err := m.database.GetMessagesByFolder(m.folderID, 10000, 0)
	if err != nil {
		log.Printf("MoveMessages: failed to get messages from source folder: %v", err)
		return err
	}

	// Move matching messages
	movedCount := 0
	var movedMsgIDs []int64
	for seqNum, msg := range messages {
		id := uint32(seqNum + 1)
		if uid {
			id = msg.UID
		}

		if !seqSet.Contains(id) {
			continue
		}

		// Copy message to destination folder
		newUID, err := m.database.CopyMessageToFolder(msg.ID, destFolder.ID)
		if err != nil {
			log.Printf("MoveMessages: failed to copy message %d to folder %s: %v", msg.ID, destName, err)
			continue
		}

		movedMsgIDs = append(movedMsgIDs, msg.ID)
		movedCount++
		log.Printf("MoveMessages: moved message %d (UID %d) to folder %s with new UID %d",
			msg.ID, msg.UID, destName, newUID)
	}

	// Delete original messages from source folder
	if len(movedMsgIDs) > 0 {
		for _, msgID := range movedMsgIDs {
			if err := m.database.DeleteMessage(msgID); err != nil {
				log.Printf("MoveMessages: failed to delete original message %d: %v", msgID, err)
			}
		}
	}

	log.Printf("MoveMessages: moved %d messages to %s", movedCount, destName)
	return nil
}

// Expunge removes messages marked as deleted
// For Trash folder: permanently delete (hard delete)
// For other folders: soft delete (move to vault)
func (m *Mailbox) Expunge() error {
	log.Printf("Expunge called for mailbox %s (folder_id=%d)", m.name, m.folderID)

	// Get folder info to check if it's Trash
	folder, err := m.database.GetFolderByID(m.folderID)
	if err != nil {
		log.Printf("Failed to get folder info: %v", err)
		return err
	}

	isTrash := folder.Type == "trash" || strings.ToLower(folder.Name) == "trash" ||
		strings.ToLower(folder.Name) == "deleted" || strings.ToLower(folder.Name) == "deleted items"

	// Get messages marked as deleted (for expunge)
	deletedMessages, err := m.database.GetDeletedMessagesByFolder(m.folderID)
	if err != nil {
		return err
	}

	// Collect UIDs of messages marked as deleted
	var deletedUIDs []uint32
	for _, msg := range deletedMessages {
		deletedUIDs = append(deletedUIDs, msg.UID)
	}

	if len(deletedUIDs) == 0 {
		log.Printf("No messages to expunge in mailbox %s", m.name)
		return nil
	}

	if isTrash {
		// Trash folder: hard delete permanently
		count, err := m.database.HardDeleteMessagesByUIDs(m.folderID, deletedUIDs)
		if err != nil {
			log.Printf("Failed to hard delete messages: %v", err)
			return err
		}
		log.Printf("Hard deleted %d messages from Trash", count)
	} else {
		// Other folders: soft delete (move to vault)
		count, err := m.database.SoftDeleteMessagesByUIDs(m.folderID, deletedUIDs)
		if err != nil {
			log.Printf("Failed to soft delete messages: %v", err)
			return err
		}
		log.Printf("Soft deleted %d messages to vault from mailbox %s", count, m.name)
	}

	return nil
}

// Helper function to convert database message to IMAP message
func (m *Mailbox) convertToIMAPMessage(msg *models.Message, seqNum uint32, items []imap.FetchItem) *imap.Message {
	imapMsg := imap.NewMessage(seqNum, items)

	for _, item := range items {
		switch item {
		case imap.FetchEnvelope:
			imapMsg.Envelope = &imap.Envelope{
				Date:      msg.Date,
				Subject:   msg.Subject,
				From:      parseAddresses(msg.From),
				Sender:    parseAddresses(msg.From),
				ReplyTo:   parseAddresses(msg.ReplyTo),
				To:        parseAddresses(msg.To),
				Cc:        parseAddresses(msg.Cc),
				Bcc:       parseAddresses(msg.Bcc),
				InReplyTo: msg.InReplyTo,
				MessageId: msg.MessageID,
			}

		case imap.FetchBody, imap.FetchBodyStructure:
			hasPlain := msg.Body != ""
			hasHTML := msg.BodyHTML != ""

			if hasPlain && hasHTML {
				// Multipart/alternative structure
				imapMsg.BodyStructure = &imap.BodyStructure{
					MIMEType:    "multipart",
					MIMESubType: "alternative",
					Params:      map[string]string{"boundary": fmt.Sprintf("----=_Part_%d", msg.ID)},
					Parts: []*imap.BodyStructure{
						{
							MIMEType:    "text",
							MIMESubType: "plain",
							Params:      map[string]string{"charset": "utf-8"},
							Size:        uint32(len(msg.Body)),
						},
						{
							MIMEType:    "text",
							MIMESubType: "html",
							Params:      map[string]string{"charset": "utf-8"},
							Size:        uint32(len(msg.BodyHTML)),
						},
					},
				}
			} else if hasHTML {
				imapMsg.BodyStructure = &imap.BodyStructure{
					MIMEType:    "text",
					MIMESubType: "html",
					Params:      map[string]string{"charset": "utf-8"},
					Size:        uint32(len(msg.BodyHTML)),
				}
			} else {
				imapMsg.BodyStructure = &imap.BodyStructure{
					MIMEType:    "text",
					MIMESubType: "plain",
					Params:      map[string]string{"charset": "utf-8"},
					Size:        uint32(len(msg.Body)),
				}
			}

		case imap.FetchFlags:
			var flags []string
			if msg.Seen {
				flags = append(flags, imap.SeenFlag)
			}
			if msg.Flagged {
				flags = append(flags, imap.FlaggedFlag)
			}
			if msg.Answered {
				flags = append(flags, imap.AnsweredFlag)
			}
			if msg.Deleted {
				flags = append(flags, imap.DeletedFlag)
			}
			if msg.Draft {
				flags = append(flags, imap.DraftFlag)
			}
			imapMsg.Flags = flags

		case imap.FetchInternalDate:
			imapMsg.InternalDate = msg.Date

		case imap.FetchUid:
			imapMsg.Uid = msg.UID

		case imap.FetchRFC822Size:
			imapMsg.Size = uint32(msg.Size)

		case imap.FetchRFC822, imap.FetchRFC822Header, imap.FetchRFC822Text:
			// Handle RFC822 fetches
			section, _ := imap.ParseBodySectionName(item)
			if section != nil {
				literal := m.buildMessageLiteral(msg, section)
				imapMsg.Body[section] = literal
			}

		default:
			// Handle BODY[] section requests
			section, err := imap.ParseBodySectionName(item)
			if err == nil && section != nil {
				literal := m.buildMessageLiteral(msg, section)
				imapMsg.Body[section] = literal
			}
		}
	}

	return imapMsg
}

// encodeHeader encodes a header value using RFC 2047 if it contains non-ASCII
func encodeHeader(s string) string {
	// Check if encoding is needed
	needsEncoding := false
	for _, r := range s {
		if r > 127 {
			needsEncoding = true
			break
		}
	}
	if !needsEncoding {
		return s
	}
	return mime.BEncoding.Encode("UTF-8", s)
}

// encodeAddressHeader encodes an address header like "Name <email@example.com>"
func encodeAddressHeader(addr string) string {
	// Find the angle brackets
	ltIdx := strings.LastIndex(addr, "<")
	if ltIdx <= 0 {
		// No name part, just email
		return addr
	}

	name := strings.TrimSpace(addr[:ltIdx])
	email := addr[ltIdx:] // includes < and >

	// Encode the name part if needed
	encodedName := encodeHeader(name)
	return encodedName + " " + email
}

// buildMessageLiteral creates a literal for body section requests
func (m *Mailbox) buildMessageLiteral(msg *models.Message, section *imap.BodySectionName) imap.Literal {
	// Build RFC822 formatted message
	var buf bytes.Buffer

	// Write headers with proper RFC 2047 encoding
	buf.WriteString(fmt.Sprintf("From: %s\r\n", encodeAddressHeader(msg.From)))
	buf.WriteString(fmt.Sprintf("To: %s\r\n", encodeAddressHeader(msg.To)))
	if msg.Cc != "" {
		buf.WriteString(fmt.Sprintf("Cc: %s\r\n", encodeAddressHeader(msg.Cc)))
	}
	buf.WriteString(fmt.Sprintf("Subject: %s\r\n", encodeHeader(msg.Subject)))
	buf.WriteString(fmt.Sprintf("Date: %s\r\n", msg.Date.Format("Mon, 02 Jan 2006 15:04:05 -0700")))
	buf.WriteString(fmt.Sprintf("Message-ID: %s\r\n", msg.MessageID))
	buf.WriteString("MIME-Version: 1.0\r\n")

	// Write body (unless only headers requested)
	specifier := section.Specifier
	if specifier != imap.HeaderSpecifier {
		// Determine content type and body based on what's available
		hasPlain := msg.Body != ""
		hasHTML := msg.BodyHTML != ""

		if hasPlain && hasHTML {
			// Multipart message with both text and HTML
			boundary := fmt.Sprintf("----=_Part_%d", msg.ID)
			buf.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary))
			buf.WriteString("\r\n")

			// Plain text part
			buf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
			buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
			buf.WriteString("Content-Transfer-Encoding: 8bit\r\n")
			buf.WriteString("\r\n")
			buf.WriteString(msg.Body)
			buf.WriteString("\r\n")

			// HTML part
			buf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
			buf.WriteString("Content-Type: text/html; charset=utf-8\r\n")
			buf.WriteString("Content-Transfer-Encoding: 8bit\r\n")
			buf.WriteString("\r\n")
			buf.WriteString(msg.BodyHTML)
			buf.WriteString("\r\n")

			// End boundary
			buf.WriteString(fmt.Sprintf("--%s--\r\n", boundary))
		} else if hasHTML {
			// Only HTML available
			buf.WriteString("Content-Type: text/html; charset=utf-8\r\n")
			buf.WriteString("\r\n")
			buf.WriteString(msg.BodyHTML)
		} else {
			// Only plain text (or empty)
			buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
			buf.WriteString("\r\n")
			buf.WriteString(msg.Body)
		}
	} else {
		// Headers only - just add content-type header and end headers
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		buf.WriteString("\r\n")
	}

	return bytes.NewReader(buf.Bytes())
}

// Helper function to match message against search criteria
func (m *Mailbox) matchesCriteria(msg *models.Message, criteria *imap.SearchCriteria) bool {
	// Simple implementation - just check flags for now
	// TODO: Implement full search criteria

	if criteria.WithoutFlags != nil {
		for _, flag := range criteria.WithoutFlags {
			if flag == imap.SeenFlag && msg.Seen {
				return false
			}
			if flag == imap.FlaggedFlag && msg.Flagged {
				return false
			}
		}
	}

	if criteria.WithFlags != nil {
		for _, flag := range criteria.WithFlags {
			if flag == imap.SeenFlag && !msg.Seen {
				return false
			}
			if flag == imap.FlaggedFlag && !msg.Flagged {
				return false
			}
		}
	}

	return true
}

// Helper function to parse address strings
func parseAddresses(addrStr string) []*imap.Address {
	if addrStr == "" {
		return nil
	}

	// Simple parsing - just return one address
	// TODO: Implement proper address parsing
	return []*imap.Address{
		{
			PersonalName: "",
			MailboxName:  addrStr,
			HostName:     "",
		},
	}
}
