// Command feedtrim-dryrun computes feed versions for one user against a live
// database without writing anything: message_feed lives in memory. Prints
// counts only; -show id,id prints the feed text of chosen messages.
package main

import (
	"flag"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/feedtrim"
)

type memStore struct {
	*db.DB
	feed  map[int64]*db.MessageFeed
	queue map[int64]bool
}

func (m *memStore) FeedPeers(userID int64, sender string, exclude int64, limit int) ([]db.FeedPeer, error) {
	var ids []int64
	for id, f := range m.feed {
		if f.UserID == userID && f.Sender == sender && id != exclude {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	var out []db.FeedPeer
	for _, id := range ids {
		if len(out) == limit {
			break
		}
		out = append(out, db.FeedPeer{Subject: m.feed[id].Subject, OwnTail: m.feed[id].OwnTail})
	}
	return out, nil
}
func (m *memStore) MessageFeedCurrent(ids []int64, version int) (map[int64]bool, error) {
	out := map[int64]bool{}
	for _, id := range ids {
		if f := m.feed[id]; f != nil && f.Version >= version {
			out[id] = true
		}
	}
	return out, nil
}
func (m *memStore) HasMessageFeed(id int64) (bool, error)      { _, ok := m.feed[id]; return ok, nil }
func (m *memStore) SaveMessageFeed(f *db.MessageFeed) error    { m.feed[f.MessageRowID] = f; return nil }
func (m *memStore) DeleteMessageFeed(id int64) error           { delete(m.feed, id); return nil }
func (m *memStore) EnqueueStaleMessageFeed(int) (int64, error) { return 0, nil }
func (m *memStore) EnqueueFeedReplies(userID int64, mid string, exclude int64) error {
	bare := strings.Trim(mid, "<>")
	rows, err := m.Query(`SELECT id FROM messages WHERE user_id=$1 AND in_reply_to IN ($2::text,'<'||$2::text||'>') AND id<>$3`, userID, bare, exclude)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		m.queue[id] = true
	}
	return rows.Err()
}
func (m *memStore) PendingMessageFeed(limit int) ([]int64, error) {
	var ids []int64
	for id := range m.queue {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}
func (m *memStore) AckMessageFeed(ids []int64) error {
	for _, id := range ids {
		delete(m.queue, id)
	}
	return nil
}

func main() {
	dsn := flag.String("dsn", "host=/var/run/postgresql dbname=mailserver sslmode=disable", "")
	user := flag.Int64("user", 0, "")
	show := flag.String("show", "", "")
	flag.Parse()
	d, err := db.Connect(*dsn)
	if err != nil {
		log.Fatal(err)
	}
	st := &memStore{DB: d, feed: map[int64]*db.MessageFeed{}, queue: map[int64]bool{}}
	rows, err := d.Query(`SELECT id FROM messages WHERE user_id=$1`, *user)
	if err != nil {
		log.Fatal(err)
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		st.queue[id] = true
	}
	rows.Close()
	total := len(st.queue)
	w := feedtrim.NewWorker(st)
	for len(st.queue) > 0 {
		if _, err := w.Drain(500); err != nil {
			log.Fatal(err)
		}
	}
	var hist, sig, both, html, text int
	for _, f := range st.feed {
		if f.HistoryCut {
			hist++
		}
		if f.SignatureCut {
			sig++
		}
		if f.HistoryCut && f.SignatureCut {
			both++
		}
		if f.FeedHTML != nil {
			html++
		}
		if f.FeedText != nil {
			text++
		}
	}
	fmt.Printf("messages %d, computed %d (rest: spam/trash)\nhistory cut %d, signature cut %d, both %d\nfeed_html %d, feed_text %d\n",
		total, len(st.feed), hist, sig, both, html, text)
	for _, s := range strings.Split(*show, ",") {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			continue
		}
		f := st.feed[id]
		if f == nil {
			fmt.Printf("\n### %d: no row\n", id)
			continue
		}
		fmt.Printf("\n### %d history=%v signature=%v\n", id, f.HistoryCut, f.SignatureCut)
		if f.FeedHTML != nil {
			fmt.Println(*f.FeedHTML)
		}
	}
}
