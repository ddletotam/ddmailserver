package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"mime"
	"sort"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/notify"
	"github.com/ddletotam/ddmailserver/internal/parser"
	"github.com/ddletotam/ddmailserver/internal/search"
	msgsvc "github.com/ddletotam/ddmailserver/internal/service/messages"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
	"github.com/emersion/go-imap"
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

// clientSeqMap maps the given UIDs to their 1-based positions in the
// client-facing sequence of this folder: the UID-ordered union of visible and
// \Deleted-flagged (not yet expunged) messages. The visible list alone is
// wrong here — clients still count \Deleted-flagged rows until they receive
// EXPUNGE. UIDs not in the folder are absent; nil on a read error.
func (m *Mailbox) clientSeqMap(uids []uint32) map[uint32]uint32 {
	want := make(map[uint32]bool, len(uids))
	for _, u := range uids {
		want[u] = true
	}

	visible, err := m.database.GetFolderMessageRefs(m.folderID)
	if err != nil {
		log.Printf("clientSeqMap: failed to load visible messages: %v", err)
		return nil
	}
	flagged, err := m.database.GetDeletedFolderUIDs(m.folderID)
	if err != nil {
		log.Printf("clientSeqMap: failed to load deleted-flagged messages: %v", err)
		return nil
	}

	// Merge the two UID-ascending lists, recording positions of wanted UIDs.
	seqOf := make(map[uint32]uint32, len(uids))
	i, j := 0, 0
	var pos uint32
	for i < len(visible) || j < len(flagged) {
		pos++
		var uid uint32
		if j >= len(flagged) || (i < len(visible) && visible[i].UID < flagged[j]) {
			uid = visible[i].UID
			i++
		} else {
			uid = flagged[j]
			j++
		}
		if want[uid] {
			seqOf[uid] = pos
		}
	}
	return seqOf
}

// clientSeqNums is clientSeqMap as an ascending list of positions.
func (m *Mailbox) clientSeqNums(uids []uint32) []uint32 {
	seqOf := m.clientSeqMap(uids)
	seqNums := make([]uint32, 0, len(seqOf))
	for _, seq := range seqOf {
		seqNums = append(seqNums, seq)
	}
	sort.Slice(seqNums, func(i, j int) bool { return seqNums[i] < seqNums[j] })
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

	// The whole folder's (id, uid, flags) gives sequence numbers and resolves
	// the set; metadata and bodies are then loaded only for what it selects,
	// a batch at a time — a folder may hold far more than fits comfortably in
	// one query or in memory.
	picks, total, err := m.selectMessages(uid, seqSet)
	if err != nil {
		log.Printf("Failed to get messages: %v", err)
		return err
	}

	// What does this FETCH need beyond the index? FLAGS/UID come from it
	// directly; ENVELOPE/INTERNALDATE/SIZE need the metadata row; body
	// sections and RFC822/BODYSTRUCTURE variants need the full message.
	needsMeta := false
	needsBody := false
	wantsSize := false
	for _, it := range items {
		switch it {
		case imap.FetchUid, imap.FetchFlags:
			// served from the index
		case imap.FetchInternalDate, imap.FetchEnvelope:
			needsMeta = true
		case imap.FetchRFC822Size:
			// Metadata-only when the stored size is already known; messages with
			// size=0 need the body loaded so the exact assembled size can be
			// computed (and persisted) — see convertToIMAPMessage.
			needsMeta = true
			wantsSize = true
		default:
			needsBody = true
		}
	}

	log.Printf("Found %d messages in mailbox %s (folder %d), %d selected (needsMeta=%v, needsBody=%v)",
		total, m.name, m.folderID, len(picks), needsMeta, needsBody)

	if !needsMeta && !needsBody {
		for _, p := range picks {
			ch <- m.convertToIMAPMessage(refMessage(p.ref), p.seqNum, items, false)
		}
		return nil
	}

	batch := fetchMetaBatch
	if needsBody {
		batch = fetchBodyBatch
	}
	for from := 0; from < len(picks); from += batch {
		to := from + batch
		if to > len(picks) {
			to = len(picks)
		}
		chunk := picks[from:to]
		ids := make([]int64, len(chunk))
		for i, p := range chunk {
			ids[i] = p.ref.ID
		}

		// full: body/attachments loaded, not just metadata.
		byID := make(map[int64]*models.Message, len(chunk))
		full := make(map[int64]bool)
		if needsBody {
			msgs, err := m.database.GetMessagesByIDs(ids)
			if err != nil {
				log.Printf("Failed to load message bodies: %v", err)
				return err
			}
			for _, msg := range msgs {
				byID[msg.ID] = msg
				full[msg.ID] = true
			}
		} else {
			msgs, err := m.database.GetMessagesMetaByIDs(ids)
			if err != nil {
				log.Printf("Failed to load message metadata: %v", err)
				return err
			}
			var sizeless []int64
			for _, msg := range msgs {
				byID[msg.ID] = msg
				if wantsSize && msg.Size == 0 {
					sizeless = append(sizeless, msg.ID)
				}
			}
			if len(sizeless) > 0 {
				// Not fatal: without the body the size stays 0 for now.
				msgs, err := m.database.GetMessagesByIDs(sizeless)
				if err != nil {
					log.Printf("Failed to load message bodies for RFC822.SIZE: %v", err)
				}
				for _, msg := range msgs {
					byID[msg.ID] = msg
					full[msg.ID] = true
				}
			}
		}

		for _, p := range chunk {
			msg, ok := byID[p.ref.ID]
			if !ok {
				continue // deleted since the index was read
			}
			ch <- m.convertToIMAPMessage(msg, p.seqNum, items, full[p.ref.ID])
		}
	}

	return nil
}

// FETCH loads selected messages this many at a time: metadata rows are
// small, full rows carry bodies.
const (
	fetchMetaBatch = 1000
	fetchBodyBatch = 100
)

// selectedMessage is a message an IMAP command addresses.
type selectedMessage struct {
	seqNum uint32
	ref    db.FolderMessageRef
}

// selectMessages reads the folder's index and returns, in sequence order, the
// messages seqSet addresses (UIDs when uid is set) plus the folder's size.
func (m *Mailbox) selectMessages(uid bool, seqSet *imap.SeqSet) ([]selectedMessage, int, error) {
	refs, err := m.database.GetFolderMessageRefs(m.folderID)
	if err != nil {
		return nil, 0, err
	}
	return pickMessages(refs, uid, seqSet), len(refs), nil
}

// pickMessages selects from refs (ascending UID, so ascending sequence
// number too) the messages the set addresses, in one pass over both.
func pickMessages(refs []db.FolderMessageRef, uid bool, seqSet *imap.SeqSet) []selectedMessage {
	ranges := append([]imap.Seq(nil), resolveSeqSet(seqSet, uid, refs).Set...)
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].Start < ranges[j].Start })

	var picks []selectedMessage
	j := 0
	for i, r := range refs {
		id := uint32(i + 1)
		if uid {
			id = r.UID
		}
		// ids only grow, so a range ending below id is done for good. If any
		// range covers id, the first remaining one does: it starts no later.
		for j < len(ranges) && ranges[j].Stop < id {
			j++
		}
		if j == len(ranges) {
			break
		}
		if ranges[j].Start <= id {
			picks = append(picks, selectedMessage{seqNum: uint32(i + 1), ref: r})
		}
	}
	return picks
}

// refMessage is a message carrying only what the index has: enough for FLAGS,
// UID and flag-only SEARCH.
func refMessage(r db.FolderMessageRef) *models.Message {
	return &models.Message{
		ID:       r.ID,
		UID:      r.UID,
		Seen:     r.Seen,
		Flagged:  r.Flagged,
		Answered: r.Answered,
		Draft:    r.Draft,
	}
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
		messages, err = m.database.GetMessagesMetaByIDs(ids)
	} else {
		// No text query — the non-text criteria matchesCriteria knows are
		// flags, so the folder's index is enough, however large the folder.
		var refs []db.FolderMessageRef
		refs, err = m.database.GetFolderMessageRefs(m.folderID)
		for _, r := range refs {
			messages = append(messages, refMessage(r))
		}
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
	all, err := m.database.GetFolderMessageRefs(m.folderID)
	if err != nil {
		return nil, err
	}
	out := make(map[uint32]uint32, len(all))
	for i, r := range all {
		out[r.UID] = uint32(i + 1)
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

	data, err := io.ReadAll(body)
	if err != nil {
		log.Printf("CreateMessage: failed to read body: %v", err)
		return fmt.Errorf("failed to read message body: %w", err)
	}
	_, _, err = m.storeAppended(flags, date, data)
	return err
}

// storeAppended persists a message handed to us by APPEND — a client saving
// its own copy, most often into Sent — and reports the UID it ended up with
// and whether it was a duplicate we skipped. Shared by CreateMessage and
// CreateMessageUID, which differ only in what they hand back to the client.
//
// Everything the parser found has to be stored here, not just the headers and
// the body. An APPEND is often the ONLY copy of a message we will ever see:
// a client that sends through someone else's SMTP still saves its Sent copy
// with us, and no submission path runs to save the attachments for it.
// Dropping the parts left such a letter with none at all — 76 KB on the wire,
// a 195-byte HTML div in the database — and dropping References cost the
// thread the links to what it answers.
func (m *Mailbox) storeAppended(flags []string, date time.Time, data []byte) (uint32, bool, error) {
	p := parser.New()
	parsed, err := p.ParseBytes(data)
	if err != nil {
		log.Printf("storeAppended: failed to parse message: %v", err)
		// Continue with minimal info even if parsing fails
		parsed = &parser.ParsedMessage{}
	}

	// Dedup for Sent folder: if message with same Message-ID already exists,
	// keep the copy we have (it came from our own submission, raw email and
	// all) and report its UID.
	if m.folderType == "sent" && parsed.GetMessageID() != "" {
		exists, err := m.database.MessageExistsInFolder(m.folderID, parsed.GetMessageID())
		if err == nil && exists {
			log.Printf("storeAppended: dedup — message %s already in Sent, skipping", parsed.GetMessageID())
			existingUID, _ := m.database.GetMessageUIDByMessageID(m.folderID, parsed.GetMessageID())
			return existingUID, true, nil
		}
	}

	nextUID, err := m.database.GetNextUIDForFolder(m.folderID)
	if err != nil {
		log.Printf("storeAppended: failed to get next UID: %v", err)
		return 0, false, fmt.Errorf("failed to get next UID: %w", err)
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

	// APPEND carries its own INTERNALDATE; fall back to the Date header, then
	// to now, so a message never lands with a zero date.
	msgDate := date
	if msgDate.IsZero() {
		msgDate = parsed.GetDate()
	}
	if msgDate.IsZero() {
		msgDate = time.Now()
	}

	msg := &models.Message{
		AccountID:         0, // Local message
		UserID:            m.user.userID,
		FolderID:          m.folderID,
		MessageID:         parsed.GetMessageID(),
		Subject:           parser.SanitizeUTF8(parsed.Subject),
		From:              parser.SanitizeUTF8(parser.FormatAddress(parsed.From)),
		To:                parser.SanitizeUTF8(parser.FormatAddressList(parsed.To)),
		Cc:                parser.SanitizeUTF8(parser.FormatAddressList(parsed.Cc)),
		ReplyTo:           parser.SanitizeUTF8(parser.FormatAddress(parsed.ReplyTo)),
		Date:              timeutil.ToMs(msgDate),
		DateTZ:            timeutil.TZOffsetMinutes(msgDate),
		Body:              parser.SanitizeUTF8(parsed.Body),
		BodyHTML:          parser.SanitizeUTF8(parsed.BodyHTML),
		RawEmail:          data, // so «показать исходник» shows the real RFC-822
		Attachments:       len(parsed.Attachments),
		Size:              int64(len(data)),
		UID:               nextUID,
		Seen:              seen,
		Flagged:           flagged,
		Answered:          answered,
		Draft:             draft,
		Deleted:           deleted,
		InReplyTo:         parser.SanitizeUTF8(parsed.InReplyTo),
		MessageReferences: parser.SanitizeUTF8(strings.Join(parsed.References, " ")),
	}

	if err := m.database.CreateMessage(msg); err != nil {
		log.Printf("storeAppended: failed to save message: %v", err)
		return 0, false, fmt.Errorf("failed to save message: %w", err)
	}

	// Attachments (inline images included — they carry a Content-ID the body
	// references, and without the rows an appended letter renders with broken
	// pictures as well as missing files).
	for _, att := range parsed.Attachments {
		attachment := &models.Attachment{
			MessageID:   msg.ID,
			ContentID:   strings.Trim(att.ContentID, "<>"),
			Filename:    att.Filename,
			ContentType: att.ContentType,
			Size:        int(att.Size),
			IsInline:    att.IsInline,
			Data:        att.Data,
		}
		if err := m.database.CreateAttachment(attachment); err != nil {
			log.Printf("storeAppended: failed to save attachment %s: %v", att.Filename, err)
		}
	}

	log.Printf("storeAppended: saved message %d with UID %d (%d attachments) to mailbox %s",
		msg.ID, msg.UID, len(parsed.Attachments), m.name)
	return nextUID, false, nil
}

// messageService returns the shared message service; a Mailbox built without
// a backend (tests) gets one over its own database handle.
func (m *Mailbox) messageService() *msgsvc.Service {
	if m.backend != nil && m.backend.messages != nil {
		return m.backend.messages
	}
	return msgsvc.NewWithDB(m.database)
}

// imapFlagUpdate translates a STORE into a flag update. FLAGS (SetFlags)
// replaces \Seen \Flagged \Answered \Deleted — unlisted ones are cleared —
// while +FLAGS/-FLAGS touch only the listed ones. \Draft and keywords are not
// stored through IMAP (never were); they are ignored here.
func imapFlagUpdate(operation imap.FlagsOp, flags []string) msgsvc.FlagUpdate {
	var update msgsvc.FlagUpdate
	if operation == imap.SetFlags {
		f := msgsvc.Bool(false)
		update = msgsvc.FlagUpdate{Seen: f, Flagged: f, Answered: f, Deleted: f}
	}
	value := operation != imap.RemoveFlags
	for _, flag := range flags {
		switch flag {
		case imap.SeenFlag, imap.FlaggedFlag, imap.AnsweredFlag, imap.DeletedFlag:
			if u, ok := msgsvc.FlagUpdateFor(flag, value); ok {
				update = update.Merge(u)
			}
		}
	}
	return update
}

// UpdateMessagesFlags updates message flags
func (m *Mailbox) UpdateMessagesFlags(uid bool, seqSet *imap.SeqSet, operation imap.FlagsOp, flags []string) error {
	log.Printf("UpdateMessagesFlags called: mailbox=%s, uid=%v, seqSet=%v, operation=%v, flags=%v",
		m.name, uid, seqSet, operation, flags)

	picks, _, err := m.selectMessages(uid, seqSet)
	if err != nil {
		return err
	}

	update := imapFlagUpdate(operation, flags)

	// Update matching messages
	updatedAny := false
	for _, p := range picks {
		msg := p.ref
		// One path for every flag change (IMAP, desktop API, web): the
		// service writes the flags and, for an external-account message,
		// queues the new state for the source server — in one transaction.
		res, err := m.messageService().SetFlags(context.Background(), m.user.userID, msg.ID, update)
		if err != nil {
			log.Printf("Failed to update flags for message %d: %v", msg.ID, err)
			continue
		}
		updatedAny = true
		after := res.After
		log.Printf("Updated flags for message %d: seen=%v, flagged=%v, answered=%v, deleted=%v",
			msg.ID, after.Seen, after.Flagged, after.Answered, after.Deleted)

		// Push untagged FETCH (FLAGS) so other sessions — and the
		// non-silent originator — see the change (go-imap suppresses
		// its own FETCH responses when a backend Updates channel exists).
		if m.backend != nil {
			m.backend.notifyFlags(m.user.username, m.name, p.seqNum, msg.UID,
				flagList(after.Seen, after.Flagged, after.Answered, after.Deleted))
		}
	}

	// One WS push per STORE so connected desktop clients refresh unread
	// state (read/starred in Thunderbird/iOS). IMAP sessions are already
	// covered by the untagged FETCH above; this is the WebSocket leg.
	if updatedAny && m.backend != nil && m.backend.hub != nil {
		m.backend.hub.Publish(notify.Event{
			UserID:   m.user.userID,
			Type:     notify.EventFlagsChanged,
			Username: m.user.username,
			Mailbox:  m.name,
		})
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
	uid, _, err := m.storeAppended(flags, date, data)
	if err != nil {
		return 0, 0, err
	}
	return uid, m.getUIDValidity(), nil
}

// transferResult is what COPY/MOVE did: the UIDPLUS mapping (COPYUID) and the
// source sequence numbers that left the folder (for EXPUNGE).
type transferResult struct {
	uidValidity uint32   // of the destination
	srcUIDs     []uint32 // paired with destUIDs
	destUIDs    []uint32
	expunged    []uint32 // ascending client-facing seqnums in this mailbox
}

// transfer runs COPY (move=false) or MOVE through the message service, all
// selected messages in one transaction. Any failure is returned — the client
// gets NO and nothing has changed; it used to be logged and answered OK.
// See messages.Service.Transfer for why COPY of a message that has a
// Message-ID moves it.
func (m *Mailbox) transfer(uid bool, seqSet *imap.SeqSet, destName string, move bool) (*transferResult, error) {
	op := "COPY"
	if move {
		op = "MOVE"
	}
	destFolder, err := m.database.GetOrCreateFolderByNameAndUser(m.user.userID, destName, inferFolderType(destName))
	if err != nil {
		return nil, fmt.Errorf("%s: destination folder %s: %w", op, destName, err)
	}
	res := &transferResult{uidValidity: destFolder.UIDValidity}
	if res.uidValidity == 0 {
		res.uidValidity = 1
	}

	picks, _, err := m.selectMessages(uid, seqSet)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if len(picks) == 0 {
		return res, nil
	}
	ids := make([]int64, len(picks))
	uids := make([]uint32, len(picks))
	for i, p := range picks {
		ids[i] = p.ref.ID
		uids[i] = p.ref.UID
	}
	// Positions as clients count them, read while the rows are still here.
	seqOf := m.clientSeqMap(uids)

	moved, err := m.messageService().Transfer(context.Background(), m.user.userID, ids, destFolder.ID, move)
	if err != nil {
		log.Printf("%s %s -> %s failed: %v", op, m.name, destName, err)
		return nil, fmt.Errorf("%s failed: %w", op, err)
	}
	for _, t := range moved {
		res.srcUIDs = append(res.srcUIDs, t.SrcUID)
		res.destUIDs = append(res.destUIDs, t.DestUID)
		if t.Removed {
			if seq, ok := seqOf[t.SrcUID]; ok {
				res.expunged = append(res.expunged, seq)
			}
		}
	}
	sort.Slice(res.expunged, func(i, j int) bool { return res.expunged[i] < res.expunged[j] })
	log.Printf("%s: %d messages %s -> %s, %d left the source", op, len(moved), m.name, destName, len(res.expunged))
	return res, nil
}

// CopyMessagesUID is COPY returning the UID mapping for UIDPLUS (COPYUID).
// Messages that left this mailbox (see transfer) are announced as EXPUNGE.
func (m *Mailbox) CopyMessagesUID(uid bool, seqSet *imap.SeqSet, destName string) (uidValidity uint32, srcUIDs, destUIDs []uint32, err error) {
	res, err := m.transfer(uid, seqSet, destName, false)
	if err != nil {
		return 0, nil, nil, err
	}
	m.notifyExpungeDesc(res.expunged)
	return res.uidValidity, res.srcUIDs, res.destUIDs, nil
}

// CopyMessages copies messages to another mailbox.
func (m *Mailbox) CopyMessages(uid bool, seqSet *imap.SeqSet, destName string) error {
	_, _, _, err := m.CopyMessagesUID(uid, seqSet, destName)
	return err
}

// MoveMessages moves messages to another mailbox (MOVE extension). The MOVE
// command itself is served by uidplusMoveHandler, which also sends COPYUID;
// this is the backend.MoveMailbox fallback.
func (m *Mailbox) MoveMessages(uid bool, seqSet *imap.SeqSet, destName string) error {
	res, err := m.transfer(uid, seqSet, destName, true)
	if err != nil {
		return err
	}
	// Untagged EXPUNGE for the source mailbox — without it, other live
	// sessions (and the originator: go-imap generates nothing for MOVE when a
	// backend Updates channel exists) keep showing moved messages.
	m.notifyExpungeDesc(res.expunged)
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

// resolveSeqSet turns the client's set into explicit ranges against the
// mailbox as loaded: "*" becomes the largest sequence number (or UID) present
// and a range no longer depends on the order of its ends ("5:2" == "2:5").
// RFC 3501 §6.4.8: "n:*" always includes the last message, even when n lies
// past it — that is how a client asks "anything new since UIDNEXT?".
// go-imap's Seq.Contains never matches a bare "*" and drops such ranges, so
// FETCH * and UID FETCH <uidnext>:* used to answer with nothing.
func resolveSeqSet(set *imap.SeqSet, uid bool, msgs []db.FolderMessageRef) *imap.SeqSet {
	out := new(imap.SeqSet)
	if set == nil || len(msgs) == 0 {
		return out
	}
	max := uint32(len(msgs))
	if uid {
		max = 0
		for _, m := range msgs {
			if m.UID > max {
				max = m.UID
			}
		}
	}
	for _, r := range set.Set {
		a, b := r.Start, r.Stop
		if a == 0 {
			a = max
		}
		if b == 0 {
			b = max
		}
		if a > b {
			a, b = b, a
		}
		out.AddRange(a, b)
	}
	return out
}
