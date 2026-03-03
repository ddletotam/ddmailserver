package server

import (
	"bytes"
	"fmt"
	"log"
	"time"

	"github.com/emersion/go-imap"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
)

// Mailbox represents an IMAP mailbox
type Mailbox struct {
	name     string
	user     *User
	database *db.DB
	folderID int64 // Local folder ID
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

	status.Messages = uint32(len(messages))
	status.UidNext = uint32(len(messages) + 1)

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

	log.Printf("Mailbox %s status: %d messages, %d unseen, %d recent", m.name, status.Messages, status.Unseen, status.Recent)

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

	// Get messages from folder
	messages, err := m.database.GetMessagesByFolder(m.folderID, 10000, 0)
	if err != nil {
		return nil, err
	}

	// Simple search implementation
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

// CreateMessage creates a new message
func (m *Mailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	log.Printf("CreateMessage not implemented for mailbox %s", m.name)
	// TODO: Implement message creation if needed for APPEND command
	return errNotImplemented
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

		// Apply flag changes
		seen := msg.Seen
		flagged := msg.Flagged
		answered := msg.Answered
		deleted := msg.Deleted

		for _, flag := range flags {
			switch flag {
			case imap.SeenFlag:
				seen = (operation == imap.AddFlags || operation == imap.SetFlags)
			case imap.FlaggedFlag:
				flagged = (operation == imap.AddFlags || operation == imap.SetFlags)
			case imap.AnsweredFlag:
				answered = (operation == imap.AddFlags || operation == imap.SetFlags)
			case imap.DeletedFlag:
				deleted = (operation == imap.AddFlags || operation == imap.SetFlags)
			}
		}

		// Update in database
		err := m.database.UpdateMessageFlags(msg.ID, seen, flagged, answered, deleted)
		if err != nil {
			log.Printf("Failed to update flags for message %d: %v", msg.ID, err)
		} else {
			log.Printf("Updated flags for message %d: seen=%v, flagged=%v, answered=%v, deleted=%v",
				msg.ID, seen, flagged, answered, deleted)
		}
	}

	return nil
}

// CopyMessages copies messages to another mailbox
func (m *Mailbox) CopyMessages(uid bool, seqSet *imap.SeqSet, destName string) error {
	log.Printf("CopyMessages not implemented")
	// TODO: Implement message copying if needed
	return errNotImplemented
}

// Expunge permanently removes messages marked as deleted
func (m *Mailbox) Expunge() error {
	log.Printf("Expunge called for mailbox %s", m.name)

	// Get messages from folder
	messages, err := m.database.GetMessagesByFolder(m.folderID, 10000, 0)
	if err != nil {
		return err
	}

	// Delete messages marked as deleted
	for _, msg := range messages {
		if msg.Deleted {
			err := m.database.DeleteMessage(msg.ID)
			if err != nil {
				log.Printf("Failed to delete message %d: %v", msg.ID, err)
			}
		}
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

// buildMessageLiteral creates a literal for body section requests
func (m *Mailbox) buildMessageLiteral(msg *models.Message, section *imap.BodySectionName) imap.Literal {
	// Build RFC822 formatted message
	var buf bytes.Buffer

	// Write headers
	buf.WriteString(fmt.Sprintf("From: %s\r\n", msg.From))
	buf.WriteString(fmt.Sprintf("To: %s\r\n", msg.To))
	if msg.Cc != "" {
		buf.WriteString(fmt.Sprintf("Cc: %s\r\n", msg.Cc))
	}
	buf.WriteString(fmt.Sprintf("Subject: %s\r\n", msg.Subject))
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
