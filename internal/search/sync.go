package search

import (
	"context"
	"fmt"
	"log"
	"time"
)

// Background sync: the database queues every message change that affects its
// search document (search_index_queue, filled by a trigger — migration 060),
// and Run drains that queue into Meilisearch. Write paths — MX and local
// delivery, the Sent copy, IMAP APPEND, upstream sync, import, moves and
// deletes — never call the index themselves and never wait for it; when
// Meilisearch is down the queue just waits and is drained once it is back.
const (
	syncBatchSize     = 200
	syncPollInterval  = 2 * time.Second
	syncMaxBackoff    = time.Minute
	reconcilePageSize = 1000
)

// Run brings the index up to date and keeps it there until ctx ends:
// waits for Meilisearch, rebuilds an empty index from scratch, drops documents
// of messages that no longer exist, then drains the change queue.
func (idx *Indexer) Run(ctx context.Context) {
	bo := backoff{}

	for {
		err := idx.Initialize()
		if err == nil {
			break
		}
		log.Printf("Search: Meilisearch not ready: %v", err)
		if !sleepCtx(ctx, bo.next()) {
			return
		}
	}
	bo.reset()

	for {
		err := idx.rebuildIfEmpty()
		if err == nil {
			break
		}
		log.Printf("Search: full reindex failed: %v", err)
		if !sleepCtx(ctx, bo.next()) {
			return
		}
	}
	bo.reset()

	if removed, err := idx.Reconcile(); err != nil {
		log.Printf("Search: reconcile failed (stale documents stay until next start): %v", err)
	} else if removed > 0 {
		log.Printf("Search: removed %d documents of deleted messages", removed)
	}

	for {
		n, err := idx.SyncPending(syncBatchSize)
		if err != nil {
			log.Printf("Search: sync failed, will retry: %v", err)
			if !sleepCtx(ctx, bo.next()) {
				return
			}
			continue
		}
		bo.reset()
		if n == syncBatchSize {
			if ctx.Err() != nil {
				return
			}
			continue // more waiting
		}
		if !sleepCtx(ctx, idx.poll()) {
			return
		}
	}
}

func (idx *Indexer) poll() time.Duration {
	if idx.pollInterval > 0 {
		return idx.pollInterval
	}
	return syncPollInterval
}

// SyncPending pushes up to limit queued changes to the index and returns how
// many it handled. A message that still exists is (re)indexed from its current
// row; one that is gone has its document deleted. Entries are removed from the
// queue only after Meilisearch accepted the update.
func (idx *Indexer) SyncPending(limit int) (int, error) {
	entries, err := idx.database.PendingSearchIndex(limit)
	if err != nil {
		return 0, err
	}
	if len(entries) == 0 {
		return 0, nil
	}

	ids := make([]int64, len(entries))
	for i, e := range entries {
		ids[i] = e.MessageRowID
	}
	messages, err := idx.database.GetMessagesForIndexingByIDs(ids)
	if err != nil {
		return 0, err
	}

	present := make(map[int64]bool, len(messages))
	docs := make([]IndexDocument, 0, len(messages))
	for _, msg := range messages {
		present[msg.ID] = true
		docs = append(docs, messageToDocument(msg))
	}
	var gone []int64
	for _, id := range ids {
		if !present[id] {
			gone = append(gone, id)
		}
	}

	if err := idx.client.IndexDocuments(docs); err != nil {
		return 0, fmt.Errorf("index %d documents: %w", len(docs), err)
	}
	if err := idx.client.DeleteDocuments(gone); err != nil {
		return 0, fmt.Errorf("delete %d documents: %w", len(gone), err)
	}
	if err := idx.database.AckSearchIndex(entries); err != nil {
		return 0, err
	}
	return len(entries), nil
}

// rebuildIfEmpty runs the full reindex when the index holds no documents
// (first start, or Meilisearch data lost). Otherwise the queue keeps the index
// current and a full pass on every start is wasted work.
func (idx *Indexer) rebuildIfEmpty() error {
	ids, total, err := idx.client.ListDocumentIDs(0, 1)
	if err != nil {
		return err
	}
	if total > 0 || len(ids) > 0 {
		return nil
	}
	return idx.IndexAllMessages()
}

// Reconcile deletes documents whose message row no longer exists — left
// behind by deletes made before the change queue existed. Returns how many
// it removed.
func (idx *Indexer) Reconcile() (int, error) {
	var gone []int64
	for offset := 0; ; offset += reconcilePageSize {
		ids, _, err := idx.client.ListDocumentIDs(offset, reconcilePageSize)
		if err != nil {
			return 0, err
		}
		if len(ids) == 0 {
			break
		}
		exists, err := idx.database.MessageRowsExist(ids)
		if err != nil {
			return 0, err
		}
		for _, id := range ids {
			if !exists[id] {
				gone = append(gone, id)
			}
		}
		if len(ids) < reconcilePageSize {
			break
		}
	}
	// Delete after the walk: deleting while paging by offset would shift
	// the pages and skip documents.
	for start := 0; start < len(gone); start += reconcilePageSize {
		end := min(start+reconcilePageSize, len(gone))
		if err := idx.client.DeleteDocuments(gone[start:end]); err != nil {
			return start, err
		}
	}
	return len(gone), nil
}

// backoff doubles from one second up to syncMaxBackoff.
type backoff struct{ d time.Duration }

func (b *backoff) next() time.Duration {
	if b.d == 0 {
		b.d = time.Second
	} else {
		b.d = min(b.d*2, syncMaxBackoff)
	}
	return b.d
}

func (b *backoff) reset() { b.d = 0 }

// sleepCtx waits d or until ctx ends; false means ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
