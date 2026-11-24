package feedtrim

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
)

// Store is the part of the database the worker uses.
type Store interface {
	FeedMessageByRowID(id int64) (*db.FeedMessage, error)
	FeedAncestor(userID int64, messageID string) (*db.FeedMessage, error)
	FeedPeers(userID int64, sender string, exclude int64, limit int) ([]db.FeedPeer, error)
	HasMessageFeed(id int64) (bool, error)
	SaveMessageFeed(f *db.MessageFeed) error
	DeleteMessageFeed(id int64) error
	EnqueueStaleMessageFeed(version int) (int64, error)
	EnqueueFeedReplies(userID int64, messageID string, exclude int64) error
	PendingMessageFeed(limit int) ([]int64, error)
	AckMessageFeed(ids []int64) error
}

const (
	batchSize    = 100
	pollInterval = 2 * time.Second
	maxBackoff   = time.Minute
	maxChain     = 60 // In-Reply-To hops followed
	peerLimit    = 30 // sender's messages compared for the signature
)

// Worker computes feed versions of messages queued in message_feed_queue.
type Worker struct {
	store Store
}

// NewWorker returns a worker over the store.
func NewWorker(store Store) *Worker {
	return &Worker{store: store}
}

// Run queues what an older version computed (or nothing computed yet), then
// drains the queue until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	if n, err := w.store.EnqueueStaleMessageFeed(Version); err != nil {
		log.Printf("Feed: queue stale messages: %v", err)
	} else if n > 0 {
		log.Printf("Feed: %d messages queued for computation (version %d)", n, Version)
	}
	wait := pollInterval
	for {
		n, err := w.Drain(batchSize)
		switch {
		case err != nil:
			log.Printf("Feed: %v", err)
			wait = min(wait*2, maxBackoff)
		case n > 0:
			wait = pollInterval
			continue
		default:
			wait = pollInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Drain computes one batch from the queue and returns its size.
func (w *Worker) Drain(limit int) (int, error) {
	ids, err := w.store.PendingMessageFeed(limit)
	if err != nil {
		return 0, err
	}
	for i, id := range ids {
		// A database error leaves this and the rest queued for a retry.
		if err := w.compute(id); err != nil {
			return i, err
		}
		// One by one, not per batch: computing a message may queue again a
		// reply computed earlier in this batch, and a batch ack would drop it.
		if err := w.store.AckMessageFeed([]int64{id}); err != nil {
			return i, err
		}
	}
	return len(ids), nil
}

var addrRe2 = regexp.MustCompile(`[\w.+-]+@[\w.-]+`)

func senderOf(from string) string {
	low := strings.ToLower(from)
	if m := addrRe2.FindString(low); m != "" {
		return m
	}
	return strings.TrimSpace(low)
}

func parseBody(m *db.FeedMessage) *Body {
	if strings.TrimSpace(m.BodyHTML) != "" {
		if b := ParseHTML(m.BodyHTML); b != nil {
			return b
		}
	}
	if strings.TrimSpace(m.Body) != "" {
		return ParseText(m.Body)
	}
	return nil
}

var msgIDRe = regexp.MustCompile(`<[^<>\s]+>|[^<>\s,]+@[^<>\s,]+`)

func (w *Worker) ancestors(m *db.FeedMessage) ([]Ancestor, error) {
	seen := map[string]bool{}
	var out []Ancestor
	add := func(a *db.FeedMessage) {
		out = append(out, Ancestor{From: a.From, Subject: a.Subject, Date: time.UnixMilli(a.Date), Body: parseBody(a)})
	}
	cur := m
	for range maxChain {
		key := strings.Trim(strings.TrimSpace(cur.InReplyTo), "<>")
		if key == "" || seen[key] {
			break
		}
		seen[key] = true
		a, err := w.store.FeedAncestor(m.UserID, key)
		if err != nil {
			return nil, err
		}
		if a == nil || a.ID == m.ID {
			break
		}
		add(a)
		cur = a
	}
	for _, ref := range msgIDRe.FindAllString(m.References, -1) {
		key := strings.Trim(ref, "<>")
		if seen[key] {
			continue
		}
		seen[key] = true
		a, err := w.store.FeedAncestor(m.UserID, key)
		if err != nil {
			return nil, err
		}
		if a != nil && a.ID != m.ID {
			add(a)
		}
	}
	return out, nil
}

func (w *Worker) compute(id int64) (err error) {
	m, err := w.store.FeedMessageByRowID(id)
	if err != nil || m == nil {
		return err
	}
	if m.Unavailable {
		return w.store.DeleteMessageFeed(id)
	}
	first, err := w.store.HasMessageFeed(id)
	if err != nil {
		return err
	}
	first = !first

	anc, err := w.ancestors(m)
	if err != nil {
		return err
	}
	sender := senderOf(m.From)
	ps, err := w.store.FeedPeers(m.UserID, sender, id, peerLimit)
	if err != nil {
		return err
	}
	peers := make([]Peer, 0, len(ps))
	for _, p := range ps {
		if p.OwnTail != "" {
			peers = append(peers, Peer{Subject: p.Subject, Own: strings.Split(p.OwnTail, "\n")})
		}
	}

	row := &db.MessageFeed{
		MessageRowID: id, UserID: m.UserID, Sender: sender, Subject: m.Subject,
		Version: Version, ComputedAt: time.Now().UnixMilli(),
	}
	// A malformed body must not stall the queue: the message just keeps its
	// original form in the feed.
	defer func() {
		if p := recover(); p != nil {
			log.Printf("Feed: message %d: %v — left as is", id, p)
			row.HistoryCut, row.SignatureCut, row.FeedHTML, row.FeedText = false, false, nil, nil
			err = w.store.SaveMessageFeed(row)
		}
	}()

	var primary Result
	if strings.TrimSpace(m.BodyHTML) != "" {
		if b := ParseHTML(m.BodyHTML); b != nil {
			primary = Trim(b, m.Subject, anc, peers)
			row.OwnTail = strings.Join(primary.Own, "\n")
			if c := primary.Cut(); c >= 0 {
				s := b.CutAt(c)
				row.FeedHTML = &s
				row.HistoryCut = primary.History >= 0
				row.SignatureCut = primary.Signature >= 0
			}
		}
	}
	if strings.TrimSpace(m.Body) != "" {
		if b := ParseText(m.Body); b != nil {
			r := Trim(b, m.Subject, anc, peers)
			if row.OwnTail == "" {
				row.OwnTail = strings.Join(r.Own, "\n")
			}
			if c := r.Cut(); c >= 0 {
				s := b.CutAt(c)
				row.FeedText = &s
				row.HistoryCut = row.HistoryCut || r.History >= 0
				row.SignatureCut = row.SignatureCut || r.Signature >= 0
			}
		}
	}
	if err := w.store.SaveMessageFeed(row); err != nil {
		return err
	}
	if first && m.MessageID != "" {
		// Replies computed before this message arrived could not see its text.
		if err := w.store.EnqueueFeedReplies(m.UserID, m.MessageID, id); err != nil {
			return fmt.Errorf("message %d: %w", id, err)
		}
	}
	return nil
}
