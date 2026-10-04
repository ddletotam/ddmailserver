package server

import (
	"testing"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/emersion/go-imap"
)

func TestResolveSeqSet(t *testing.T) {
	msgs := []*models.Message{{UID: 10}, {UID: 12}, {UID: 15}}
	parse := func(s string) *imap.SeqSet {
		set, err := imap.ParseSeqSet(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return set
	}
	cases := []struct {
		set  string
		uid  bool
		in   []uint32
		out  []uint32
		name string
	}{
		{"*", false, []uint32{3}, []uint32{1, 2}, "bare * is the last sequence number"},
		{"*", true, []uint32{15}, []uint32{10, 12}, "bare UID * is the highest UID"},
		{"16:*", true, []uint32{15}, []uint32{10, 12}, "n:* past the end still yields the last message"},
		{"11:*", true, []uint32{12, 15}, []uint32{10}, "n:* from the middle"},
		{"3:1", false, []uint32{1, 2, 3}, nil, "reversed range"},
		{"2", false, []uint32{2}, []uint32{1, 3}, "plain number unchanged"},
	}
	for _, c := range cases {
		got := resolveSeqSet(parse(c.set), c.uid, msgs)
		for _, id := range c.in {
			if !got.Contains(id) {
				t.Errorf("%s: %q should contain %d", c.name, c.set, id)
			}
		}
		for _, id := range c.out {
			if got.Contains(id) {
				t.Errorf("%s: %q should not contain %d", c.name, c.set, id)
			}
		}
	}
	if !resolveSeqSet(parse("*"), true, nil).Empty() {
		t.Error("empty mailbox resolves to an empty set")
	}
}
