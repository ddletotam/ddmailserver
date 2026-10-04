package server

import (
	"testing"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/emersion/go-imap"
)

func TestIMAPFlagUpdate(t *testing.T) {
	start := db.MessageFlags{Seen: true, Flagged: true, Draft: true}
	cases := []struct {
		name  string
		op    imap.FlagsOp
		flags []string
		want  db.MessageFlags
	}{
		{"add deleted keeps the rest", imap.AddFlags, []string{imap.DeletedFlag},
			db.MessageFlags{Seen: true, Flagged: true, Deleted: true, Draft: true}},
		{"remove seen", imap.RemoveFlags, []string{imap.SeenFlag},
			db.MessageFlags{Flagged: true, Draft: true}},
		{"set replaces the four system flags", imap.SetFlags, []string{imap.AnsweredFlag},
			db.MessageFlags{Answered: true, Draft: true}},
		{"set to nothing clears them", imap.SetFlags, nil,
			db.MessageFlags{Draft: true}},
		{"draft and keywords are not stored via IMAP", imap.AddFlags, []string{imap.DraftFlag, "$Label1"},
			start},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := imapFlagUpdate(c.op, c.flags).Apply(start); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}
