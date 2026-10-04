package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-smtp"
)

type fakeSenders struct {
	accounts  []*models.Account
	mailboxes []*db.MailboxWithDomain
	err       error
}

func (f *fakeSenders) GetAccountsByUserID(int64) ([]*models.Account, error) {
	return f.accounts, f.err
}

func (f *fakeSenders) GetMailboxesWithDomainByUserID(int64) ([]*db.MailboxWithDomain, error) {
	return f.mailboxes, f.err
}

func mailbox(userID int64, local, domain string, enabled bool) *db.MailboxWithDomain {
	m := &db.MailboxWithDomain{DomainName: domain}
	m.UserID = userID
	m.LocalPart = local
	m.Enabled = enabled
	return m
}

// User 7 owns: mailbox me@local.test (direct), external account
// ext@gmail.test (id 11) with alias alias@corp.test, a disabled account
// old@yahoo.test (id 12), and a disabled mailbox off@local.test.
func testStore() *fakeSenders {
	return &fakeSenders{
		accounts: []*models.Account{
			{ID: 11, UserID: 7, Email: "Ext@Gmail.test", Enabled: true, Aliases: "alias@corp.test, me@local.test"},
			{ID: 12, UserID: 7, Email: "old@yahoo.test", Enabled: false},
		},
		mailboxes: []*db.MailboxWithDomain{
			mailbox(7, "me", "local.test", true),
			mailbox(7, "off", "local.test", false),
		},
	}
}

func authedSession() *Session {
	return &Session{userID: 7, username: "alice", senders: testStore()}
}

func smtpCode(err error) int {
	var se *smtp.SMTPError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

func TestMailRcptRequireAuth(t *testing.T) {
	s := &Session{senders: testStore()}
	if err := s.Mail("me@local.test", nil); err != smtp.ErrAuthRequired {
		t.Fatalf("Mail before AUTH: got %v, want ErrAuthRequired", err)
	}
	if err := s.Rcpt("x@example.com", nil); err != smtp.ErrAuthRequired {
		t.Fatalf("Rcpt before AUTH: got %v, want ErrAuthRequired", err)
	}
	if err := s.Data(strings.NewReader("")); err != smtp.ErrAuthRequired {
		t.Fatalf("Data before AUTH: got %v, want ErrAuthRequired", err)
	}
}

func TestMailFromOwnership(t *testing.T) {
	cases := []struct {
		from    string
		ok      bool
		account int64
	}{
		{"me@local.test", true, 0},          // local mailbox → direct delivery
		{"<ME@Local.Test>", true, 0},        // case/brackets don't matter
		{"ext@gmail.test", true, 11},        // external account → relay
		{"alias@corp.test", true, 11},       // alias → relay via its account
		{"boss@local.test", false, 0},       // same local domain, not a mailbox of ours
		{"off@local.test", false, 0},        // disabled mailbox
		{"old@yahoo.test", false, 0},        // disabled account
		{"someone@else.test", false, 0},     // unrelated
		{"", false, 0},                      // null reverse-path
		{"me@local.test.evil", false, 0},    // suffix trick
		{"Name <ext@gmail.test>", true, 11}, // display-name form
	}
	for _, c := range cases {
		s := authedSession()
		err := s.Mail(c.from, nil)
		if c.ok {
			if err != nil {
				t.Errorf("Mail(%q): unexpected error %v", c.from, err)
				continue
			}
			if s.accountID != c.account {
				t.Errorf("Mail(%q): account %d, want %d", c.from, s.accountID, c.account)
			}
		} else if smtpCode(err) != 553 {
			t.Errorf("Mail(%q): got %v, want 553", c.from, err)
		}
	}
}

func TestAccountAddressBeatsMailboxAndAlias(t *testing.T) {
	// me@local.test is both a mailbox and an alias of account 11: the
	// mailbox wins (direct delivery). An account's own address wins over a
	// mailbox with the same address.
	st := testStore()
	st.mailboxes = append(st.mailboxes, mailbox(7, "ext", "gmail.test", true))
	owned, err := db.SenderIdentities(st, 7)
	if err != nil {
		t.Fatal(err)
	}
	if owned["me@local.test"] != 0 {
		t.Errorf("mailbox vs alias: got account %d", owned["me@local.test"])
	}
	if owned["ext@gmail.test"] != 11 {
		t.Errorf("account vs mailbox: got account %d", owned["ext@gmail.test"])
	}
}

func TestOtherUsersRowsIgnored(t *testing.T) {
	st := &fakeSenders{
		accounts:  []*models.Account{{ID: 99, UserID: 8, Email: "victim@x.test", Enabled: true}},
		mailboxes: []*db.MailboxWithDomain{mailbox(8, "victim", "local.test", true)},
	}
	owned, err := db.SenderIdentities(st, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 0 {
		t.Fatalf("foreign rows leaked into identities: %v", owned)
	}
}

func TestMailStoreErrorIsTemporary(t *testing.T) {
	s := &Session{userID: 7, senders: &fakeSenders{err: errors.New("db down")}}
	if code := smtpCode(s.Mail("me@local.test", nil)); code != 451 {
		t.Fatalf("got %d, want 451", code)
	}
}

func message(from string) string {
	return "From: " + from + "\r\nTo: bob@example.com\r\nSubject: hi\r\nMessage-Id: <1@local.test>\r\n\r\nbody\r\n"
}

func TestDataRejectsForeignHeaderFrom(t *testing.T) {
	cases := []struct {
		name, msg string
	}{
		{"forged From", message("CEO <ceo@local.test>")},
		{"one of two From foreign", message("me@local.test, ceo@local.test")},
		{"missing From", "To: bob@example.com\r\nMessage-Id: <2@local.test>\r\n\r\nbody\r\n"},
		{"foreign Sender", "From: me@local.test\r\nSender: ceo@local.test\r\nMessage-Id: <3@local.test>\r\n\r\nbody\r\n"},
	}
	for _, c := range cases {
		s := authedSession()
		if err := s.Mail("me@local.test", nil); err != nil {
			t.Fatal(err)
		}
		if err := s.Rcpt("bob@example.com", nil); err != nil {
			t.Fatal(err)
		}
		// No database: an accepted message would panic on the Sent lookup,
		// so a 550 here proves rejection happens before anything is stored.
		if code := smtpCode(s.Data(strings.NewReader(c.msg))); code != 550 {
			t.Errorf("%s: got code %d, want 550", c.name, code)
		}
	}
}

func TestCheckHeaderSendersAcceptsOwned(t *testing.T) {
	s := authedSession()
	for _, from := range []string{"Me <me@local.test>", "ext@gmail.test", "alias@corp.test"} {
		h := headerOf(t, message(from))
		if err := s.checkHeaderSenders(h); err != nil {
			t.Errorf("From %q rejected: %v", from, err)
		}
	}
}

func headerOf(t *testing.T, raw string) mail.Header {
	t.Helper()
	mr, err := mail.CreateReader(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return mr.Header
}
