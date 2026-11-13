package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/ddletotam/ddmailserver/internal/authfail"
	carddavclient "github.com/ddletotam/ddmailserver/internal/carddav/client"
	"github.com/ddletotam/ddmailserver/internal/carddav/vcardedit"
	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/task"
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

	guard := authGuard(t.database)
	sub := authfail.ContactSource(t.source)
	if !guard.Allow(sub) {
		return nil // credentials rejected, pause not over (auth_backoff)
	}

	log.Printf("Contact sync push: %d pending entries for %s", len(entries), t.source.Name)

	// Connect to remote CardDAV
	client := carddavclient.New(t.source, t.database)
	if err := client.Connect(); err != nil {
		return fmt.Errorf("failed to connect to CardDAV server: %w", err)
	}

	successCount := 0
	failCount := 0
	var authErr error
	defer func() { reportDAVAuth(guard, sub, client.AuthWatch(), authErr) }()

	for _, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if authErr != nil {
			break
		}

		var err error
		switch {
		case entry.Operation == "update" && entry.Patch != "" && entry.RemoteID != "":
			var done bool
			done, err = t.pushPatch(ctx, client, entry)
			if done {
				successCount++
				continue
			}
		case entry.Operation == "create" || entry.Operation == "update":
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

		case entry.Operation == "delete":
			if entry.RemoteID == "" {
				// No remote path - nothing to delete on remote
				log.Printf("Contact sync push: skip delete for %s (no remote ID)", entry.UID)
				t.database.DeleteContactSyncEntry(entry.ID)
				successCount++
				continue
			}
			err = client.DeleteContact(ctx, entry.RemoteID)
		}

		if err != nil && (authfail.Is(err) || client.AuthWatch().Rejected()) {
			// Password rejected: stop the round instead of collecting one
			// failed login per queued contact.
			log.Printf("Contact sync push for %s stopped: credentials rejected (%v)", t.source.Name, err)
			authErr = err
			continue
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

// pushPatch отправляет правку по полям: свежая карточка с сервера, правка по
// строкам (vcardedit), PUT с If-Match. Всё, чего правка не касается (фото,
// дни рождения, адреса, X-свойства, чужие правки в iCloud), остаётся на
// сервере как было.
//
// done=true — запись очереди обработана (и удалена) здесь, считать её
// успехом; иначе err разбирает общий цикл.
func (t *ContactSyncPushTask) pushPatch(ctx context.Context, client *carddavclient.Client, entry *models.ContactSyncEntry) (bool, error) {
	var patch vcardedit.Patch
	if err := json.Unmarshal([]byte(entry.Patch), &patch); err != nil {
		// Битая правка не станет целой при повторе — убираем её, синк вернёт
		// карточку с сервера.
		log.Printf("Contact sync push: bad patch for %s, dropped: %v", entry.UID, err)
		t.finish(entry)
		return true, nil
	}
	_, err := client.PatchContact(ctx, entry.RemoteID, func(current string) (string, error) {
		res, err := vcardedit.Apply(current, patch, time.Now())
		return res.Data, err
	})
	switch {
	case errors.Is(err, carddavclient.ErrRemoteGone):
		// Карточку удалили на сервере — править нечего. Следующий синк уберёт
		// и локальную копию.
		log.Printf("Contact sync push: %s is gone on the server, patch dropped", entry.UID)
		t.finish(entry)
		return true, nil
	case errors.Is(err, carddavclient.ErrForbidden):
		// Сервер правку не примет и при повторе: книга только для чтения.
		// Снимаем локальную правку — синк вернёт карточку с сервера, а не
		// оставит навсегда разошедшуюся копию.
		log.Printf("Contact sync push: server refused %s (read-only?), local edit dropped: %v", entry.UID, err)
		t.finish(entry)
		return true, nil
	case err != nil:
		return false, err
	}
	t.finish(entry)
	return true, nil
}

// finish убирает запись из очереди и снимает local_modified с пустым ETag:
// следующий синк перечитает карточку с сервера (такой, какой её сохранил
// сервер), а не будет держать нашу версию.
func (t *ContactSyncPushTask) finish(entry *models.ContactSyncEntry) {
	if err := t.database.DeleteContactSyncEntry(entry.ID); err != nil {
		log.Printf("Failed to delete contact sync entry %d: %v", entry.ID, err)
	}
	if err := t.database.MarkContactSynced(entry.ContactID, ""); err != nil {
		log.Printf("Failed to mark contact %d synced: %v", entry.ContactID, err)
	}
}
