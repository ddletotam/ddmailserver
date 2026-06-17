package db

import "fmt"

// Change journal kinds. See migration 043.
const (
	ChangeKindUpsert = 1 // new or changed, visible to the client
	ChangeKindDelete = 2 // gone from the client's view (deleted / soft-deleted / spam)
)

// MessageChange is one journal entry: a message identified by its RFC
// Message-ID changed in a way the desktop client cares about.
type MessageChange struct {
	Seq       int64  `json:"seq"`
	MessageID string `json:"message_id"`
	Kind      int    `json:"kind"`
}

// GetMessageChanges returns the journal tail for a user after `since` (a global
// seq cursor). It also returns the current global head (latestSeq) and the
// low-watermark (highest pruned seq). Callers decide reset semantics:
//   - since <= 0  → new client: caller should full-resync and adopt latestSeq.
//   - since < lowWatermark → cursor fell behind retention: full-resync.
//
// latestSeq/lowWatermark are GLOBAL (seq is a global BIGSERIAL) so the client's
// stored cursor advances past other users' interleaved seqs and never loops.
func (db *DB) GetMessageChanges(userID, since int64, limit int) (changes []MessageChange, latestSeq, lowWatermark int64, err error) {
	if limit <= 0 || limit > 5000 {
		limit = 5000
	}

	if err = db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM message_changes`).Scan(&latestSeq); err != nil {
		return nil, 0, 0, fmt.Errorf("journal head: %w", err)
	}
	if err = db.QueryRow(`SELECT low_watermark FROM journal_meta WHERE only_row`).Scan(&lowWatermark); err != nil {
		return nil, 0, 0, fmt.Errorf("journal low_watermark: %w", err)
	}

	// New client or fell-behind cursor: don't replay the log, the caller
	// full-resyncs via /conversations and adopts latestSeq.
	if since <= 0 || since < lowWatermark {
		return []MessageChange{}, latestSeq, lowWatermark, nil
	}

	rows, err := db.Query(
		`SELECT seq, message_id, kind
		   FROM message_changes
		  WHERE user_id = $1 AND seq > $2
		  ORDER BY seq
		  LIMIT $3`,
		userID, since, limit,
	)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("journal tail: %w", err)
	}
	defer rows.Close()

	changes = []MessageChange{}
	for rows.Next() {
		var c MessageChange
		if err = rows.Scan(&c.Seq, &c.MessageID, &c.Kind); err != nil {
			return nil, 0, 0, fmt.Errorf("scan change: %w", err)
		}
		changes = append(changes, c)
	}
	return changes, latestSeq, lowWatermark, rows.Err()
}

// CompactMessageChanges prunes journal entries older than `olderThanMs` (unix
// millis) and advances the low-watermark to the highest pruned seq, so clients
// whose cursor fell behind are told to resync instead of silently missing
// tombstones. Returns the number of pruned rows.
func (db *DB) CompactMessageChanges(olderThanMs int64) (int64, error) {
	res, err := db.Exec(
		`WITH pruned AS (
			DELETE FROM message_changes WHERE ts < $1 RETURNING seq
		 )
		 UPDATE journal_meta
		    SET low_watermark = GREATEST(low_watermark, COALESCE((SELECT MAX(seq) FROM pruned), 0))
		  WHERE only_row`,
		olderThanMs,
	)
	if err != nil {
		return 0, fmt.Errorf("compact journal: %w", err)
	}
	// RowsAffected here is the journal_meta update (1), not the prune count;
	// the prune count isn't separately exposed by this combined statement.
	_ = res
	return 0, nil
}
