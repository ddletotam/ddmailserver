package db

import (
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// QueueContactSync adds or updates a contact sync entry
func (db *DB) QueueContactSync(contactID, addressBookID, sourceID int64, uid, remoteID, vcardData, operation string) error {
	query := `
		INSERT INTO contact_sync_queue (contact_id, address_book_id, source_id, uid, remote_id, vcard_data, operation, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (contact_id) DO UPDATE SET
			vcard_data = EXCLUDED.vcard_data,
			operation = EXCLUDED.operation,
			created_at = EXCLUDED.created_at
	`

	_, err := db.Exec(query, contactID, addressBookID, sourceID, uid, remoteID, vcardData, operation, time.Now())
	if err != nil {
		return fmt.Errorf("failed to queue contact sync: %w", err)
	}
	return nil
}

// GetPendingContactSync retrieves pending contact sync entries for a source
func (db *DB) GetPendingContactSync(sourceID int64, limit int) ([]*models.ContactSyncEntry, error) {
	query := `
		SELECT id, contact_id, address_book_id, source_id, uid, COALESCE(remote_id, ''), COALESCE(vcard_data, ''), operation, created_at
		FROM contact_sync_queue
		WHERE source_id = $1
		ORDER BY created_at ASC
		LIMIT $2
	`

	rows, err := db.Query(query, sourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get pending contact sync: %w", err)
	}
	defer rows.Close()

	var entries []*models.ContactSyncEntry
	for rows.Next() {
		e := &models.ContactSyncEntry{}
		err := rows.Scan(
			&e.ID, &e.ContactID, &e.AddressBookID, &e.SourceID,
			&e.UID, &e.RemoteID, &e.VCardData, &e.Operation, &e.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan contact sync entry: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// DeleteContactSyncEntry removes a completed contact sync entry
func (db *DB) DeleteContactSyncEntry(id int64) error {
	_, err := db.Exec(`DELETE FROM contact_sync_queue WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("failed to delete contact sync entry: %w", err)
	}
	return nil
}

// GetSourcesWithPendingContactSync returns source IDs with pending contact sync entries
func (db *DB) GetSourcesWithPendingContactSync() ([]int64, error) {
	rows, err := db.Query(`SELECT DISTINCT source_id FROM contact_sync_queue`)
	if err != nil {
		return nil, fmt.Errorf("failed to get sources with pending contact sync: %w", err)
	}
	defer rows.Close()

	var sourceIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan source ID: %w", err)
		}
		sourceIDs = append(sourceIDs, id)
	}
	return sourceIDs, nil
}
