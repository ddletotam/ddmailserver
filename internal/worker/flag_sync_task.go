package worker

import (
	"context"
	"fmt"
	"log"

	"github.com/yourusername/mailserver/internal/db"
	imapclient "github.com/yourusername/mailserver/internal/imap/client"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/task"
)

// FlagSyncTask synchronizes local flag changes back to the remote IMAP server
type FlagSyncTask struct {
	account  *models.Account
	database *db.DB
	priority int
}

// NewFlagSyncTask creates a new flag sync task for an account
func NewFlagSyncTask(account *models.Account, database *db.DB) *FlagSyncTask {
	return &FlagSyncTask{
		account:  account,
		database: database,
		priority: 2, // Higher priority than regular sync
	}
}

// Type returns the task type
func (t *FlagSyncTask) Type() task.Type {
	return task.TypeIMAP
}

// Priority returns task priority
func (t *FlagSyncTask) Priority() int {
	return t.priority
}

// String returns a human-readable description
func (t *FlagSyncTask) String() string {
	return fmt.Sprintf("Flag sync for %s (account %d)", t.account.Email, t.account.ID)
}

// Execute runs the flag synchronization
func (t *FlagSyncTask) Execute(ctx context.Context) error {
	// Get pending flag sync entries for this account
	entries, err := t.database.GetPendingFlagSync(t.account.ID, 50)
	if err != nil {
		return fmt.Errorf("failed to get pending flag sync: %w", err)
	}

	if len(entries) == 0 {
		return nil
	}

	log.Printf("Flag sync: %d pending entries for %s", len(entries), t.account.Email)

	// Create and connect IMAP client. A connect failure counts as a failed
	// attempt for every fetched entry — without the backoff bump the
	// scheduler would retry the full connect+LOGIN on its very next tick,
	// forever, while the source server is down or the password is stale.
	client, err := imapclient.New(t.account)
	if err != nil {
		t.markAllFailed(entries, err)
		return fmt.Errorf("failed to create IMAP client: %w", err)
	}

	if err := client.Connect(); err != nil {
		t.markAllFailed(entries, err)
		return fmt.Errorf("failed to connect to IMAP server: %w", err)
	}
	defer client.Disconnect()

	// Process each pending entry
	successCount := 0
	failCount := 0

	for _, entry := range entries {
		// Check context cancellation
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Delete + flag sync are mutually exclusive on the wire: a queued
		// "deleted" entry means we want the message gone from the source
		// server entirely (STORE +\Deleted + UID EXPUNGE). The seen /
		// flagged / answered bits on a deleted row are irrelevant — the
		// message ceases to exist on the remote — so we don't waste a
		// STORE round-trip on them.
		var err error
		if entry.Deleted {
			err = client.DeleteMessageRemote(entry.RemoteFolder, entry.RemoteUID)
		} else {
			err = client.StoreFlags(
				entry.RemoteFolder,
				entry.RemoteUID,
				entry.Seen,
				entry.Flagged,
				entry.Answered,
				false,
			)
		}

		if err != nil {
			log.Printf("Flag sync failed for message %d (remote UID %d, deleted=%v, retry %d): %v",
				entry.MessageID, entry.RemoteUID, entry.Deleted, entry.RetryCount, err)
			// Persist failure for backoff: without it the entry is retried
			// on every scheduler tick. Best-effort — a bookkeeping error
			// must not abort the remaining entries.
			if markErr := t.database.MarkFlagSyncFailed(entry.ID, err.Error()); markErr != nil {
				log.Printf("Failed to mark flag sync failure for entry %d: %v", entry.ID, markErr)
			}
			failCount++
			// Continue with other entries, don't abort
			continue
		}

		// Delete successful entry from queue
		if err := t.database.DeleteFlagSyncEntry(entry.ID); err != nil {
			log.Printf("Failed to delete flag sync entry %d: %v", entry.ID, err)
		}

		successCount++
	}

	log.Printf("Flag sync completed for %s: %d success, %d failed",
		t.account.Email, successCount, failCount)

	return nil
}

// markAllFailed records a shared failure (e.g. connect refused) against every
// entry so each one's backoff advances and the account stops being picked up
// by the scheduler until next_attempt_at.
func (t *FlagSyncTask) markAllFailed(entries []*models.FlagSyncEntry, cause error) {
	for _, entry := range entries {
		if err := t.database.MarkFlagSyncFailed(entry.ID, cause.Error()); err != nil {
			log.Printf("Failed to mark flag sync failure for entry %d: %v", entry.ID, err)
		}
	}
}
