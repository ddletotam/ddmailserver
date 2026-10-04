package server

import (
	"bufio"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/commands"
)

// memSearchSource is a folder in memory; it records what each stage asked
// for, so tests can check that cheap criteria do not load more.
type memSearchSource struct {
	msgs     []*models.Message // visible messages
	raw      map[int64]string  // stored RFC 822 source
	metaIDs  []int64
	textReqs []textTerm
	textIDs  []int64
}

func (s *memSearchSource) refs() ([]db.FolderMessageRef, error) {
	sorted := append([]*models.Message(nil), s.msgs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].UID < sorted[j].UID })
	var out []db.FolderMessageRef
	for _, m := range sorted {
		out = append(out, db.FolderMessageRef{ID: m.ID, UID: m.UID, Seen: m.Seen, Flagged: m.Flagged, Answered: m.Answered, Draft: m.Draft})
	}
	return out, nil
}

func (s *memSearchSource) byID(id int64) *models.Message {
	for _, m := range s.msgs {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func (s *memSearchSource) meta(ids []int64, withSize bool) ([]*models.Message, error) {
	s.metaIDs = append(s.metaIDs, ids...)
	var out []*models.Message
	for _, id := range ids {
		if m := s.byID(id); m != nil {
			c := *m
			c.Body, c.BodyHTML = "", ""
			out = append(out, &c)
		}
	}
	return out, nil
}

func (s *memSearchSource) textHits(t textTerm, ids []int64) (map[int64]bool, error) {
	s.textReqs = append(s.textReqs, t)
	s.textIDs = append(s.textIDs, ids...)
	hits := map[int64]bool{}
	for _, id := range ids {
		m := s.byID(id)
		if m == nil {
			continue
		}
		if t.kind == textRawHeader {
			if raw, ok := s.raw[id]; ok && rawHeaderMatches([]byte(raw), t.field, strings.ToLower(t.value)) {
				hits[id] = true
			}
			continue
		}
		col := map[string]string{
			"subject": m.Subject, "from_addr": m.From, "to_addr": m.To, "cc": m.Cc, "bcc": m.Bcc,
			"reply_to": m.ReplyTo, "message_id": m.MessageID, "in_reply_to": m.InReplyTo,
			"message_references": m.MessageReferences, "body": m.Body, "body_html": m.BodyHTML,
		}
		var values []string
		for _, c := range t.columns() {
			values = append(values, col[c])
		}
		if termMatchesValues(t, values) {
			hits[id] = true
		}
	}
	return hits, nil
}

func ms(s string) int64 {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t.UnixMilli()
}

func searchFixture() *memSearchSource {
	return &memSearchSource{
		msgs: []*models.Message{
			{ID: 101, UID: 10, Seen: true, Subject: "Hello world", From: "Alice <alice@example.org>", To: "bob@example.org",
				MessageID: "<a1@example.org>", Date: ms("2024-03-01 10:00"), Size: 1000, Body: "Quarterly report attached"},
			{ID: 102, UID: 11, Flagged: true, Subject: "Счёт за МАРТ", From: "Иван Петров <ivan@example.ru>", To: "bob@example.org",
				Cc: "carol@example.org", MessageID: "<b2@example.org>", Date: ms("2024-03-02 23:30"), Size: 5000,
				Body: "Оплатите счёт до пятницы", BodyHTML: "<p>Оплатите</p>"},
			{ID: 103, UID: 12, Answered: true, Draft: true, Subject: "Re: Hello", From: "bob@example.org", Bcc: "secret@example.org",
				MessageID: "<c3@example.org>", InReplyTo: "<a1@example.org>", Date: ms("2024-03-05 00:00"), Size: 300,
				Body: "100% sure_thing"},
			{ID: 104, UID: 20, Date: ms("2024-04-01 12:00"), Size: 70000},
		},
		raw: map[int64]string{
			101: "From: Alice <alice@example.org>\r\nX-Priority: 1\r\nList-Id: Dev list\r\n <dev.example.org>\r\n\r\nQuarterly report attached\r\n",
			103: "From: bob@example.org\r\nX-Note: =?utf-8?B?0J/RgNC40LLQtdGC?=\r\n\r\n100% sure_thing\r\n",
		},
	}
}

// parseSearch parses a SEARCH command's arguments the way the server does.
func parseSearch(t *testing.T, args string) *imap.SearchCriteria {
	t.Helper()
	r := imap.NewReader(bufio.NewReader(strings.NewReader(args + "\r\n")))
	fields, err := r.ReadLine()
	if err != nil {
		t.Fatalf("read %q: %v", args, err)
	}
	cmd := &commands.Search{}
	if err := cmd.Parse(fields); err != nil {
		t.Fatalf("parse %q: %v", args, err)
	}
	return cmd.Criteria
}

func TestSearchCriteria(t *testing.T) {
	all := []uint32{10, 11, 12, 20}
	cases := []struct {
		query string
		want  []uint32
	}{
		{"ALL", all},
		{"SEEN", []uint32{10}},
		{"UNSEEN", []uint32{11, 12, 20}},
		{"FLAGGED", []uint32{11}},
		{"UNFLAGGED", []uint32{10, 12, 20}},
		{"ANSWERED", []uint32{12}},
		{"UNANSWERED", []uint32{10, 11, 20}},
		{"DRAFT", []uint32{12}},
		{"UNDRAFT", []uint32{10, 11, 20}},
		{"DELETED", nil},
		{"UNDELETED", all},
		{"RECENT", nil},
		{"NEW", nil},
		{"OLD", all},
		{"KEYWORD $Forwarded", nil},
		{"UNKEYWORD $Forwarded", all},

		{"UID 11:12", []uint32{11, 12}},
		{"UID 15:*", []uint32{20}},
		{"UID *", []uint32{20}},
		{"UID 100:*", []uint32{20}},
		{"UID 1:5", nil},
		{"2:3", []uint32{11, 12}},
		{"*", []uint32{20}},
		{"1,4", []uint32{10, 20}},

		{"HEADER Message-ID <b2@example.org>", []uint32{11}},
		{"HEADER MESSAGE-ID B2@EXAMPLE", []uint32{11}},
		{`HEADER Message-ID ""`, []uint32{10, 11, 12}},
		{"HEADER In-Reply-To <a1@example.org>", []uint32{12}},
		{"HEADER References a1", nil},
		{"HEADER X-Priority 1", []uint32{10}},
		{`HEADER List-Id "dev.example.org"`, []uint32{10}},
		{`HEADER List-Id ""`, []uint32{10}},
		{"HEADER X-Note привет", []uint32{12}},
		{`HEADER X-Missing ""`, nil},
		{`HEADER Date "Mar 2024"`, []uint32{10, 11, 12}},

		{"FROM alice", []uint32{10}},
		{"FROM иван", []uint32{11}},
		{"FROM ИВАН", []uint32{11}},
		{"TO bob", []uint32{10, 11}},
		{"CC carol", []uint32{11}},
		{"BCC secret", []uint32{12}},
		{"SUBJECT hello", []uint32{10, 12}},
		{`SUBJECT "счёт за март"`, []uint32{11}},
		{"CHARSET UTF-8 SUBJECT март", []uint32{11}},

		{"BODY quarterly", []uint32{10}},
		{"BODY ОПЛАТИТЕ", []uint32{11}},
		{`BODY "<p>"`, []uint32{11}},
		{"BODY 100%", []uint32{12}},
		{"BODY _thing", []uint32{12}},
		{`BODY ""`, all},
		{"TEXT alice", []uint32{10}},
		{"TEXT пятницы", []uint32{11}},
		{"TEXT b2@example", []uint32{11}},
		{"TEXT nowhere", nil},

		{"SINCE 2-Mar-2024", []uint32{11, 12, 20}},
		{"ON 2-Mar-2024", []uint32{11}},
		{"BEFORE 2-Mar-2024", []uint32{10}},
		{"SENTON 5-Mar-2024", []uint32{12}},
		{"SENTSINCE 1-Apr-2024", []uint32{20}},
		{"SENTBEFORE 1-Mar-2024", nil},
		{"SINCE 1-Mar-2024 BEFORE 5-Mar-2024", []uint32{10, 11}},

		{"LARGER 999", []uint32{10, 11, 20}},
		{"SMALLER 1000", []uint32{12}},
		{"LARGER 300 SMALLER 5000", []uint32{10}},

		{"NOT SEEN", []uint32{11, 12, 20}},
		{"OR SEEN FLAGGED", []uint32{10, 11}},
		{"OR SUBJECT hello FROM иван", []uint32{10, 11, 12}},
		{"NOT OR SEEN FLAGGED", []uint32{12, 20}},
		{"OR (SEEN SUBJECT hello) (BODY счёт SINCE 1-Mar-2024)", []uint32{10, 11}},
		{"NOT HEADER Message-ID b2", []uint32{10, 12, 20}},
		{"UNSEEN NOT BODY оплатите", []uint32{12, 20}},
		{"NOT NOT FLAGGED", []uint32{11}},
		{"OR OR SEEN FLAGGED DRAFT", []uint32{10, 11, 12}},
		{"UID 10:12 NOT OR SUBJECT hello LARGER 4000", nil},
		{"UID 10:20 NOT OR SUBJECT hello LARGER 100000", []uint32{11, 20}},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			got, err := runSearch(searchFixture(), true, parseSearch(t, c.query))
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(got) == 0 {
				got = nil
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("UID SEARCH %s = %v, want %v", c.query, got, c.want)
			}
		})
	}
}

func TestSearchSequenceNumbers(t *testing.T) {
	got, err := runSearch(searchFixture(), false, parseSearch(t, "OR FLAGGED UID 20"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if want := []uint32{2, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SEARCH = %v, want %v", got, want)
	}
}

// TestSearchStages checks that each stage loads only what the previous one
// left undecided.
func TestSearchStages(t *testing.T) {
	src := searchFixture()
	if _, err := runSearch(src, true, parseSearch(t, "UNSEEN UID 1:15 FLAGGED")); err != nil {
		t.Fatal(err)
	}
	if len(src.metaIDs) != 0 || len(src.textReqs) != 0 {
		t.Fatalf("flag/UID search loaded meta %v, text %v", src.metaIDs, src.textReqs)
	}

	src = searchFixture()
	if _, err := runSearch(src, true, parseSearch(t, "UNSEEN SINCE 2-Mar-2024 SUBJECT март")); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(src.metaIDs) != "[102 103 104]" {
		t.Fatalf("meta loaded for %v, want the unseen ones", src.metaIDs)
	}
	if len(src.textReqs) != 1 || fmt.Sprint(src.textIDs) != "[102 103 104]" {
		t.Fatalf("text searched %v over %v", src.textReqs, src.textIDs)
	}

	// The same string twice is looked up once.
	src = searchFixture()
	if _, err := runSearch(src, true, parseSearch(t, "OR SUBJECT x (NOT SUBJECT x)")); err != nil {
		t.Fatal(err)
	}
	if len(src.textReqs) != 1 {
		t.Fatalf("text lookups %v, want 1", src.textReqs)
	}
}

func TestLikeContainsPattern(t *testing.T) {
	if got, want := db.LikeContainsPattern(`50%_a\b`), `%50\%\_a\\b%`; got != want {
		t.Fatalf("pattern %q, want %q", got, want)
	}
}
