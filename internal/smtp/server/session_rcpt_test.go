package server

import (
	"errors"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
)

// fakeDirectory hosts local.test with mailboxes me (enabled) and off
// (disabled).
type fakeDirectory struct{ err error }

func (f fakeDirectory) GetDomainByName(name string) (*models.Domain, error) {
	if f.err != nil {
		return nil, f.err
	}
	if name == "local.test" {
		return &models.Domain{ID: 1, Domain: name, Enabled: true}, nil
	}
	return nil, db.ErrDomainNotFound
}

func (f fakeDirectory) GetMailbox(_ int64, local string) (*models.Mailbox, error) {
	switch local {
	case "me":
		return &models.Mailbox{ID: 1, UserID: 7, LocalPart: local, Enabled: true}, nil
	case "off":
		return &models.Mailbox{ID: 2, UserID: 7, LocalPart: local, Enabled: false}, nil
	}
	return nil, db.ErrMailboxNotFound
}

func TestRcptLocalRecipients(t *testing.T) {
	cases := []struct {
		to   string
		code int // 0 = accepted
	}{
		{"me@local.test", 0},
		{"Someone <ME@Local.test>", 0},
		{"anyone@example.com", 0}, // not ours: queued for MX as before
		{"nobody@local.test", 550},
		{"off@local.test", 550},
	}
	for _, c := range cases {
		s := authedSession()
		s.directory = fakeDirectory{}
		err := s.Rcpt(c.to, nil)
		if got := smtpCode(err); got != c.code || (c.code == 0 && err != nil) {
			t.Errorf("RCPT %s: got %v (code %d), want code %d", c.to, err, got, c.code)
		}
		if c.code != 0 && len(s.to) != 0 {
			t.Errorf("RCPT %s: rejected recipient was kept", c.to)
		}
	}
}

func TestRcptLookupFailureIsTemporary(t *testing.T) {
	s := authedSession()
	s.directory = fakeDirectory{err: errors.New("db down")}
	if code := smtpCode(s.Rcpt("me@local.test", nil)); code != 451 {
		t.Fatalf("got code %d, want 451", code)
	}
}
