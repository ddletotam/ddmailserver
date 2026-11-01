package mx

import (
	"errors"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/emersion/go-smtp"
)

type fakeDirectory struct{ err error }

func (f fakeDirectory) GetDomainByName(name string) (*models.Domain, error) {
	if f.err != nil {
		return nil, f.err
	}
	if name == "ours.test" {
		return &models.Domain{ID: 1, Domain: name, Enabled: true}, nil
	}
	return nil, db.ErrDomainNotFound
}

func (f fakeDirectory) GetMailbox(_ int64, local string) (*models.Mailbox, error) {
	if local == "bob" {
		return &models.Mailbox{ID: 1, UserID: 20, LocalPart: local, Enabled: true}, nil
	}
	return nil, db.ErrMailboxNotFound
}

// RCPT rejections must be real *smtp.SMTPError: go-smtp answers any other
// error with 451, which made the sending MTA retry instead of bouncing.
func TestRcptRepliesAreRealSMTPCodes(t *testing.T) {
	cases := []struct {
		dir  fakeDirectory
		to   string
		code int
	}{
		{fakeDirectory{}, "<bob@ours.test>", 0},
		{fakeDirectory{}, "<nobody@ours.test>", 550},
		{fakeDirectory{}, "<x@elsewhere.test>", 550},
		{fakeDirectory{err: errors.New("db down")}, "<bob@ours.test>", 451},
	}
	for _, c := range cases {
		s := &Session{directory: c.dir}
		err := s.Rcpt(c.to, nil)
		if c.code == 0 {
			if err != nil || len(s.recipients) != 1 || s.recipients[0].Mailbox.UserID != 20 {
				t.Errorf("RCPT %s: got %v, want accepted", c.to, err)
			}
			continue
		}
		var se *smtp.SMTPError
		if !errors.As(err, &se) || se.Code != c.code {
			t.Errorf("RCPT %s: got %#v, want SMTP %d", c.to, err, c.code)
		}
	}
}
