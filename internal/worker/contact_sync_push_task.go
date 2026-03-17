package worker

import (
	"context"
	"fmt"
	"log"

	carddavclient "github.com/yourusername/mailserver/internal/carddav/client"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/task"
)

// ContactSyncPushTask pushes local contact changes back to the remote CardDAV server
type ContactSyncPushTask struct {
	source   *models.ContactSource
	database *db.DB
	priority int
}

// NewContactSyncPushTask creates a new contact sync push task
func NewContactSyncPushTask(source *models.ContactSource, database *db.DB) *ContactSyncPushTask {
	return &ContactSyncPushTask{
		source:   source,
		database: database,
		priority: 2,
	}
}

func (t *ContactSyncPushTask) Type() task.Type {
	return task.TypeIMAP // Uses same worker pool
}

func (t *ContactSyncPushTask) Priority() int {
	return t.priority
}

func (t *ContactSyncPushTask) String() string {
	return fmt.Sprintf("ContactSyncPush[%s]", t.source.Name)
}

func (t *ContactSyncPushTask) Execute(ctx context.Context) error {
	entries, err := t.database.GetPendingContactSync(t.source.ID, 50)
	if err != nil {
		return fmt.Errorf("failed to get pending contact sync: %w", err)
	}

	if len(entries) == 0 {
		return nil
	}

	log.Printf("Contact sync push: %d pending entries for %s", len(entries), t.source.Name)

	// Connect to remote CardDAV
	client := carddavclient.New(t.source, t.database)
	if err := client.Connect(); err != nil {
		return fmt.Errorf("failed to connect to CardDAV server: %w", err)
	}

	successCount := 0
	failCount := 0

	for _, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var err error
		switch entry.Operation {
		case "create", "update":
			remotePath := entry.RemoteID
			if remotePath == "" {
				// New contact - construct path from address book's remote ID and UID
				book, bookErr := t.database.GetAddressBookByID(entry.AddressBookID)
				if bookErr != nil {
					log.Printf("Contact sync push: failed to get address book %d: %v", entry.AddressBookID, bookErr)
					failCount++
					continue
				}
				remotePath = fmt.Sprintf("%s%s.vcf", book.RemoteID, entry.UID)
			}
			err = client.PutContactRaw(ctx, remotePath, entry.VCardData)
			if err == nil && entry.RemoteID == "" {
				// Update the contact's RemoteID in DB
				t.database.UpdateContactRemoteID(entry.ContactID, remotePath)
			}

		case "delete":
			if entry.RemoteID == "" {
				// No remote path - nothing to delete on remote
				log.Printf("Contact sync push: skip delete for %s (no remote ID)", entry.UID)
				t.database.DeleteContactSyncEntry(entry.ID)
				successCount++
				continue
			}
			err = client.DeleteContact(ctx, entry.RemoteID)
		}

		if err != nil {
			log.Printf("Contact sync push failed for %s (%s): %v", entry.UID, entry.Operation, err)
			failCount++
			continue
		}

		if err := t.database.DeleteContactSyncEntry(entry.ID); err != nil {
			log.Printf("Failed to delete contact sync entry %d: %v", entry.ID, err)
		}

		// Clear local_modified flag after successful push
		if entry.Operation != "delete" {
			t.database.MarkContactSynced(entry.ContactID, "")
		}

		successCount++
	}

	log.Printf("Contact sync push completed for %s: %d success, %d failed",
		t.source.Name, successCount, failCount)

	return nil
}
