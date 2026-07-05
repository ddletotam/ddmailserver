package server

import (
	"bytes"
	"encoding/base64"
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
	"github.com/yourusername/mailserver/internal/timeutil"
)

// Mailbox represents an IMAP mailbox
type Mailbox struct {
	name          string
	folderType    string // inbox, sent, drafts, trash, junk, archive, custom
	user          *User
	database      *db.DB
	folderID      int64 // Local folder ID
	searchIndexer *search.Indexer
	bodyCache     *bodyCache
	backend       *Backend // for pushing untagged EXPUNGE/FETCH updates to sessions
}

// clientSeqNums maps the given UIDs (any order) to their 1-based positions
// in the client-facing sequence of this folder: the UID-ordered union of
// visible and \Deleted-flagged (not yet expunged) messages. The visible
// list alone is wrong here — clients still count \Deleted-flagged rows
// until they receive EXPUNGE. Returned seqnums are ascending.
func (m *Mailbox) clientSeqNums(uids []uint32) []uint32 {
	want := make(map[uint32]bool, len(uids))
	for _, u := range uids {
		want[u] = true
	}

	visible, err := m.database.GetMessagesByFolderMeta(m.folderID, 1000000, 0)
	if err != nil {
		log.Printf("clientSeqNums: failed to load visible messages: %v", err)
		return nil
	}
	flagged, err := m.database.GetDeletedMessagesByFolder(m.folderID)
	if err != nil {
		log.Printf("clientSeqNums: failed to load deleted-flagged messages: %v", err)
		return nil
	}

	// Merge the two UID-ascending lists, recording positions of wanted UIDs.
	seqNums := make([]uint32, 0, len(uids))
	i, j := 0, 0
	var pos uint32
	for i < len(visible) || j < len(flagged) {
		pos++
		var uid uint32
		if j >= len(flagged) || (i < len(visible) && visible[i].UID < flagged[j].UID) {
			uid = visible[i].UID
			i++
		} else {
			uid = flagged[j].UID
			j++
		}
		if want[uid] {
			seqNums = append(seqNums, pos)
		}
	}
	return seqNums
}

// notifyExpungeDesc pushes untagged EXPUNGE updates for ascending seqnums,
// sending them in descending order so each seqnum stays valid while the
// client applies the preceding (higher) ones.
func (m *Mailbox) notifyExpungeDesc(seqNumsAsc []uint32) {
	if m.backend == nil || len(seqNumsAsc) == 0 {
		return
	}
	desc := make([]uint32, 0, len(seqNumsAsc))
	for i := len(seqNumsAsc) - 1; i >= 0; i-- {
		desc = append(desc, seqNumsAsc[i])
	}
	m.backend.notifyExpunge(m.user.username, m.name, desc)
}

// flagList converts stored flag booleans to an IMAP flag list.
func flagList(seen, flagged, answered, deleted bool) []string {
	var flags []string
	if seen {
		flags = append(flags, imap.SeenFlag)
	}
	if flagged {
		flags = append(flags, imap.FlaggedFlag)
	}
	if answered {
		flags = append(flags, imap.AnsweredFlag)
	}
	if deleted {
		flags = append(flags, imap.DeletedFlag)
	}
	return flags
}

// Name returns the mailbox name
func (m *Mailbox) Name() string {
	return m.name
}

// Info returns mailbox information with RFC 6154 Special-Use attributes
func (m *Mailbox) Info() (*imap.MailboxInfo, error) {
	var attrs []string
	switch m.folderType {
	case "inbox":
		// INBOX is implied by name, but some clients check attribute
	case "sent":
		attrs = append(attrs, "\\Sent")
	case "trash":
		attrs = append(attrs, "\\Trash")
	case "drafts":
		attrs = append(attrs, "\\Drafts")
	case "junk":
		attrs = append(attrs, "\\Junk")
	case "archive":
		attrs = append(attrs, "\\Archive")
	}

	return &imap.MailboxInfo{
		Attributes: attrs,
		Delimiter:  "/",
		Name:       m.name,
	}, nil
}

// Status returns mailbox status
func (m *Mailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	log.Printf("Getting status for mailbox %s (folder %d)", m.name, m.folderID)

	status := imap.NewMailboxStatus(m.name, items)

	// Count messages via a single COUNT query — never load bodies just to count.
	yesterdayMs := timeutil.Now() - 24*60*60*1000
	total, unseen, recent, err := m.database.GetFolderStatusCounts(m.folderID, yesterdayMs)
	if err != nil {
		log.Printf("Failed to get folder status counts: %v", err)
		return nil, err
	}

	// Get folder info for UIDNEXT and UIDVALIDITY
	folder, err := m.database.GetFolderByID(m.folderID)
	if err != nil {
		log.Printf("Failed to get folder: %v", err)
		// Fallback to calculated values
		status.UidNext = total + 1
		status.UidValidity = 1
	} else {
		status.UidNext = folder.UIDNext
		status.UidValidity = folder.UIDValidity
		// If UIDVALIDITY is 0, set it to 1 (must be non-zero)
		if status.UidValidity == 0 {
			status.UidValidity = 1
		}
	}

	status.Messages = total
	status.Unseen = unseen
	status.Recent = recent

	// Set flags that this mailbox supports
	status.Flags = []string{imap.SeenFlag, imap.AnsweredFlag, imap.FlaggedFlag, imap.DeletedFlag, imap.DraftFlag}
	// Set permanent flags - tells client which flags can be changed permanently
	// \* means client can create custom flags (we don't support this, so we omit it)
	status.PermanentFlags = []string{imap.SeenFlag, imap.AnsweredFlag, imap.FlaggedFlag, imap.DeletedFlag, imap.DraftFlag}

	// APPENDLIMIT: max message size for APPEND (RFC 7889).
	// 0 means "0 bytes allowed" which makes clients think APPEND is forbidden!
	// We set it to our actual limit (10 MB, same as SMTP MaxMessageBytes).
	status.AppendLimit = 10 * 1024 * 1024

	log.Printf("Mailbox %s status: %d messages, %d unseen, %d recent, uidnext=%d, uidvalidity=%d, permanentflags=%v",
		m.name, status.Messages, status.Unseen, status.Recent, status.UidNext, status.UidValidity, status.PermanentFlags)

	return status, nil
}

// SetSubscribed sets the mailbox subscription status
func (m *Mailbox) SetSubscribed(subscribed bool) error {
	log.Printf("SetSubscribed %s=%v for user %s", m.name, subscribed, m.user.username)
	if subscribed {
		return m.database.SubscribeFolder(m.user.userID, m.folderID)
	}
	return m.database.UnsubscribeFolder(m.user.userID, m.folderID)
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

	log.Printf("Listing messages for mailbox %s (uid: %v, seqset: %v, items: %v)", m.name, uid, seqSet, items)

	// Load lightweight metadata for the whole folder (no body/body_html). We need
	// the full ordered list to assign sequence numbers and evaluate seqSet, but
	// loading every body here is what makes per-message FETCH O(N²).
	metas, err := m.database.GetMessagesByFolderMeta(m.folderID, 10000, 0)
	if err != nil {
		log.Printf("Failed to get messages: %v", err)
		return err
	}

	// Does this FETCH actually need the message body? Only body sections and
	// RFC822/BODYSTRUCTURE variants do; FLAGS/UID/ENVELOPE/SIZE/INTERNALDATE
	// are served entirely from metadata.
	needsBody := false
	wantsSize := false
	for _, it := range items {
		switch it {
		case imap.FetchUid, imap.FetchFlags, imap.FetchInternalDate, imap.FetchEnvelope:
			// metadata-only
		case imap.FetchRFC822Size:
			// Metadata-only when the stored size is already known; messages with
			// size=0 need the body loaded so the exact assembled size can be
			// computed (and persisted) — see convertToIMAPMessage.
			wantsSize = true
		default:
			needsBody = true
		}
	}

	// Select the messages the seqSet actually requests.
	type selected struct {
		seqNum int
		msg    *models.Message
		full   bool // body/attachments loaded, not just metadata
	}
	var picks []selected
	for seqNum, msg := range metas {
		id := uint32(seqNum + 1)
		if uid {
			id = msg.UID
		}
		if !seqSet.Contains(id) {
			continue
		}
		picks = append(picks, selected{seqNum: seqNum, msg: msg})
	}

	// IDs that need the full row: everything when a body item was requested,
	// otherwise just the size=0 messages when RFC822.SIZE was requested.
	var pickedIDs []int64
	for _, p := range picks {
		if needsBody || (wantsSize && p.msg.Size == 0) {
			pickedIDs = append(pickedIDs, p.msg.ID)
		}
	}

	log.Printf("Found %d messages in mailbox %s (folder %d), %d selected (needsBody=%v, fullLoads=%d)",
		len(metas), m.name, m.folderID, len(picks), needsBody, len(pickedIDs))

	// Load full bodies only for the selected subset, only when needed.
	if len(pickedIDs) > 0 {
		full, ferr := m.database.GetMessagesByIDs(pickedIDs)
		if ferr != nil {
			log.Printf("Failed to load message bodies: %v", ferr)
		} else {
			byID := make(map[int64]*models.Message, len(full))
			for _, fm := range full {
				byID[fm.ID] = fm
			}
			for i := range picks {
				if fm, ok := byID[picks[i].msg.ID]; ok {
					picks[i].msg = fm
					picks[i].full = true
				}
			}
		}
	}

	for _, p := range picks {
		ch <- m.convertToIMAPMessage(p.msg, uint32(p.seqNum+1), items, p.full)
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

	if textQuery != "" {
		// Text search REQUIRES the search indexer. If it isn't available or fails,
		// return zero hits — falling back to "all messages in folder" produces
		// massively confusing results for the user.
		if m.searchIndexer == nil {
			log.Printf("Text search requested but search indexer unavailable; returning empty")
			return nil, nil
		}
		log.Printf("Using Meilisearch for text query: %s", textQuery)
		searchResult, searchErr := m.searchIndexer.SearchInFolder(m.user.userID, m.folderID, textQuery, 10000, 0)
		if searchErr != nil || searchResult == nil {
			log.Printf("Meilisearch query failed: %v", searchErr)
			return nil, nil
		}
		ids := make([]int64, 0, len(searchResult.Hits))
		for _, hit := range searchResult.Hits {
			ids = append(ids, hit.ID)
		}
		if len(ids) == 0 {
			return nil, nil
		}
		messages, err = m.database.GetMessagesByIDs(ids)
	} else {
		// No text query — pull the whole folder so non-text criteria can filter it.
		messages, err = m.database.GetMessagesByFolderMeta(m.folderID, 10000, 0)
	}

	if err != nil {
		return nil, err
	}

	// Apply non-text criteria filters
	var results []uint32
	if uid {
		for _, msg := range messages {
			if m.matchesCriteria(msg, criteria) {
				results = append(results, msg.UID)
			}
		}
	} else {
		// IMAP SEARCH returns sequence numbers relative to the SELECTed mailbox,
		// NOT the index inside the matched-result subset. Build UID→seqno from
		// the full mailbox once, then look up each match.
		uidToSeq, mapErr := m.uidSeqMap()
		if mapErr != nil {
			log.Printf("uidSeqMap failed (%v); falling back to UIDs in SEARCH response", mapErr)
		}
		for _, msg := range messages {
			if !m.matchesCriteria(msg, criteria) {
				continue
			}
			if uidToSeq == nil {
				results = append(results, msg.UID)
				continue
			}
			if seq, ok := uidToSeq[msg.UID]; ok {
				results = append(results, seq)
			}
		}
	}

	log.Printf("Search found %d messages", len(results))
	return results, nil
}

// uidSeqMap returns a UID → 1-based sequence-number map for the entire mailbox,
// ordered by UID ascending (which is the standard IMAP sequence ordering).
func (m *Mailbox) uidSeqMap() (map[uint32]uint32, error) {
	all, err := m.database.GetMessagesByFolderMeta(m.folderID, 1000000, 0)
	if err != nil {
		return nil, err
	}
	out := make(map[uint32]uint32, len(all))
	for i, msg := range all {
		out[msg.UID] = uint32(i + 1)
	}
	return out, nil
}

// extractTextQuery extracts text search terms from criteria.
// Only TEXT, BODY and SUBJECT contribute — the search index is built over
// subject+body, so FROM/TO/CC criteria are ignored here.
//
// Recurses into the Or list: clients that send `OR SUBJECT "x" BODY "x"` parse into
// criteria.Or = [[<SUBJECT>, <BODY>]] rather than flat Header+Body, so the surface-
// level fields are empty and a non-recursive walk would think the query is empty —
// which makes SearchMessages fall through to the "no text query" branch and dump
// the entire folder.
func (m *Mailbox) extractTextQuery(criteria *imap.SearchCriteria) string {
	if criteria == nil {
		return ""
	}
	parts := collectTextParts(criteria)
	return strings.Join(parts, " ")
}

func collectTextParts(criteria *imap.SearchCriteria) []string {
	if criteria == nil {
		return nil
	}
	var parts []string
	for _, text := range criteria.Text {
		if text != "" {
			parts = append(parts, text)
		}
	}
	for _, body := range criteria.Body {
		if body != "" {
			parts = append(parts, body)
		}
	}
	for key, values := range criteria.Header {
		if !strings.EqualFold(key, "SUBJECT") {
			continue
		}
		for _, v := range values {
			if v != "" {
				parts = append(parts, v)
			}
		}
	}
	for _, pair := range criteria.Or {
		parts = append(parts, collectTextParts(pair[0])...)
		parts = append(parts, collectTextParts(pair[1])...)
	}
	// Note: criteria.Not is intentionally skipped — those terms must NOT appear,
	// so they shouldn't be sent to the full-text index as "find these".
	return parts
}

// CreateMessage creates a new message (APPEND command)
func (m *Mailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	log.Printf("CreateMessage called for mailbox %s (type=%s) with %d flags", m.name, m.folderType, len(flags))

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

	// Dedup for Sent folder: if message with same Message-ID already exists, skip
	if m.folderType == "sent" && parsed.GetMessageID() != "" {
		exists, err := m.database.MessageExistsInFolder(m.folderID, parsed.GetMessageID())
		if err == nil && exists {
			log.Printf("CreateMessage: dedup — message %s already in Sent, skipping", parsed.GetMessageID())
			return nil // Return OK to client
		}
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
	msgDateMs := timeutil.ToMs(msgDate)

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
		Date:      msgDateMs,
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
	messages, err := m.database.GetMessagesByFolderMeta(m.folderID, 10000, 0)
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

			// Push untagged FETCH (FLAGS) so other sessions — and the
			// non-silent originator — see the change (go-imap suppresses
			// its own FETCH responses when a backend Updates channel exists).
			if m.backend != nil {
				m.backend.notifyFlags(m.user.username, m.name, uint32(seqNum+1), msg.UID,
					flagList(seen, flagged, answered, deleted))
			}

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

// getUIDValidity returns the UIDVALIDITY for this mailbox (always >= 1).
func (m *Mailbox) getUIDValidity() uint32 {
	folder, err := m.database.GetFolderByID(m.folderID)
	if err != nil || folder.UIDValidity == 0 {
		return 1
	}
	return folder.UIDValidity
}

// CreateMessageUID is like CreateMessage but returns (uid, uidValidity) for UIDPLUS.
func (m *Mailbox) CreateMessageUID(flags []string, date time.Time, body imap.Literal) (uint32, uint32, error) {
	log.Printf("CreateMessageUID called for mailbox %s (type=%s) with %d flags", m.name, m.folderType, len(flags))

	data, err := io.ReadAll(body)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to read message body: %w", err)
	}

	p := parser.New()
	parsed, err := p.ParseBytes(data)
	if err != nil {
		parsed = &parser.ParsedMessage{}
	}

	// Dedup for Sent folder
	if m.folderType == "sent" && parsed.GetMessageID() != "" {
		exists, err := m.database.MessageExistsInFolder(m.folderID, parsed.GetMessageID())
		if err == nil && exists {
			log.Printf("CreateMessageUID: dedup — message %s already in Sent, skipping", parsed.GetMessageID())
			// Return existing UID for deduped message
			existingUID, _ := m.database.GetMessageUIDByMessageID(m.folderID, parsed.GetMessageID())
			return existingUID, m.getUIDValidity(), nil
		}
	}

	nextUID, err := m.database.GetNextUIDForFolder(m.folderID)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get next UID: %w", err)
	}

	seen, flagged, answered, draft, deleted := false, false, false, false, false
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

	msgDate2 := date
	if msgDate2.IsZero() {
		msgDate2 = parsed.GetDate()
	}
	if msgDate2.IsZero() {
		msgDate2 = time.Now()
	}
	msgDateMs2 := timeutil.ToMs(msgDate2)

	msg := &models.Message{
		AccountID: 0,
		UserID:    m.user.userID,
		FolderID:  m.folderID,
		MessageID: parsed.GetMessageID(),
		Subject:   parser.SanitizeUTF8(parsed.Subject),
		From:      parser.SanitizeUTF8(parser.FormatAddress(parsed.From)),
		To:        parser.SanitizeUTF8(parser.FormatAddressList(parsed.To)),
		Cc:        parser.SanitizeUTF8(parser.FormatAddressList(parsed.Cc)),
		ReplyTo:   parser.SanitizeUTF8(parser.FormatAddress(parsed.ReplyTo)),
		Date:      msgDateMs2,
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
		return 0, 0, fmt.Errorf("failed to save message: %w", err)
	}

	log.Printf("CreateMessageUID: saved message %d with UID %d to mailbox %s", msg.ID, msg.UID, m.name)
	return nextUID, m.getUIDValidity(), nil
}

// CopyMessagesUID is like CopyMessages but returns UID mapping for UIDPLUS.
func (m *Mailbox) CopyMessagesUID(uid bool, seqSet *imap.SeqSet, destName string) (uidValidity uint32, srcUIDs, destUIDs []uint32, err error) {
	destFolder, err := m.database.GetOrCreateFolderByNameAndUser(m.user.userID, destName, inferFolderType(destName))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("failed to get destination folder: %w", err)
	}

	messages, err := m.database.GetMessagesByFolderMeta(m.folderID, 10000, 0)
	if err != nil {
		return 0, nil, nil, err
	}

	for seqNum, msg := range messages {
		id := uint32(seqNum + 1)
		if uid {
			id = msg.UID
		}
		if !seqSet.Contains(id) {
			continue
		}

		newUID, copyErr := m.database.CopyMessageToFolder(msg.ID, destFolder.ID)
		if copyErr != nil {
			log.Printf("CopyMessagesUID: failed to copy message %d: %v", msg.ID, copyErr)
			continue
		}
		srcUIDs = append(srcUIDs, msg.UID)
		destUIDs = append(destUIDs, newUID)
	}

	// Get destination folder UIDVALIDITY
	df, _ := m.database.GetFolderByID(destFolder.ID)
	uidValidity = 1
	if df != nil && df.UIDValidity > 0 {
		uidValidity = df.UIDValidity
	}

	log.Printf("CopyMessagesUID: copied %d messages to %s", len(srcUIDs), destName)
	return uidValidity, srcUIDs, destUIDs, nil
}

// MoveMessagesUID is like MoveMessages but returns UID mapping for UIDPLUS.
func (m *Mailbox) MoveMessagesUID(uid bool, seqSet *imap.SeqSet, destName string) (uidValidity uint32, srcUIDs, destUIDs []uint32, err error) {
	destFolder, err := m.database.GetOrCreateFolderByNameAndUser(m.user.userID, destName, inferFolderType(destName))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("failed to get destination folder: %w", err)
	}

	messages, err := m.database.GetMessagesByFolderMeta(m.folderID, 10000, 0)
	if err != nil {
		return 0, nil, nil, err
	}

	var movedMsgIDs []int64
	for seqNum, msg := range messages {
		id := uint32(seqNum + 1)
		if uid {
			id = msg.UID
		}
		if !seqSet.Contains(id) {
			continue
		}

		newUID, copyErr := m.database.CopyMessageToFolder(msg.ID, destFolder.ID)
		if copyErr != nil {
			log.Printf("MoveMessagesUID: failed to move message %d: %v", msg.ID, copyErr)
			continue
		}
		srcUIDs = append(srcUIDs, msg.UID)
		destUIDs = append(destUIDs, newUID)
		movedMsgIDs = append(movedMsgIDs, msg.ID)
	}

	// Map UIDs to client-facing seqnums while the source rows still exist,
	// then notify all sessions after the deletes (same as MoveMessages).
	expungeSeqNums := m.clientSeqNums(srcUIDs)
	defer m.notifyExpungeDesc(expungeSeqNums)

	for _, msgID := range movedMsgIDs {
		if delErr := m.database.DeleteMessage(msgID); delErr != nil {
			log.Printf("MoveMessagesUID: failed to delete original message %d: %v", msgID, delErr)
		}
	}

	df, _ := m.database.GetFolderByID(destFolder.ID)
	uidValidity = 1
	if df != nil && df.UIDValidity > 0 {
		uidValidity = df.UIDValidity
	}

	log.Printf("MoveMessagesUID: moved %d messages to %s", len(srcUIDs), destName)
	return uidValidity, srcUIDs, destUIDs, nil
}

// CopyMessages copies messages to another mailbox
func (m *Mailbox) CopyMessages(uid bool, seqSet *imap.SeqSet, destName string) error {
	log.Printf("CopyMessages called: uid=%v, seqSet=%v, destName=%s", uid, seqSet, destName)

	// Get or create destination folder
	destFolder, err := m.database.GetOrCreateFolderByNameAndUser(m.user.userID, destName, inferFolderType(destName))
	if err != nil {
		log.Printf("CopyMessages: failed to get/create destination folder %s: %v", destName, err)
		return fmt.Errorf("failed to get destination folder: %w", err)
	}

	// Get messages from source folder
	messages, err := m.database.GetMessagesByFolderMeta(m.folderID, 10000, 0)
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
	destFolder, err := m.database.GetOrCreateFolderByNameAndUser(m.user.userID, destName, inferFolderType(destName))
	if err != nil {
		log.Printf("MoveMessages: failed to get/create destination folder %s: %v", destName, err)
		return fmt.Errorf("failed to get destination folder: %w", err)
	}

	// Get messages from source folder
	messages, err := m.database.GetMessagesByFolderMeta(m.folderID, 10000, 0)
	if err != nil {
		log.Printf("MoveMessages: failed to get messages from source folder: %v", err)
		return err
	}

	// Move matching messages
	movedCount := 0
	var movedMsgIDs []int64
	var movedUIDs []uint32
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
		movedUIDs = append(movedUIDs, msg.UID)
		movedCount++
		log.Printf("MoveMessages: moved message %d (UID %d) to folder %s with new UID %d",
			msg.ID, msg.UID, destName, newUID)
	}

	// Delete original messages from source folder
	if len(movedMsgIDs) > 0 {
		// Map UIDs to client-facing seqnums while the rows still exist.
		expungeSeqNums := m.clientSeqNums(movedUIDs)
		for _, msgID := range movedMsgIDs {
			if err := m.database.DeleteMessage(msgID); err != nil {
				log.Printf("MoveMessages: failed to delete original message %d: %v", msgID, err)
			}
		}
		// Untagged EXPUNGE for the source mailbox — without it, other live
		// sessions (and the originator: go-imap generates nothing for MOVE
		// when a backend Updates channel exists) keep showing moved messages.
		m.notifyExpungeDesc(expungeSeqNums)
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

	isTrash := m.folderType == "trash" || folder.Type == "trash"

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

	// Compute the seqnums clients will expunge BEFORE deleting (the mapping
	// is gone once the rows are). On mapping failure we still expunge and
	// just skip the untagged updates.
	expungeSeqNums := m.clientSeqNums(deletedUIDs)

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

	// Untagged EXPUNGE to every session on this mailbox, originator included
	// (go-imap only auto-generates expunge responses when no backend Updates
	// channel exists — with one, it is the backend's job).
	m.notifyExpungeDesc(expungeSeqNums)

	return nil
}

// Helper function to convert database message to IMAP message
// fullyLoaded reports whether msg carries its body/attachments (vs. a
// metadata-only row) — assembling the RFC822 from a metadata-only row would
// poison the body cache with an empty rendition.
func (m *Mailbox) convertToIMAPMessage(msg *models.Message, seqNum uint32, items []imap.FetchItem, fullyLoaded bool) *imap.Message {
	imapMsg := imap.NewMessage(seqNum, items)

	for _, item := range items {
		switch item {
		case imap.FetchEnvelope:
			imapMsg.Envelope = &imap.Envelope{
				Date:      timeutil.FromMs(msg.Date),
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

			// Fetch all attachments
			allAtts, attErr := m.database.GetAttachmentsByMessageID(msg.ID)
			if attErr != nil {
				allAtts = nil
			}
			var inlineAtts, regularAtts []*models.Attachment
			for _, att := range allAtts {
				if att.IsInline && att.ContentID != "" {
					inlineAtts = append(inlineAtts, att)
				} else {
					regularAtts = append(regularAtts, att)
				}
			}

			// Build the text body structure
			var textStructure *imap.BodyStructure

			if hasPlain && hasHTML {
				altStructure := &imap.BodyStructure{
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

				if len(inlineAtts) > 0 {
					relatedParts := []*imap.BodyStructure{altStructure}
					for _, att := range inlineAtts {
						mimeType, mimeSubType := splitMIME(att.ContentType)
						relatedParts = append(relatedParts, &imap.BodyStructure{
							MIMEType:          mimeType,
							MIMESubType:       mimeSubType,
							Size:              uint32(att.Size),
							Disposition:       "inline",
							DispositionParams: map[string]string{"filename": att.Filename},
							Id:                att.ContentID,
						})
					}
					textStructure = &imap.BodyStructure{
						MIMEType:    "multipart",
						MIMESubType: "related",
						Params:      map[string]string{"boundary": fmt.Sprintf("----=_Related_%d", msg.ID)},
						Parts:       relatedParts,
					}
				} else {
					textStructure = altStructure
				}
			} else if hasHTML {
				textStructure = &imap.BodyStructure{
					MIMEType:    "text",
					MIMESubType: "html",
					Params:      map[string]string{"charset": "utf-8"},
					Size:        uint32(len(msg.BodyHTML)),
				}
			} else {
				textStructure = &imap.BodyStructure{
					MIMEType:    "text",
					MIMESubType: "plain",
					Params:      map[string]string{"charset": "utf-8"},
					Size:        uint32(len(msg.Body)),
				}
			}

			if len(regularAtts) > 0 {
				// Wrap in multipart/mixed with file attachments
				mixedParts := []*imap.BodyStructure{textStructure}
				for _, att := range regularAtts {
					mimeType, mimeSubType := splitMIME(att.ContentType)
					// base64 size is ~4/3 of original
					b64Size := uint32((att.Size*4)/3 + att.Size/76 + 4)
					mixedParts = append(mixedParts, &imap.BodyStructure{
						MIMEType:          mimeType,
						MIMESubType:       mimeSubType,
						Params:            map[string]string{"name": att.Filename},
						Size:              b64Size,
						Encoding:          "base64",
						Disposition:       "attachment",
						DispositionParams: map[string]string{"filename": att.Filename},
					})
				}
				imapMsg.BodyStructure = &imap.BodyStructure{
					MIMEType:    "multipart",
					MIMESubType: "mixed",
					Params:      map[string]string{"boundary": fmt.Sprintf("----=_Mixed_%d", msg.ID)},
					Parts:       mixedParts,
				}
			} else {
				imapMsg.BodyStructure = textStructure
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
			imapMsg.InternalDate = timeutil.FromMs(msg.Date)

		case imap.FetchUid:
			imapMsg.Uid = msg.UID

		case imap.FetchRFC822Size:
			// Sync never populated size (historically 0). RFC822.SIZE must be the
			// exact length of the BODY[] literal we assemble — iOS Mail discards
			// bodies whose size doesn't match and retries for tens of minutes.
			// Compute it from the assembled message once and persist.
			if msg.Size == 0 && fullyLoaded {
				size := int64(len(m.entireMessageBytes(msg)))
				msg.Size = size
				if err := m.database.UpdateMessageSize(msg.ID, size); err != nil {
					log.Printf("Failed to persist size for message %d: %v", msg.ID, err)
				}
			}
			imapMsg.Size = uint32(msg.Size)

		case imap.FetchRFC822, imap.FetchRFC822Header, imap.FetchRFC822Text:
			// Handle RFC822 fetches
			section, _ := imap.ParseBodySectionName(item)
			if section != nil {
				imapMsg.Body[section] = applyPartial(section, m.buildMessageLiteral(msg, section))
			}

		default:
			// Handle BODY[] section requests
			section, err := imap.ParseBodySectionName(item)
			if err == nil && section != nil {
				imapMsg.Body[section] = applyPartial(section, m.buildMessageLiteral(msg, section))
			}
		}
	}

	return imapMsg
}

// applyPartial honors a BODY[]<from.length> partial fetch. go-imap emits the
// <from> origin in the response header but does NOT truncate the literal we
// supply — the backend must do it. Without this, a client that fetches a large
// message in chunks (iOS Mail uses <0.393216>) gets the entire message instead
// of the requested window, can't reconcile it, drops the connection and retries
// forever — looking like "server unavailable" on one big message.
func applyPartial(section *imap.BodySectionName, lit imap.Literal) imap.Literal {
	if section == nil || len(section.Partial) != 2 || lit == nil {
		return lit
	}
	data, err := io.ReadAll(lit)
	if err != nil {
		return lit
	}
	return bytes.NewReader(section.ExtractPartial(data))
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

// splitMIME splits "type/subtype" into parts, defaulting to application/octet-stream
func splitMIME(ct string) (string, string) {
	if parts := strings.SplitN(ct, "/", 2); len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "application", "octet-stream"
}

// writeMessageHeaders writes the synthetic RFC822 header block for a stored message.
func writeMessageHeaders(buf *bytes.Buffer, msg *models.Message) {
	buf.WriteString(fmt.Sprintf("From: %s\r\n", encodeAddressHeader(msg.From)))
	buf.WriteString(fmt.Sprintf("To: %s\r\n", encodeAddressHeader(msg.To)))
	if msg.Cc != "" {
		buf.WriteString(fmt.Sprintf("Cc: %s\r\n", encodeAddressHeader(msg.Cc)))
	}
	buf.WriteString(fmt.Sprintf("Subject: %s\r\n", encodeHeader(msg.Subject)))
	buf.WriteString(fmt.Sprintf("Date: %s\r\n", timeutil.FromMs(msg.Date).Format("Mon, 02 Jan 2006 15:04:05 -0700")))
	buf.WriteString(fmt.Sprintf("Message-ID: %s\r\n", msg.MessageID))
	buf.WriteString("MIME-Version: 1.0\r\n")
}

// buildMessageLiteral creates a literal for body section requests.
// Handles section paths like BODY[2] to return individual MIME parts.
func (m *Mailbox) buildMessageLiteral(msg *models.Message, section *imap.BodySectionName) imap.Literal {
	// Handle section path requests (e.g. BODY[2] for attachment)
	if len(section.Path) > 0 {
		return m.buildSectionLiteral(msg, section)
	}

	// Headers only — cheap, build directly.
	if section.Specifier == imap.HeaderSpecifier {
		var buf bytes.Buffer
		writeMessageHeaders(&buf, msg)
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		buf.WriteString("\r\n")
		return bytes.NewReader(buf.Bytes())
	}

	// Entire message — expensive to assemble (loads + base64-encodes every
	// attachment). Clients fetch large messages in many BODY[]<from.length>
	// windows, so memoize the assembled RFC822 and serve every window from cache.
	return bytes.NewReader(m.entireMessageBytes(msg))
}

// entireMessageBytes returns the full assembled RFC822 for a message, using the
// per-message body cache. A delivered message's content is immutable, so cached
// bytes never need invalidation — eviction is purely size-bounded.
func (m *Mailbox) entireMessageBytes(msg *models.Message) []byte {
	if data, ok := m.bodyCache.get(msg.ID); ok {
		return data
	}
	data := m.buildEntireMessageBytes(msg)
	m.bodyCache.put(msg.ID, data)
	return data
}

// buildEntireMessageBytes assembles the complete RFC822 representation
// (headers + body + attachments) for a stored message.
func (m *Mailbox) buildEntireMessageBytes(msg *models.Message) []byte {
	var buf bytes.Buffer
	writeMessageHeaders(&buf, msg)

	{
		hasPlain := msg.Body != ""
		hasHTML := msg.BodyHTML != ""

		// Fetch all attachments for this message
		allAtts, attErr := m.database.GetAttachmentsByMessageID(msg.ID)
		if attErr != nil {
			allAtts = nil
		}

		// Separate inline and regular attachments
		var inlineAtts, regularAtts []*models.Attachment
		for _, att := range allAtts {
			if att.IsInline && att.ContentID != "" {
				inlineAtts = append(inlineAtts, att)
			} else {
				regularAtts = append(regularAtts, att)
			}
		}

		// Build the text/body part into a helper buffer
		var bodyBuf bytes.Buffer
		altBoundary := fmt.Sprintf("----=_Part_%d", msg.ID)

		if hasPlain && hasHTML {
			if len(inlineAtts) > 0 {
				// multipart/related wrapping alternative + inline images
				relatedBoundary := fmt.Sprintf("----=_Related_%d", msg.ID)
				bodyBuf.WriteString(fmt.Sprintf("Content-Type: multipart/related; boundary=\"%s\"\r\n", relatedBoundary))
				bodyBuf.WriteString("\r\n")

				bodyBuf.WriteString(fmt.Sprintf("--%s\r\n", relatedBoundary))
				bodyBuf.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", altBoundary))
				bodyBuf.WriteString("\r\n")

				bodyBuf.WriteString(fmt.Sprintf("--%s\r\n", altBoundary))
				bodyBuf.WriteString("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
				bodyBuf.WriteString(msg.Body)
				bodyBuf.WriteString("\r\n")

				bodyBuf.WriteString(fmt.Sprintf("--%s\r\n", altBoundary))
				bodyBuf.WriteString("Content-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
				bodyBuf.WriteString(msg.BodyHTML)
				bodyBuf.WriteString("\r\n")
				bodyBuf.WriteString(fmt.Sprintf("--%s--\r\n", altBoundary))

				for _, attMeta := range inlineAtts {
					// GetAttachmentsByMessageID returns metadata only — load the
					// data individually, same as regular attachments below. Writing
					// attMeta.Data here silently produced empty inline parts while
					// BODYSTRUCTURE still advertised the real size; iOS Mail
					// discards such messages and retries indefinitely.
					att, err := m.database.GetAttachmentByID(attMeta.ID)
					if err != nil {
						log.Printf("buildEntireMessageBytes: failed to load inline attachment %d: %v", attMeta.ID, err)
						continue
					}
					bodyBuf.WriteString(fmt.Sprintf("--%s\r\n", relatedBoundary))
					bodyBuf.WriteString(fmt.Sprintf("Content-Type: %s\r\n", att.ContentType))
					bodyBuf.WriteString("Content-Transfer-Encoding: base64\r\n")
					bodyBuf.WriteString(fmt.Sprintf("Content-ID: <%s>\r\n", att.ContentID))
					bodyBuf.WriteString(fmt.Sprintf("Content-Disposition: inline; filename=\"%s\"\r\n", encodeHeader(att.Filename)))
					bodyBuf.WriteString("\r\n")
					encoded := base64.StdEncoding.EncodeToString(att.Data)
					for i := 0; i < len(encoded); i += 76 {
						end := i + 76
						if end > len(encoded) {
							end = len(encoded)
						}
						bodyBuf.WriteString(encoded[i:end])
						bodyBuf.WriteString("\r\n")
					}
				}
				bodyBuf.WriteString(fmt.Sprintf("--%s--\r\n", relatedBoundary))
			} else {
				// multipart/alternative
				bodyBuf.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", altBoundary))
				bodyBuf.WriteString("\r\n")

				bodyBuf.WriteString(fmt.Sprintf("--%s\r\n", altBoundary))
				bodyBuf.WriteString("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
				bodyBuf.WriteString(msg.Body)
				bodyBuf.WriteString("\r\n")

				bodyBuf.WriteString(fmt.Sprintf("--%s\r\n", altBoundary))
				bodyBuf.WriteString("Content-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
				bodyBuf.WriteString(msg.BodyHTML)
				bodyBuf.WriteString("\r\n")
				bodyBuf.WriteString(fmt.Sprintf("--%s--\r\n", altBoundary))
			}
		} else if hasHTML {
			bodyBuf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
			bodyBuf.WriteString(msg.BodyHTML)
		} else {
			bodyBuf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
			bodyBuf.WriteString(msg.Body)
		}

		if len(regularAtts) > 0 {
			// Wrap everything in multipart/mixed to include file attachments
			mixedBoundary := fmt.Sprintf("----=_Mixed_%d", msg.ID)
			buf.WriteString(fmt.Sprintf("Content-Type: multipart/mixed; boundary=\"%s\"\r\n", mixedBoundary))
			buf.WriteString("\r\n")

			// First part: the body content
			buf.WriteString(fmt.Sprintf("--%s\r\n", mixedBoundary))
			buf.Write(bodyBuf.Bytes())
			buf.WriteString("\r\n")

			// Attachment parts — load data individually
			for _, attMeta := range regularAtts {
				fullAtt, err := m.database.GetAttachmentByID(attMeta.ID)
				if err != nil {
					continue
				}
				buf.WriteString(fmt.Sprintf("--%s\r\n", mixedBoundary))
				buf.WriteString(fmt.Sprintf("Content-Type: %s; name=\"%s\"\r\n", fullAtt.ContentType, encodeHeader(fullAtt.Filename)))
				buf.WriteString("Content-Transfer-Encoding: base64\r\n")
				buf.WriteString(fmt.Sprintf("Content-Disposition: attachment; filename=\"%s\"\r\n", encodeHeader(fullAtt.Filename)))
				buf.WriteString("\r\n")
				encoded := base64.StdEncoding.EncodeToString(fullAtt.Data)
				for i := 0; i < len(encoded); i += 76 {
					end := i + 76
					if end > len(encoded) {
						end = len(encoded)
					}
					buf.WriteString(encoded[i:end])
					buf.WriteString("\r\n")
				}
			}

			buf.WriteString(fmt.Sprintf("--%s--\r\n", mixedBoundary))
		} else {
			// No regular attachments — write body directly
			buf.Write(bodyBuf.Bytes())
		}
	}

	return buf.Bytes()
}

// buildSectionLiteral returns a specific MIME part for section path requests.
// For a multipart/mixed message with attachments:
//
//	BODY[1]   → text body part (alternative/related/plain/html)
//	BODY[1.1] → text/plain
//	BODY[1.2] → text/html
//	BODY[2]   → first file attachment
//	BODY[3]   → second file attachment, etc.
//
// For a message without regular attachments, BODY[1] → text/plain, BODY[2] → text/html
func (m *Mailbox) buildSectionLiteral(msg *models.Message, section *imap.BodySectionName) imap.Literal {
	path := section.Path
	hasPlain := msg.Body != ""
	hasHTML := msg.BodyHTML != ""

	// Fetch attachments to determine structure
	allAtts, attErr := m.database.GetAttachmentsByMessageID(msg.ID)
	if attErr != nil {
		log.Printf("buildSectionLiteral: failed to get attachments for msg %d: %v", msg.ID, attErr)
		allAtts = nil
	}
	var regularAtts []*models.Attachment
	for _, att := range allAtts {
		if !att.IsInline || att.ContentID == "" {
			regularAtts = append(regularAtts, att)
		}
	}

	hasRegularAtts := len(regularAtts) > 0
	log.Printf("buildSectionLiteral: msg=%d path=%v hasPlain=%v hasHTML=%v allAtts=%d regularAtts=%d",
		msg.ID, path, hasPlain, hasHTML, len(allAtts), len(regularAtts))

	// Structure when we have regular attachments:
	//   multipart/mixed
	//     [1] → text part (alternative or single)
	//     [2] → first attachment
	//     [3] → second attachment ...
	//
	// Structure without regular attachments (plain+html):
	//   multipart/alternative
	//     [1] → text/plain
	//     [2] → text/html

	if hasRegularAtts {
		partNum := path[0]
		if partNum == 1 {
			// Text body part
			if len(path) == 1 {
				// Return the whole text part
				var buf bytes.Buffer
				if hasPlain && hasHTML {
					altBoundary := fmt.Sprintf("----=_Part_%d", msg.ID)
					buf.WriteString(fmt.Sprintf("--%s\r\n", altBoundary))
					buf.WriteString("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
					buf.WriteString(msg.Body)
					buf.WriteString("\r\n")
					buf.WriteString(fmt.Sprintf("--%s\r\n", altBoundary))
					buf.WriteString("Content-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
					buf.WriteString(msg.BodyHTML)
					buf.WriteString("\r\n")
					buf.WriteString(fmt.Sprintf("--%s--\r\n", altBoundary))
				} else if hasHTML {
					buf.WriteString(msg.BodyHTML)
				} else {
					buf.WriteString(msg.Body)
				}
				return bytes.NewReader(buf.Bytes())
			}
			// Sub-part of text, e.g. BODY[1.1] or BODY[1.2]
			subPart := path[1]
			if subPart == 1 && hasPlain {
				return strings.NewReader(msg.Body)
			} else if subPart == 2 && hasHTML {
				return strings.NewReader(msg.BodyHTML)
			}
		} else if partNum >= 2 && partNum-2 < len(regularAtts) {
			// File attachment — need to load data from DB
			attMeta := regularAtts[partNum-2]
			att, err := m.database.GetAttachmentByID(attMeta.ID)
			if err != nil || len(att.Data) == 0 {
				log.Printf("buildSectionLiteral: failed to load attachment %d data: %v", attMeta.ID, err)
				return strings.NewReader("")
			}
			log.Printf("buildSectionLiteral: returning attachment %s (%d bytes)", att.Filename, len(att.Data))
			encoded := base64.StdEncoding.EncodeToString(att.Data)
			// Format in 76-char lines
			var buf bytes.Buffer
			for i := 0; i < len(encoded); i += 76 {
				end := i + 76
				if end > len(encoded) {
					end = len(encoded)
				}
				buf.WriteString(encoded[i:end])
				buf.WriteString("\r\n")
			}
			return bytes.NewReader(buf.Bytes())
		}
	} else {
		// No regular attachments: multipart/alternative [1]=plain [2]=html
		// or single part
		partNum := path[0]
		if hasPlain && hasHTML {
			if partNum == 1 {
				return strings.NewReader(msg.Body)
			} else if partNum == 2 {
				return strings.NewReader(msg.BodyHTML)
			}
		}
	}

	// Fallback: return empty
	return strings.NewReader("")
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

	var result []*imap.Address
	for _, part := range strings.Split(addrStr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		result = append(result, parseSingleAddress(part))
	}
	return result
}

func parseSingleAddress(raw string) *imap.Address {
	raw = strings.TrimSpace(raw)

	// "Name <user@host>" or "<user@host>"
	if lt := strings.LastIndex(raw, "<"); lt >= 0 {
		if gt := strings.Index(raw[lt:], ">"); gt >= 0 {
			email := raw[lt+1 : lt+gt]
			name := strings.TrimSpace(raw[:lt])
			// Strip surrounding quotes from name
			name = strings.Trim(name, "\"'")
			mailbox, host := splitEmail(email)
			return &imap.Address{
				PersonalName: name,
				MailboxName:  mailbox,
				HostName:     host,
			}
		}
	}

	// Bare email: user@host
	if strings.Contains(raw, "@") {
		mailbox, host := splitEmail(raw)
		return &imap.Address{
			PersonalName: "",
			MailboxName:  mailbox,
			HostName:     host,
		}
	}

	// Fallback — unparseable
	return &imap.Address{
		PersonalName: "",
		MailboxName:  raw,
		HostName:     "",
	}
}

func splitEmail(email string) (mailbox, host string) {
	email = strings.TrimSpace(email)
	if at := strings.LastIndex(email, "@"); at >= 0 {
		return email[:at], email[at+1:]
	}
	return email, ""
}
