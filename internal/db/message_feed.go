package db

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

// Feed version of messages (migration 064, internal/feedtrim).

// FeedMessage is what the feed computation reads of a message.
type FeedMessage struct {
	ID          int64
	UserID      int64
	MessageID   string
	InReplyTo   string
	References  string
	From        string
	Subject     string
	Date        int64 // unix millis
	Body        string
	BodyHTML    string
	Unavailable bool // spam, soft-deleted, in Trash or Junk: not part of the feed
}

// MessageFeed is a computed row of message_feed.
type MessageFeed struct {
	MessageRowID int64
	UserID       int64
	Sender       string
	Subject      string
	Version      int
	HistoryCut   bool
	SignatureCut bool
	FeedHTML     *string
	FeedText     *string
	OwnTail      string
	ComputedAt   int64
}

// FeedPeer is another message of a sender: its subject and own lines.
type FeedPeer struct {
	Subject string
	OwnTail string
}

const feedMessageCols = `m.id, m.user_id, COALESCE(m.message_id, ''), COALESCE(m.in_reply_to, ''),
	COALESCE(m.message_references, ''), COALESCE(m.from_addr, ''), COALESCE(m.subject, ''),
	COALESCE(m.date, 0), COALESCE(m.body, ''), COALESCE(m.body_html, ''),
	(COALESCE(m.is_spam, FALSE) OR COALESCE(m.soft_deleted, FALSE) OR f.type IN ('trash', 'junk', 'spam'))`

func scanFeedMessage(row interface{ Scan(...any) error }) (*FeedMessage, error) {
	m := &FeedMessage{}
	err := row.Scan(&m.ID, &m.UserID, &m.MessageID, &m.InReplyTo, &m.References, &m.From,
		&m.Subject, &m.Date, &m.Body, &m.BodyHTML, &m.Unavailable)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// FeedMessageByRowID returns the message, or nil if the row is gone.
func (db *DB) FeedMessageByRowID(id int64) (*FeedMessage, error) {
	m, err := scanFeedMessage(db.QueryRow(
		`SELECT `+feedMessageCols+` FROM messages m JOIN folders f ON f.id = m.folder_id WHERE m.id = $1`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read message %d for feed: %w", id, err)
	}
	return m, nil
}

// bareMessageID strips the angle brackets: Message-IDs of the user's own sent
// copies are stored without them, received ones with them.
func bareMessageID(id string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(id), "<>"))
}

// FeedAncestor returns the user's message with the given Message-ID that is
// part of the feed, or nil.
func (db *DB) FeedAncestor(userID int64, messageID string) (*FeedMessage, error) {
	bare := bareMessageID(messageID)
	if bare == "" {
		return nil, nil
	}
	m, err := scanFeedMessage(db.QueryRow(
		`SELECT `+feedMessageCols+` FROM messages m JOIN folders f ON f.id = m.folder_id
		  WHERE m.user_id = $1 AND m.message_id IN ($2::text, '<' || $2::text || '>')
		  ORDER BY m.id LIMIT 1`, userID, bare))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read ancestor %q: %w", bare, err)
	}
	if m.Unavailable {
		return nil, nil
	}
	return m, nil
}

// FeedPeers returns up to limit other computed messages of the sender, newest first.
func (db *DB) FeedPeers(userID int64, sender string, exclude int64, limit int) ([]FeedPeer, error) {
	rows, err := db.Query(
		`SELECT subject, own_tail FROM message_feed
		  WHERE user_id = $1 AND sender = $2 AND message_row_id <> $3
		  ORDER BY message_row_id DESC LIMIT $4`, userID, sender, exclude, limit)
	if err != nil {
		return nil, fmt.Errorf("read feed peers: %w", err)
	}
	defer rows.Close()
	var out []FeedPeer
	for rows.Next() {
		var p FeedPeer
		if err := rows.Scan(&p.Subject, &p.OwnTail); err != nil {
			return nil, fmt.Errorf("scan feed peer: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// HasMessageFeed reports whether the message was computed before (any version).
func (db *DB) HasMessageFeed(id int64) (bool, error) {
	var ok bool
	err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM message_feed WHERE message_row_id = $1)`, id).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check message feed %d: %w", id, err)
	}
	return ok, nil
}

// SaveMessageFeed inserts or replaces the row.
func (db *DB) SaveMessageFeed(f *MessageFeed) error {
	_, err := db.Exec(
		`INSERT INTO message_feed (message_row_id, user_id, sender, subject, version, history_cut,
		        signature_cut, feed_html, feed_text, own_tail, computed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 ON CONFLICT (message_row_id) DO UPDATE SET
		        user_id = EXCLUDED.user_id, sender = EXCLUDED.sender, subject = EXCLUDED.subject,
		        version = EXCLUDED.version, history_cut = EXCLUDED.history_cut,
		        signature_cut = EXCLUDED.signature_cut, feed_html = EXCLUDED.feed_html,
		        feed_text = EXCLUDED.feed_text, own_tail = EXCLUDED.own_tail,
		        computed_at = EXCLUDED.computed_at`,
		f.MessageRowID, f.UserID, f.Sender, f.Subject, f.Version, f.HistoryCut, f.SignatureCut,
		f.FeedHTML, f.FeedText, f.OwnTail, f.ComputedAt)
	if err != nil {
		return fmt.Errorf("save message feed %d: %w", f.MessageRowID, err)
	}
	return nil
}

// DeleteMessageFeed removes the row (the message left the feed: spam, Trash).
func (db *DB) DeleteMessageFeed(id int64) error {
	if _, err := db.Exec(`DELETE FROM message_feed WHERE message_row_id = $1`, id); err != nil {
		return fmt.Errorf("delete message feed %d: %w", id, err)
	}
	return nil
}

// EnqueueStaleMessageFeed queues messages never computed or computed by an
// older version. Returns how many were queued.
func (db *DB) EnqueueStaleMessageFeed(version int) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO message_feed_queue (message_row_id, enqueued_at)
		 SELECT m.id, (EXTRACT(EPOCH FROM now()) * 1000)::BIGINT
		   FROM messages m LEFT JOIN message_feed f ON f.message_row_id = m.id
		  WHERE f.message_row_id IS NULL OR f.version < $1
		 ON CONFLICT (message_row_id) DO NOTHING`, version)
	if err != nil {
		return 0, fmt.Errorf("enqueue stale message feed: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// EnqueueFeedReplies queues the user's messages that reply to messageID.
func (db *DB) EnqueueFeedReplies(userID int64, messageID string, exclude int64) error {
	bare := bareMessageID(messageID)
	if bare == "" {
		return nil
	}
	_, err := db.Exec(
		`INSERT INTO message_feed_queue (message_row_id, enqueued_at)
		 SELECT id, (EXTRACT(EPOCH FROM now()) * 1000)::BIGINT FROM messages
		  WHERE user_id = $1 AND in_reply_to IN ($2::text, '<' || $2::text || '>') AND id <> $3
		 ON CONFLICT (message_row_id) DO NOTHING`, userID, bare, exclude)
	if err != nil {
		return fmt.Errorf("enqueue replies of %q: %w", bare, err)
	}
	return nil
}

// PendingMessageFeed returns up to limit queued ids, oldest message first:
// earlier mail of a sender is computed before later mail compares with it.
func (db *DB) PendingMessageFeed(limit int) ([]int64, error) {
	rows, err := db.Query(`SELECT message_row_id FROM message_feed_queue ORDER BY message_row_id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("read message feed queue: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan message feed queue: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AckMessageFeed removes computed ids from the queue.
func (db *DB) AckMessageFeed(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := db.Exec(`DELETE FROM message_feed_queue WHERE message_row_id = ANY($1)`, pq.Array(ids)); err != nil {
		return fmt.Errorf("ack message feed queue: %w", err)
	}
	return nil
}

// MessageFeeds returns the rows with a cut for the given message ids, by id.
func (db *DB) MessageFeeds(ids []int64) (map[int64]*MessageFeed, error) {
	out := make(map[int64]*MessageFeed, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.Query(
		`SELECT message_row_id, history_cut, signature_cut, feed_html, feed_text FROM message_feed
		  WHERE message_row_id = ANY($1) AND (history_cut OR signature_cut)`, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("read message feeds: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		f := &MessageFeed{}
		if err := rows.Scan(&f.MessageRowID, &f.HistoryCut, &f.SignatureCut, &f.FeedHTML, &f.FeedText); err != nil {
			return nil, fmt.Errorf("scan message feed: %w", err)
		}
		out[f.MessageRowID] = f
	}
	return out, rows.Err()
}
