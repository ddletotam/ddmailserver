package server

import (
	"testing"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/emersion/go-imap"
)

func parseSet(t *testing.T, s string) *imap.SeqSet {
	t.Helper()
	set, err := imap.ParseSeqSet(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return set
}

func TestResolveSeqSet(t *testing.T) {
	msgs := []db.FolderMessageRef{{UID: 10}, {UID: 12}, {UID: 15}}
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
		got := resolveSeqSet(parseSet(t, c.set), c.uid, msgs)
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
	if !resolveSeqSet(parseSet(t, "*"), true, nil).Empty() {
		t.Error("empty mailbox resolves to an empty set")
	}
}

// bigFolder is a folder past the old 10 000-message cap, UIDs 2, 4, 6, ...
func bigFolder(n int) []db.FolderMessageRef {
	refs := make([]db.FolderMessageRef, n)
	for i := range refs {
		refs[i] = db.FolderMessageRef{ID: int64(1000 + i), UID: uint32(2 * (i + 1))}
	}
	return refs
}

func TestPickMessages(t *testing.T) {
	refs := bigFolder(25000)
	cases := []struct {
		name string
		set  string
		uid  bool
		want []uint32 // sequence numbers
	}{
		{"bare * past 10 000 is the last message", "*", false, []uint32{25000}},
		{"UID * is the highest UID", "*", true, []uint32{25000}},
		{"seq range across the old cap", "9999:10002", false, []uint32{9999, 10000, 10001, 10002}},
		{"UID range maps to sequence numbers", "20001:20006", true, []uint32{10001, 10002, 10003}},
		{"UID n:* past the end yields the last", "60000:*", true, []uint32{25000}},
		{"overlapping and unsorted ranges, no duplicates", "24999:*,3,1:2,2:4", false, []uint32{1, 2, 3, 4, 24999, 25000}},
		{"nested ranges", "5:20,7:8", false, []uint32{5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}},
		{"UIDs between messages select nothing", "3,5,7", true, nil},
		{"seq past the end selects nothing", "25001:25010", false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			picks := pickMessages(refs, c.uid, parseSet(t, c.set))
			if len(picks) != len(c.want) {
				t.Fatalf("picked %d messages, want %d", len(picks), len(c.want))
			}
			for i, p := range picks {
				if p.seqNum != c.want[i] {
					t.Fatalf("pick %d: seq %d, want %d", i, p.seqNum, c.want[i])
				}
				if p.ref != refs[p.seqNum-1] {
					t.Fatalf("pick %d: ref %+v does not belong to seq %d", i, p.ref, p.seqNum)
				}
			}
		})
	}
	if got := pickMessages(nil, false, parseSet(t, "1:*")); len(got) != 0 {
		t.Errorf("empty folder picked %d", len(got))
	}
}

func TestPickMessages_AllOfALargeFolder(t *testing.T) {
	refs := bigFolder(100000)
	picks := pickMessages(refs, true, parseSet(t, "1:*"))
	if len(picks) != len(refs) {
		t.Fatalf("UID FETCH 1:* picked %d of %d", len(picks), len(refs))
	}
	if last := picks[len(picks)-1]; last.seqNum != 100000 || last.ref.UID != 200000 {
		t.Fatalf("last pick = %+v", last)
	}
}
