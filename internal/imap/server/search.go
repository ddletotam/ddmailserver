package server

import (
	"bufio"
	"bytes"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/parser"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-message/textproto"
)

// IMAP SEARCH (RFC 3501 §6.4.4) over a folder of any size.
//
// The folder's index — (id, uid, flags) of every visible message — gives
// sequence numbers and answers flag, UID and sequence-set criteria. Criteria
// it cannot answer are left "unknown", and only the messages still undecided
// go on to the next, costlier stage: metadata rows (dates, sizes), then one
// query per search string (ILIKE over the matching columns, or a streamed
// match in Go where the database cannot fold the string's case).

// tri is a three-valued truth: a criterion over data not loaded yet is
// unknown, and NOT/OR/AND propagate that until the data arrives.
type tri uint8

const (
	triNo tri = iota
	triYes
	triUnknown
)

func (t tri) not() tri {
	switch t {
	case triNo:
		return triYes
	case triYes:
		return triNo
	}
	return triUnknown
}

// textKind says where a search string is looked for.
type textKind uint8

const (
	// textColumn: a header the messages table keeps decoded in a column.
	textColumn textKind = iota
	// textRawHeader: any other header, read from the stored RFC 822 source.
	textRawHeader
	// textBody: BODY — the message text.
	textBody
	// textText: TEXT — the header we serve plus the text.
	textText
)

// textTerm is one string criterion. field is the lower-case header name for
// textColumn/textRawHeader; value is what to find (case-insensitively; empty
// means "the header is present").
type textTerm struct {
	kind  textKind
	field string
	value string
}

// columns lists the messages columns a term is matched against.
func (t textTerm) columns() []string {
	switch t.kind {
	case textColumn:
		return []string{headerColumn(t.field)}
	case textBody:
		return []string{"body", "body_html"}
	case textText:
		return []string{"subject", "from_addr", "to_addr", "cc", "message_id", "body", "body_html"}
	}
	return nil
}

// headerColumn maps a lower-case header name to the messages column holding
// its decoded value, or "" when the header is not kept in a column.
func headerColumn(field string) string {
	switch field {
	case "from":
		return "from_addr"
	case "to":
		return "to_addr"
	case "cc":
		return "cc"
	case "bcc":
		return "bcc"
	case "reply-to":
		return "reply_to"
	case "subject":
		return "subject"
	case "message-id":
		return "message_id"
	case "in-reply-to":
		return "in_reply_to"
	case "references":
		return "message_references"
	}
	return ""
}

// searchNode is a compiled imap.SearchCriteria: every criterion in it must
// hold (AND), sets are resolved against the folder ("*" and reversed ranges),
// strings are indices into searcher.terms.
type searchNode struct {
	seqSet, uidSet          *imap.SeqSet
	withFlags, withoutFlags []string
	since, before           time.Time // internal date, day precision
	sentSince, sentBefore   time.Time // Date: header, day precision
	larger, smaller         uint32
	dateHeader              []string // HEADER Date <value>
	terms                   []int
	not                     []*searchNode
	or                      [][2]*searchNode
}

// searchItem is one message as far as the search has loaded it.
type searchItem struct {
	seq  uint32
	ref  db.FolderMessageRef
	meta *models.Message // nil until metadata is loaded
}

// searchSource is where a search reads the folder from.
type searchSource interface {
	// refs returns the folder's index, ascending UID (index i = seq i+1).
	refs() ([]db.FolderMessageRef, error)
	// meta returns metadata rows (without bodies) for ids; withSize asks for
	// RFC822.SIZE to be filled in where the stored size is still 0.
	meta(ids []int64, withSize bool) ([]*models.Message, error)
	// textHits returns which of ids match term (ids may be the whole folder).
	textHits(term textTerm, ids []int64) (map[int64]bool, error)
}

// searcher compiles criteria and evaluates them in stages.
type searcher struct {
	refs      []db.FolderMessageRef
	terms     []textTerm
	termIndex map[textTerm]int
	hits      []map[int64]bool // per term; nil until computed
	needsMeta bool
	needsSize bool
}

func (s *searcher) compile(c *imap.SearchCriteria) *searchNode {
	n := &searchNode{
		withFlags:    c.WithFlags,
		withoutFlags: c.WithoutFlags,
		since:        c.Since,
		before:       c.Before,
		sentSince:    c.SentSince,
		sentBefore:   c.SentBefore,
		larger:       c.Larger,
		smaller:      c.Smaller,
	}
	if c.SeqNum != nil {
		n.seqSet = resolveSeqSet(c.SeqNum, false, s.refs)
	}
	if c.Uid != nil {
		n.uidSet = resolveSeqSet(c.Uid, true, s.refs)
	}
	if !c.Since.IsZero() || !c.Before.IsZero() || !c.SentSince.IsZero() || !c.SentBefore.IsZero() {
		s.needsMeta = true
	}
	if c.Larger > 0 || c.Smaller > 0 {
		s.needsMeta = true
		s.needsSize = true
	}
	// Header keys come canonicalised by go-imap (Message-Id); sort them so
	// evaluation order does not depend on map iteration.
	keys := make([]string, 0, len(c.Header))
	for k := range c.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		field := strings.ToLower(k)
		for _, v := range c.Header[k] {
			switch {
			case field == "date":
				n.dateHeader = append(n.dateHeader, v)
				s.needsMeta = true
			case headerColumn(field) != "":
				n.terms = append(n.terms, s.term(textTerm{kind: textColumn, field: field, value: v}))
			default:
				n.terms = append(n.terms, s.term(textTerm{kind: textRawHeader, field: field, value: v}))
			}
		}
	}
	for _, v := range c.Body {
		if v != "" { // every message contains the empty string
			n.terms = append(n.terms, s.term(textTerm{kind: textBody, value: v}))
		}
	}
	for _, v := range c.Text {
		if v != "" {
			n.terms = append(n.terms, s.term(textTerm{kind: textText, value: v}))
		}
	}
	for _, sub := range c.Not {
		n.not = append(n.not, s.compile(sub))
	}
	for _, pair := range c.Or {
		n.or = append(n.or, [2]*searchNode{s.compile(pair[0]), s.compile(pair[1])})
	}
	return n
}

// term interns t, so a string repeated in the criteria is searched once.
func (s *searcher) term(t textTerm) int {
	if i, ok := s.termIndex[t]; ok {
		return i
	}
	s.terms = append(s.terms, t)
	s.termIndex[t] = len(s.terms) - 1
	return len(s.terms) - 1
}

// hasFlag reports a flag of an indexed message. Only messages not flagged
// \Deleted are in the index, nothing is \Recent, and keywords are not
// stored, so those never match.
func hasFlag(r db.FolderMessageRef, flag string) bool {
	switch flag {
	case imap.SeenFlag:
		return r.Seen
	case imap.FlaggedFlag:
		return r.Flagged
	case imap.AnsweredFlag:
		return r.Answered
	case imap.DraftFlag:
		return r.Draft
	}
	return false
}

// searchDay is a stored date (ms) as the UTC calendar day IMAP compares by
// (RFC 3501: time and timezone are disregarded). We serve both INTERNALDATE
// and the Date: header from the same column, in UTC.
func searchDay(ms int64) time.Time {
	t := time.UnixMilli(ms).UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// inDayRange checks day against SINCE (on or after) and BEFORE (strictly
// earlier); ON arrives from go-imap as SINCE d + BEFORE d+1.
func inDayRange(day, since, before time.Time) bool {
	if !since.IsZero() && day.Before(since) {
		return false
	}
	if !before.IsZero() && !day.Before(before) {
		return false
	}
	return true
}

// foldContains reports whether s contains the already lower-cased sub,
// ignoring case (Unicode — Cyrillic included).
func foldContains(s, lowerSub string) bool {
	return strings.Contains(strings.ToLower(s), lowerSub)
}

func (s *searcher) eval(n *searchNode, it *searchItem) tri {
	res := triYes

	if n.seqSet != nil && !n.seqSet.Contains(it.seq) {
		return triNo
	}
	if n.uidSet != nil && !n.uidSet.Contains(it.ref.UID) {
		return triNo
	}
	for _, f := range n.withFlags {
		if !hasFlag(it.ref, f) {
			return triNo
		}
	}
	for _, f := range n.withoutFlags {
		if hasFlag(it.ref, f) {
			return triNo
		}
	}

	needsMeta := !n.since.IsZero() || !n.before.IsZero() || !n.sentSince.IsZero() || !n.sentBefore.IsZero() ||
		n.larger > 0 || n.smaller > 0 || len(n.dateHeader) > 0
	if needsMeta {
		if it.meta == nil {
			res = triUnknown
		} else {
			day := searchDay(it.meta.Date)
			if !inDayRange(day, n.since, n.before) || !inDayRange(day, n.sentSince, n.sentBefore) {
				return triNo
			}
			size := uint32(it.meta.Size)
			if n.larger > 0 && size <= n.larger {
				return triNo
			}
			if n.smaller > 0 && size >= n.smaller {
				return triNo
			}
			date := time.UnixMilli(it.meta.Date).UTC().Format("Mon, 02 Jan 2006 15:04:05 -0700")
			for _, v := range n.dateHeader {
				if !foldContains(date, strings.ToLower(v)) {
					return triNo
				}
			}
		}
	}

	for _, i := range n.terms {
		if s.hits[i] == nil {
			res = triUnknown
			continue
		}
		if !s.hits[i][it.ref.ID] {
			return triNo
		}
	}

	for _, sub := range n.not {
		switch s.eval(sub, it).not() {
		case triNo:
			return triNo
		case triUnknown:
			res = triUnknown
		}
	}
	for _, pair := range n.or {
		a := s.eval(pair[0], it)
		if a == triYes {
			continue
		}
		b := s.eval(pair[1], it)
		switch {
		case b == triYes:
			continue
		case a == triNo && b == triNo:
			return triNo
		default:
			res = triUnknown
		}
	}
	return res
}

// runSearch evaluates criteria over the folder src reads and returns the
// matching UIDs (uid) or sequence numbers, ascending.
func runSearch(src searchSource, uid bool, criteria *imap.SearchCriteria) ([]uint32, error) {
	refs, err := src.refs()
	if err != nil {
		return nil, err
	}
	s := &searcher{refs: refs, termIndex: make(map[textTerm]int)}
	if criteria == nil {
		criteria = imap.NewSearchCriteria()
	}
	root := s.compile(criteria)
	s.hits = make([]map[int64]bool, len(s.terms))

	var matched, pending []*searchItem
	for i := range refs {
		it := &searchItem{seq: uint32(i + 1), ref: refs[i]}
		switch s.eval(root, it) {
		case triYes:
			matched = append(matched, it)
		case triUnknown:
			pending = append(pending, it)
		}
	}

	// Stage 2: metadata for what the index left open, a batch at a time.
	if len(pending) > 0 && s.needsMeta {
		var still []*searchItem
		for from := 0; from < len(pending); from += fetchMetaBatch {
			to := from + fetchMetaBatch
			if to > len(pending) {
				to = len(pending)
			}
			chunk := pending[from:to]
			ids := make([]int64, len(chunk))
			for i, it := range chunk {
				ids[i] = it.ref.ID
			}
			msgs, err := src.meta(ids, s.needsSize)
			if err != nil {
				return nil, err
			}
			byID := make(map[int64]*models.Message, len(msgs))
			for _, msg := range msgs {
				byID[msg.ID] = msg
			}
			for _, it := range chunk {
				it.meta = byID[it.ref.ID]
				if it.meta == nil {
					continue // deleted since the index was read
				}
				switch s.eval(root, it) {
				case triYes:
					matched = append(matched, it)
				case triUnknown:
					still = append(still, it)
				}
			}
		}
		pending = still
	}

	// Stage 3: one lookup per search string over what is still open.
	if len(pending) > 0 && len(s.terms) > 0 {
		ids := make([]int64, len(pending))
		for i, it := range pending {
			ids[i] = it.ref.ID
		}
		for i, t := range s.terms {
			h, err := src.textHits(t, ids)
			if err != nil {
				return nil, err
			}
			if h == nil {
				h = map[int64]bool{}
			}
			s.hits[i] = h
		}
		for _, it := range pending {
			if s.eval(root, it) == triYes {
				matched = append(matched, it)
			}
		}
	}

	sort.Slice(matched, func(i, j int) bool { return matched[i].seq < matched[j].seq })
	out := make([]uint32, len(matched))
	for i, it := range matched {
		if uid {
			out[i] = it.ref.UID
		} else {
			out[i] = it.seq
		}
	}
	return out, nil
}

// termMatchesValues matches a term against the values of its columns, in
// Go: substring ignoring case, or — for an empty header term — presence.
func termMatchesValues(t textTerm, values []string) bool {
	want := strings.ToLower(t.value)
	for _, v := range values {
		if want == "" {
			if v != "" {
				return true
			}
			continue
		}
		if foldContains(v, want) {
			return true
		}
	}
	return false
}

// rawHeaderMatches reports whether the header of a stored RFC 822 source
// (or its first part) has the field, with a value containing want
// (lower-cased; empty = any value) once RFC 2047 encoded words are decoded.
func rawHeaderMatches(head []byte, field, want string) bool {
	h, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(head)))
	if err != nil && h.Len() == 0 {
		return false
	}
	fields := h.FieldsByKey(field)
	for fields.Next() {
		if want == "" {
			return true
		}
		v := strings.NewReplacer("\r\n", "", "\n", "").Replace(fields.Value())
		if foldContains(parser.DecodeMIMEHeader(v), want) {
			return true
		}
	}
	return false
}

// dbSearchSource is the folder of a Mailbox in PostgreSQL.
type dbSearchSource struct {
	m *Mailbox
	// folds caches FoldsUnicodeCase for this search (nil: not asked yet).
	folds *bool
}

func (d *dbSearchSource) refs() ([]db.FolderMessageRef, error) {
	return d.m.database.GetFolderMessageRefs(d.m.folderID)
}

func (d *dbSearchSource) meta(ids []int64, withSize bool) ([]*models.Message, error) {
	msgs, err := d.m.database.GetMessagesMetaByIDs(ids)
	if err != nil {
		return nil, err
	}
	if !withSize {
		return msgs, nil
	}
	// Messages synced before sizes were stored have size 0; the size is that
	// of the RFC 822 we assemble — compute and persist it as FETCH does.
	var sizeless []int64
	idx := make(map[int64]int, len(msgs))
	for i, msg := range msgs {
		idx[msg.ID] = i
		if msg.Size == 0 {
			sizeless = append(sizeless, msg.ID)
		}
	}
	for from := 0; from < len(sizeless); from += fetchBodyBatch {
		to := from + fetchBodyBatch
		if to > len(sizeless) {
			to = len(sizeless)
		}
		full, err := d.m.database.GetMessagesByIDs(sizeless[from:to])
		if err != nil {
			return nil, err
		}
		for _, f := range full {
			size := int64(len(d.m.entireMessageBytes(f)))
			if err := d.m.database.UpdateMessageSize(f.ID, size); err != nil {
				return nil, err
			}
			if i, ok := idx[f.ID]; ok {
				msgs[i].Size = size
			}
		}
	}
	return msgs, nil
}

// sqlCanMatch reports whether ILIKE in this database matches value exactly
// as a case-insensitive substring search should.
func (d *dbSearchSource) sqlCanMatch(value string) (bool, error) {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return false, nil
	}
	ascii := true
	for i := 0; i < len(value); i++ {
		if value[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return true, nil
	}
	if d.folds == nil {
		ok, err := d.m.database.FoldsUnicodeCase()
		if err != nil {
			return false, err
		}
		d.folds = &ok
	}
	return *d.folds, nil
}

func (d *dbSearchSource) textHits(t textTerm, ids []int64) (map[int64]bool, error) {
	want := make(map[int64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	hits := make(map[int64]bool)

	if t.kind == textRawHeader {
		lower := strings.ToLower(t.value)
		err := d.m.database.ScanFolderRawHeads(d.m.folderID, ids, func(id int64, head []byte) error {
			if want[id] && rawHeaderMatches(head, t.field, lower) {
				hits[id] = true
			}
			return nil
		})
		return hits, err
	}

	cols := t.columns()
	viaSQL, err := d.sqlCanMatch(t.value)
	if err != nil {
		return nil, err
	}
	if viaSQL {
		found, err := d.m.database.SearchFolderTextIDs(d.m.folderID, ids, cols, t.value)
		if err != nil {
			return nil, err
		}
		for _, id := range found {
			if want[id] {
				hits[id] = true
			}
		}
		return hits, nil
	}
	err = d.m.database.ScanFolderTextColumns(d.m.folderID, ids, cols, func(id int64, values []string) error {
		if want[id] && termMatchesValues(t, values) {
			hits[id] = true
		}
		return nil
	})
	return hits, err
}
