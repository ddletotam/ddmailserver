package localdelivery

import (
	"errors"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/notify"
	"github.com/ddletotam/ddmailserver/internal/parser"
)

// fakeStore is an in-memory Store: domains by name, mailboxes by
// "domainID/local", messages keyed by (user, Message-ID) like the real index.
type fakeStore struct {
	domains    map[string]*models.Domain
	mailboxes  map[string]*models.Mailbox
	lookupErr  error
	createErr  error
	spamAction string
	spamRule   *db.SpamRule

	messages    []*models.Message
	attachments []*models.Attachment
	nextID      int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		domains: map[string]*models.Domain{
			"ours.test": {ID: 1, Domain: "ours.test", Enabled: true},
			"off.test":  {ID: 2, Domain: "off.test", Enabled: false},
		},
		mailboxes: map[string]*models.Mailbox{
			"1/bob":   {ID: 10, UserID: 20, DomainID: 1, LocalPart: "bob", Enabled: true},
			"1/carol": {ID: 11, UserID: 21, DomainID: 1, LocalPart: "carol", Enabled: true},
			"1/gone":  {ID: 12, UserID: 22, DomainID: 1, LocalPart: "gone", Enabled: false},
		},
	}
}

func (f *fakeStore) GetDomainByName(name string) (*models.Domain, error) {
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	if d, ok := f.domains[name]; ok {
		return d, nil
	}
	return nil, db.ErrDomainNotFound
}

func (f *fakeStore) GetMailbox(domainID int64, local string) (*models.Mailbox, error) {
	for _, m := range f.mailboxes {
		if m.DomainID == domainID && m.LocalPart == local {
			return m, nil
		}
	}
	return nil, db.ErrMailboxNotFound
}

func (f *fakeStore) CheckSpamRules(int64, string) (string, *db.SpamRule, error) {
	return f.spamAction, f.spamRule, nil
}
func (f *fakeStore) GetDisabledSpamChecksMap(int64) (map[string]bool, error) { return nil, nil }
func (f *fakeStore) GetSpamCheckWeights(int64) (map[string]float64, error)   { return nil, nil }
func (f *fakeStore) GetOrCreateLocalInbox(userID int64) (*models.Folder, error) {
	return &models.Folder{ID: 1000 + userID, UserID: userID}, nil
}
func (f *fakeStore) GetNextUIDForFolder(int64) (uint32, error) {
	return uint32(len(f.messages) + 1), nil
}
func (f *fakeStore) CreateMessage(m *models.Message) error {
	if f.createErr != nil {
		return f.createErr
	}
	for _, e := range f.messages {
		if e.UserID == m.UserID && e.MessageID == m.MessageID && m.MessageID != "" {
			return db.ErrDuplicateMessage
		}
	}
	f.nextID++
	m.ID = f.nextID
	f.messages = append(f.messages, m)
	return nil
}
func (f *fakeStore) CreateAttachment(a *models.Attachment) error {
	f.attachments = append(f.attachments, a)
	return nil
}
func (f *fakeStore) GetMessageCountByFolder(int64) (uint32, error) {
	return uint32(len(f.messages)), nil
}
func (f *fakeStore) GetUserByID(id int64) (*models.User, error) {
	return &models.User{ID: id, Username: "user"}, nil
}

func TestLookup(t *testing.T) {
	store := newFakeStore()
	cases := []struct {
		addr   string
		user   int64  // >0: resolves to this user
		local  bool   // false: ErrNotLocal
		status string // RejectError status
	}{
		{addr: "bob@ours.test", user: 20},
		{addr: "Bob@OURS.test", user: 20},
		{addr: "someone@elsewhere.test", local: false},
		{addr: "nobody@ours.test", status: "5.1.1"},
		{addr: "gone@ours.test", status: "5.2.1"},
		{addr: "bob@off.test", status: "5.7.1"},
		{addr: "not-an-address", status: "5.1.3"},
	}
	for _, c := range cases {
		rcpt, err := Lookup(store, c.addr)
		var reject *RejectError
		switch {
		case c.user > 0:
			if err != nil || rcpt.Mailbox.UserID != c.user {
				t.Errorf("%s: got %v, %v; want user %d", c.addr, rcpt, err, c.user)
			}
		case c.status != "":
			if !errors.As(err, &reject) || reject.Status() != c.status || reject.Code != 550 {
				t.Errorf("%s: got %v; want 550 %s", c.addr, err, c.status)
			}
		default:
			if !errors.Is(err, ErrNotLocal) {
				t.Errorf("%s: got %v; want ErrNotLocal", c.addr, err)
			}
		}
	}
}

func TestLookupDatabaseErrorIsNotNotLocal(t *testing.T) {
	// A DB failure must not be read as "not our domain": the message would be
	// routed to MX, i.e. back at ourselves through NAT.
	store := newFakeStore()
	store.lookupErr = errors.New("connection refused")
	_, err := Lookup(store, "bob@ours.test")
	var reject *RejectError
	if err == nil || errors.Is(err, ErrNotLocal) || errors.As(err, &reject) {
		t.Fatalf("got %v; want a temporary (non-reject, non-NotLocal) error", err)
	}
}

func parse(t *testing.T, raw string) *parser.ParsedMessage {
	t.Helper()
	p, err := parser.New().ParseBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const testMsg = "From: Alice <alice@ours.test>\r\n" +
	"To: bob@ours.test, carol@ours.test\r\n" +
	"Subject: hello\r\n" +
	"Message-ID: <m1@ours.test>\r\n" +
	"Date: Sun, 01 Nov 2026 10:00:00 +0300\r\n" +
	"\r\n" +
	"body\r\n"

func TestDeliverStoresNotifiesAndDedups(t *testing.T) {
	store := newFakeStore()
	hub := notify.NewHub()
	events := hub.Subscribe(20)
	d := NewWithStore(store, hub, nil, nil)
	var itipCalls int
	d.SetITIP(func(*parser.ParsedMessage, int64, int64, string) (bool, error) {
		itipCalls++
		return false, nil
	})

	bob, _ := Lookup(store, "bob@ours.test")
	carol, _ := Lookup(store, "carol@ours.test")
	results, errs := d.Deliver(Envelope{From: "alice@ours.test", Trusted: true}, []byte(testMsg), parse(t, testMsg), []*Recipient{bob, carol})
	for i, r := range results {
		if r != Delivered || errs[i] != nil {
			t.Fatalf("recipient %d: %v %v", i, r, errs[i])
		}
	}
	if len(store.messages) != 2 {
		t.Fatalf("stored %d messages, want 2", len(store.messages))
	}
	m := store.messages[0]
	if m.UserID != 20 || m.FolderID != 1020 || m.MessageID != "m1@ours.test" || m.Subject != "hello" || m.AccountID != 0 || m.Seen || m.IsSpam {
		t.Fatalf("unexpected stored message: %+v", m)
	}
	if string(m.RawEmail) != testMsg {
		t.Fatal("raw message not stored verbatim")
	}
	select {
	case ev := <-events:
		if ev.Type != notify.EventNewMessage || ev.Mailbox != "INBOX" || ev.MessageID != m.ID {
			t.Fatalf("unexpected event %+v", ev)
		}
	default:
		t.Fatal("no new-message notification for the recipient")
	}
	if itipCalls != 2 {
		t.Fatalf("iTIP hook called %d times, want 2", itipCalls)
	}

	// A retry delivers again: recipients already holding the message are
	// reported as Duplicate and get no second copy.
	results, _ = d.Deliver(Envelope{Trusted: true}, []byte(testMsg), parse(t, testMsg), []*Recipient{bob})
	if results[0] != Duplicate || len(store.messages) != 2 {
		t.Fatalf("redelivery: result %v, %d messages", results[0], len(store.messages))
	}
}

func TestDeliverHonoursRecipientBlacklist(t *testing.T) {
	store := newFakeStore()
	store.spamAction = "spam"
	store.spamRule = &db.SpamRule{ID: 5, Action: "spam"}
	hub := notify.NewHub()
	events := hub.Subscribe(20)
	d := NewWithStore(store, hub, nil, nil)

	bob, _ := Lookup(store, "bob@ours.test")
	results, _ := d.Deliver(Envelope{Trusted: true}, []byte(testMsg), parse(t, testMsg), []*Recipient{bob})
	if results[0] != Delivered {
		t.Fatalf("result %v", results[0])
	}
	m := store.messages[0]
	if !m.IsSpam || m.SpamRuleID == nil || *m.SpamRuleID != 5 {
		t.Fatalf("blacklisted sender not marked spam: %+v", m)
	}
	select {
	case ev := <-events:
		t.Fatalf("spam must not notify, got %+v", ev)
	default:
	}
}

func TestDeliverStoreFailureIsFailed(t *testing.T) {
	store := newFakeStore()
	store.createErr = errors.New("disk full")
	d := NewWithStore(store, nil, nil, nil)
	bob, _ := Lookup(store, "bob@ours.test")
	results, errs := d.Deliver(Envelope{}, []byte(testMsg), parse(t, testMsg), []*Recipient{bob})
	if results[0] != Failed || errs[0] == nil {
		t.Fatalf("got %v %v; want Failed with error", results[0], errs[0])
	}
}
