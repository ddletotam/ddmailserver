package db

import (
	"testing"

	"github.com/yourusername/mailserver/internal/models"
)

type fakeSenderStore struct {
	accounts  []*models.Account
	mailboxes []*MailboxWithDomain
}

func (f *fakeSenderStore) GetAccountsByUserID(int64) ([]*models.Account, error) {
	return f.accounts, nil
}

func (f *fakeSenderStore) GetMailboxesWithDomainByUserID(int64) ([]*MailboxWithDomain, error) {
	return f.mailboxes, nil
}

func TestSenderIdentities(t *testing.T) {
	mb := func(user int64, local string, enabled bool) *MailboxWithDomain {
		m := &MailboxWithDomain{DomainName: "Local.Test"}
		m.UserID, m.LocalPart, m.Enabled = user, local, enabled
		return m
	}
	st := &fakeSenderStore{
		accounts: []*models.Account{
			{ID: 11, UserID: 7, Email: "ext@gmail.test", Enabled: true, Aliases: "alias@corp.test"},
			{ID: 12, UserID: 7, Email: "off@yahoo.test", Enabled: false, Aliases: "offalias@corp.test"},
			{ID: 13, UserID: 8, Email: "foreign@gmail.test", Enabled: true},
		},
		mailboxes: []*MailboxWithDomain{mb(7, "me", true), mb(7, "off", false), mb(8, "victim", true)},
	}
	owned, err := SenderIdentities(st, 7)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"me@local.test": 0, "ext@gmail.test": 11, "alias@corp.test": 11}
	if len(owned) != len(want) {
		t.Fatalf("got %v, want %v", owned, want)
	}
	for addr, acc := range want {
		if got, ok := owned[addr]; !ok || got != acc {
			t.Errorf("%s: got (%d,%v), want %d", addr, got, ok, acc)
		}
	}
}

func TestParseSingleSender(t *testing.T) {
	ok := map[string]string{
		"me@local.test":                       "me@local.test",
		"<Me@Local.Test>":                     "me@local.test",
		"Me <me@local.test>":                  "me@local.test",
		"Иван Петров <ivan@local.test>":       "ivan@local.test",
		"=?utf-8?b?0JjQstCw0L0=?= <i@l.test>": "i@l.test",
	}
	for in, want := range ok {
		got, err := ParseSingleSender(in)
		if err != nil || got != want {
			t.Errorf("ParseSingleSender(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"",
		"me@local.test, ceo@local.test",
		"Me <me@local.test>, <ceo@local.test>",
		"me@local.test\r\nFrom: ceo@local.test",
		"not an address",
	} {
		if _, err := ParseSingleSender(in); err == nil {
			t.Errorf("ParseSingleSender(%q) accepted", in)
		}
	}
}
