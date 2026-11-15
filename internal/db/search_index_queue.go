package db

import (
	"fmt"

	"github.com/lib/pq"
)

// SearchIndexEntry is one search_index_queue row (migration 060): the message
// row id whose search document is stale, and the seq of the change that made
// it stale.
type SearchIndexEntry struct {
	MessageRowID int64
	Seq          int64
}

// PendingSearchIndex returns up to limit queued entries, oldest change first.
func (db *DB) PendingSearchIndex(limit int) ([]SearchIndexEntry, error) {
	rows, err := db.Query(
		`SELECT message_row_id, seq FROM search_index_queue ORDER BY seq LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("read search index queue: %w", err)
	}
	defer rows.Close()

	var entries []SearchIndexEntry
	for rows.Next() {
		var e SearchIndexEntry
		if err := rows.Scan(&e.MessageRowID, &e.Seq); err != nil {
			return nil, fmt.Errorf("scan search index queue: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// AckSearchIndex removes entries the worker has pushed to the index. An entry
// is removed only if its seq is unchanged: a message changed again meanwhile
// keeps its (newer) entry and is indexed on the next pass.
func (db *DB) AckSearchIndex(entries []SearchIndexEntry) error {
	if len(entries) == 0 {
		return nil
	}
	ids := make([]int64, len(entries))
	seqs := make([]int64, len(entries))
	for i, e := range entries {
		ids[i], seqs[i] = e.MessageRowID, e.Seq
	}
	_, err := db.Exec(
		`DELETE FROM search_index_queue q
		  USING unnest($1::bigint[], $2::bigint[]) AS a(id, seq)
		  WHERE q.message_row_id = a.id AND q.seq = a.seq`,
		pq.Array(ids), pq.Array(seqs))
	if err != nil {
		return fmt.Errorf("ack search index queue: %w", err)
	}
	return nil
}

// ClearSearchIndexQueue empties the queue. Used when no search index is
// configured, so the queue does not keep every change ever made.
func (db *DB) ClearSearchIndexQueue() error {
	if _, err := db.Exec(`DELETE FROM search_index_queue`); err != nil {
		return fmt.Errorf("clear search index queue: %w", err)
	}
	return nil
}

// MessageRowsExist returns the subset of ids that have a messages row.
func (db *DB) MessageRowsExist(ids []int64) (map[int64]bool, error) {
	found := make(map[int64]bool, len(ids))
	if len(ids) == 0 {
		return found, nil
	}
	rows, err := db.Query(`SELECT id FROM messages WHERE id = ANY($1)`, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("check message rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan message id: %w", err)
		}
		found[id] = true
	}
	return found, rows.Err()
}
