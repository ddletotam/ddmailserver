package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// CreateContact creates a new contact
func (db *DB) CreateContact(contact *models.Contact) error {
	contact.CreatedAt = time.Now()
	contact.UpdatedAt = time.Now()

	var birthday sql.NullTime
	if contact.Birthday.Valid {
		birthday = contact.Birthday
	}

	query := `
		INSERT INTO contacts (
			user_id, address_book_id, uid, remote_id, vcard_data,
			full_name, given_name, family_name, nickname,
			email, email2, email3, phone, phone2, phone3,
			organization, title, department, address, notes, photo_url, birthday,
			etag, local_modified, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26)
		RETURNING id
	`

	err := db.QueryRow(
		query,
		contact.UserID, contact.AddressBookID, contact.UID, contact.RemoteID, contact.VCardData,
		contact.FullName, contact.GivenName, contact.FamilyName, contact.Nickname,
		contact.Email, contact.Email2, contact.Email3, contact.Phone, contact.Phone2, contact.Phone3,
		contact.Organization, contact.Title, contact.Department, contact.Address, contact.Notes, contact.PhotoURL, birthday,
		contact.ETag, contact.LocalModified, contact.CreatedAt, contact.UpdatedAt,
	).Scan(&contact.ID)

	if err != nil {
		return fmt.Errorf("failed to create contact: %w", err)
	}

	return nil
}

// GetContactsByAddressBookID retrieves all contacts for an address book
func (db *DB) GetContactsByAddressBookID(addressBookID int64) ([]*models.Contact, error) {
	query := `
		SELECT id, user_id, address_book_id, uid, COALESCE(remote_id, ''), vcard_data,
		       COALESCE(full_name, ''), COALESCE(given_name, ''), COALESCE(family_name, ''), COALESCE(nickname, ''),
		       COALESCE(email, ''), COALESCE(email2, ''), COALESCE(email3, ''),
		       COALESCE(phone, ''), COALESCE(phone2, ''), COALESCE(phone3, ''),
		       COALESCE(organization, ''), COALESCE(title, ''), COALESCE(department, ''),
		       COALESCE(address, ''), COALESCE(notes, ''), COALESCE(photo_url, ''), birthday,
		       COALESCE(etag, ''), local_modified, created_at, updated_at
		FROM contacts
		WHERE address_book_id = $1
		ORDER BY COALESCE(full_name, email, '') ASC
	`

	rows, err := db.Query(query, addressBookID)
	if err != nil {
		return nil, fmt.Errorf("failed to get contacts: %w", err)
	}
	defer rows.Close()

	return scanContacts(rows)
}

// GetContactsByUserID retrieves all contacts for a user
func (db *DB) GetContactsByUserID(userID int64, limit, offset int) ([]*models.Contact, error) {
	query := `
		SELECT id, user_id, address_book_id, uid, COALESCE(remote_id, ''), vcard_data,
		       COALESCE(full_name, ''), COALESCE(given_name, ''), COALESCE(family_name, ''), COALESCE(nickname, ''),
		       COALESCE(email, ''), COALESCE(email2, ''), COALESCE(email3, ''),
		       COALESCE(phone, ''), COALESCE(phone2, ''), COALESCE(phone3, ''),
		       COALESCE(organization, ''), COALESCE(title, ''), COALESCE(department, ''),
		       COALESCE(address, ''), COALESCE(notes, ''), COALESCE(photo_url, ''), birthday,
		       COALESCE(etag, ''), local_modified, created_at, updated_at
		FROM contacts
		WHERE user_id = $1
		ORDER BY COALESCE(full_name, email, '') ASC
		LIMIT $2 OFFSET $3
	`

	rows, err := db.Query(query, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to get contacts: %w", err)
	}
	defer rows.Close()

	return scanContacts(rows)
}

// GetContactByID retrieves a contact by ID
func (db *DB) GetContactByID(id int64) (*models.Contact, error) {
	query := `
		SELECT id, user_id, address_book_id, uid, COALESCE(remote_id, ''), vcard_data,
		       COALESCE(full_name, ''), COALESCE(given_name, ''), COALESCE(family_name, ''), COALESCE(nickname, ''),
		       COALESCE(email, ''), COALESCE(email2, ''), COALESCE(email3, ''),
		       COALESCE(phone, ''), COALESCE(phone2, ''), COALESCE(phone3, ''),
		       COALESCE(organization, ''), COALESCE(title, ''), COALESCE(department, ''),
		       COALESCE(address, ''), COALESCE(notes, ''), COALESCE(photo_url, ''), birthday,
		       COALESCE(etag, ''), local_modified, created_at, updated_at
		FROM contacts
		WHERE id = $1
	`

	contact := &models.Contact{}
	err := db.QueryRow(query, id).Scan(
		&contact.ID, &contact.UserID, &contact.AddressBookID, &contact.UID, &contact.RemoteID, &contact.VCardData,
		&contact.FullName, &contact.GivenName, &contact.FamilyName, &contact.Nickname,
		&contact.Email, &contact.Email2, &contact.Email3,
		&contact.Phone, &contact.Phone2, &contact.Phone3,
		&contact.Organization, &contact.Title, &contact.Department,
		&contact.Address, &contact.Notes, &contact.PhotoURL, &contact.Birthday,
		&contact.ETag, &contact.LocalModified, &contact.CreatedAt, &contact.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("contact not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get contact: %w", err)
	}

	return contact, nil
}

// GetContactByUID retrieves a contact by UID within an address book
func (db *DB) GetContactByUID(addressBookID int64, uid string) (*models.Contact, error) {
	query := `
		SELECT id, user_id, address_book_id, uid, COALESCE(remote_id, ''), vcard_data,
		       COALESCE(full_name, ''), COALESCE(given_name, ''), COALESCE(family_name, ''), COALESCE(nickname, ''),
		       COALESCE(email, ''), COALESCE(email2, ''), COALESCE(email3, ''),
		       COALESCE(phone, ''), COALESCE(phone2, ''), COALESCE(phone3, ''),
		       COALESCE(organization, ''), COALESCE(title, ''), COALESCE(department, ''),
		       COALESCE(address, ''), COALESCE(notes, ''), COALESCE(photo_url, ''), birthday,
		       COALESCE(etag, ''), local_modified, created_at, updated_at
		FROM contacts
		WHERE address_book_id = $1 AND uid = $2
	`

	contact := &models.Contact{}
	err := db.QueryRow(query, addressBookID, uid).Scan(
		&contact.ID, &contact.UserID, &contact.AddressBookID, &contact.UID, &contact.RemoteID, &contact.VCardData,
		&contact.FullName, &contact.GivenName, &contact.FamilyName, &contact.Nickname,
		&contact.Email, &contact.Email2, &contact.Email3,
		&contact.Phone, &contact.Phone2, &contact.Phone3,
		&contact.Organization, &contact.Title, &contact.Department,
		&contact.Address, &contact.Notes, &contact.PhotoURL, &contact.Birthday,
		&contact.ETag, &contact.LocalModified, &contact.CreatedAt, &contact.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil // Not found is not an error
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get contact by UID: %w", err)
	}

	return contact, nil
}

// UpdateContact updates an existing contact
func (db *DB) UpdateContact(contact *models.Contact) error {
	contact.UpdatedAt = time.Now()

	var birthday sql.NullTime
	if contact.Birthday.Valid {
		birthday = contact.Birthday
	}

	query := `
		UPDATE contacts SET
			vcard_data = $1, full_name = $2, given_name = $3, family_name = $4, nickname = $5,
			email = $6, email2 = $7, email3 = $8, phone = $9, phone2 = $10, phone3 = $11,
			organization = $12, title = $13, department = $14, address = $15, notes = $16, photo_url = $17, birthday = $18,
			etag = $19, local_modified = $20, updated_at = $21
		WHERE id = $22
	`

	_, err := db.Exec(
		query,
		contact.VCardData, contact.FullName, contact.GivenName, contact.FamilyName, contact.Nickname,
		contact.Email, contact.Email2, contact.Email3, contact.Phone, contact.Phone2, contact.Phone3,
		contact.Organization, contact.Title, contact.Department, contact.Address, contact.Notes, contact.PhotoURL, birthday,
		contact.ETag, contact.LocalModified, contact.UpdatedAt,
		contact.ID,
	)

	if err != nil {
		return fmt.Errorf("failed to update contact: %w", err)
	}

	return nil
}

// DeleteContact deletes a contact
func (db *DB) DeleteContact(id int64) error {
	query := `DELETE FROM contacts WHERE id = $1`
	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete contact: %w", err)
	}
	return nil
}

// DeleteContactByUID deletes a contact by UID within an address book
func (db *DB) DeleteContactByUID(addressBookID int64, uid string) error {
	query := `DELETE FROM contacts WHERE address_book_id = $1 AND uid = $2`
	_, err := db.Exec(query, addressBookID, uid)
	if err != nil {
		return fmt.Errorf("failed to delete contact by UID: %w", err)
	}
	return nil
}

// SearchContacts searches contacts by name, email, or phone
func (db *DB) SearchContacts(userID int64, query string, limit int) ([]*models.Contact, error) {
	searchQuery := `
		SELECT id, user_id, address_book_id, uid, COALESCE(remote_id, ''), vcard_data,
		       COALESCE(full_name, ''), COALESCE(given_name, ''), COALESCE(family_name, ''), COALESCE(nickname, ''),
		       COALESCE(email, ''), COALESCE(email2, ''), COALESCE(email3, ''),
		       COALESCE(phone, ''), COALESCE(phone2, ''), COALESCE(phone3, ''),
		       COALESCE(organization, ''), COALESCE(title, ''), COALESCE(department, ''),
		       COALESCE(address, ''), COALESCE(notes, ''), COALESCE(photo_url, ''), birthday,
		       COALESCE(etag, ''), local_modified, created_at, updated_at
		FROM contacts
		WHERE user_id = $1 AND (
			full_name ILIKE $2 OR
			given_name ILIKE $2 OR
			family_name ILIKE $2 OR
			email ILIKE $2 OR
			email2 ILIKE $2 OR
			email3 ILIKE $2 OR
			phone ILIKE $2 OR
			organization ILIKE $2
		)
		ORDER BY COALESCE(full_name, email, '') ASC
		LIMIT $3
	`

	pattern := "%" + query + "%"
	rows, err := db.Query(searchQuery, userID, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to search contacts: %w", err)
	}
	defer rows.Close()

	return scanContacts(rows)
}

// GetLocallyModifiedContacts retrieves contacts that have been modified locally
func (db *DB) GetLocallyModifiedContacts(addressBookID int64) ([]*models.Contact, error) {
	query := `
		SELECT id, user_id, address_book_id, uid, COALESCE(remote_id, ''), vcard_data,
		       COALESCE(full_name, ''), COALESCE(given_name, ''), COALESCE(family_name, ''), COALESCE(nickname, ''),
		       COALESCE(email, ''), COALESCE(email2, ''), COALESCE(email3, ''),
		       COALESCE(phone, ''), COALESCE(phone2, ''), COALESCE(phone3, ''),
		       COALESCE(organization, ''), COALESCE(title, ''), COALESCE(department, ''),
		       COALESCE(address, ''), COALESCE(notes, ''), COALESCE(photo_url, ''), birthday,
		       COALESCE(etag, ''), local_modified, created_at, updated_at
		FROM contacts
		WHERE address_book_id = $1 AND local_modified = true
	`

	rows, err := db.Query(query, addressBookID)
	if err != nil {
		return nil, fmt.Errorf("failed to get locally modified contacts: %w", err)
	}
	defer rows.Close()

	return scanContacts(rows)
}

// MarkContactSynced marks a contact as synced (not locally modified)
func (db *DB) MarkContactSynced(id int64, etag string) error {
	query := `UPDATE contacts SET etag = $1, local_modified = false, updated_at = $2 WHERE id = $3`
	_, err := db.Exec(query, etag, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to mark contact synced: %w", err)
	}
	return nil
}

// CountContactsByUserID returns the total number of contacts for a user
func (db *DB) CountContactsByUserID(userID int64) (int, error) {
	var count int
	query := `SELECT COUNT(*) FROM contacts WHERE user_id = $1`
	err := db.QueryRow(query, userID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count contacts: %w", err)
	}
	return count, nil
}

// GetContactCountForAddressBook returns the number of contacts in an address book
func (db *DB) GetContactCountForAddressBook(addressBookID int64) (int, error) {
	var count int
	query := `SELECT COUNT(*) FROM contacts WHERE address_book_id = $1`
	err := db.QueryRow(query, addressBookID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count contacts: %w", err)
	}
	return count, nil
}

// GetAllUserContacts retrieves all contacts for a user up to a limit
func (db *DB) GetAllUserContacts(userID int64, limit int) ([]*models.Contact, error) {
	query := `
		SELECT id, user_id, address_book_id, uid, COALESCE(remote_id, ''), vcard_data,
		       COALESCE(full_name, ''), COALESCE(given_name, ''), COALESCE(family_name, ''), COALESCE(nickname, ''),
		       COALESCE(email, ''), COALESCE(email2, ''), COALESCE(email3, ''),
		       COALESCE(phone, ''), COALESCE(phone2, ''), COALESCE(phone3, ''),
		       COALESCE(organization, ''), COALESCE(title, ''), COALESCE(department, ''),
		       COALESCE(address, ''), COALESCE(notes, ''), COALESCE(photo_url, ''), birthday,
		       COALESCE(etag, ''), local_modified, created_at, updated_at
		FROM contacts
		WHERE user_id = $1
		ORDER BY COALESCE(full_name, email, '') ASC
		LIMIT $2
	`

	rows, err := db.Query(query, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get contacts: %w", err)
	}
	defer rows.Close()

	return scanContacts(rows)
}

// scanContacts scans multiple contact rows
func scanContacts(rows *sql.Rows) ([]*models.Contact, error) {
	var contacts []*models.Contact
	for rows.Next() {
		contact := &models.Contact{}
		err := rows.Scan(
			&contact.ID, &contact.UserID, &contact.AddressBookID, &contact.UID, &contact.RemoteID, &contact.VCardData,
			&contact.FullName, &contact.GivenName, &contact.FamilyName, &contact.Nickname,
			&contact.Email, &contact.Email2, &contact.Email3,
			&contact.Phone, &contact.Phone2, &contact.Phone3,
			&contact.Organization, &contact.Title, &contact.Department,
			&contact.Address, &contact.Notes, &contact.PhotoURL, &contact.Birthday,
			&contact.ETag, &contact.LocalModified, &contact.CreatedAt, &contact.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan contact: %w", err)
		}
		contacts = append(contacts, contact)
	}
	return contacts, nil
}

// SyncContactChanges represents changes to apply during sync
type SyncContactChanges struct {
	Creates    []*models.Contact
	Updates    []*models.Contact
	DeleteUIDs []string
}

// ApplyContactSyncChanges applies sync changes atomically
func (db *DB) ApplyContactSyncChanges(addressBookID int64, changes *SyncContactChanges) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Delete contacts
	for _, uid := range changes.DeleteUIDs {
		_, err := tx.Exec(`DELETE FROM contacts WHERE address_book_id = $1 AND uid = $2`, addressBookID, uid)
		if err != nil {
			return fmt.Errorf("failed to delete contact %s: %w", uid, err)
		}
	}

	// Create new contacts
	for _, contact := range changes.Creates {
		contact.AddressBookID = addressBookID
		contact.CreatedAt = time.Now()
		contact.UpdatedAt = time.Now()

		var birthday sql.NullTime
		if contact.Birthday.Valid {
			birthday = contact.Birthday
		}

		query := `
			INSERT INTO contacts (
				user_id, address_book_id, uid, remote_id, vcard_data,
				full_name, given_name, family_name, nickname,
				email, email2, email3, phone, phone2, phone3,
				organization, title, department, address, notes, photo_url, birthday,
				etag, local_modified, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26)
			RETURNING id
		`

		err := tx.QueryRow(
			query,
			contact.UserID, contact.AddressBookID, contact.UID, contact.RemoteID, contact.VCardData,
			contact.FullName, contact.GivenName, contact.FamilyName, contact.Nickname,
			contact.Email, contact.Email2, contact.Email3, contact.Phone, contact.Phone2, contact.Phone3,
			contact.Organization, contact.Title, contact.Department, contact.Address, contact.Notes, contact.PhotoURL, birthday,
			contact.ETag, false, contact.CreatedAt, contact.UpdatedAt,
		).Scan(&contact.ID)

		if err != nil {
			return fmt.Errorf("failed to create contact %s: %w", contact.UID, err)
		}
	}

	// Update existing contacts
	for _, contact := range changes.Updates {
		contact.UpdatedAt = time.Now()

		var birthday sql.NullTime
		if contact.Birthday.Valid {
			birthday = contact.Birthday
		}

		query := `
			UPDATE contacts SET
				vcard_data = $1, full_name = $2, given_name = $3, family_name = $4, nickname = $5,
				email = $6, email2 = $7, email3 = $8, phone = $9, phone2 = $10, phone3 = $11,
				organization = $12, title = $13, department = $14, address = $15, notes = $16, photo_url = $17, birthday = $18,
				etag = $19, local_modified = false, updated_at = $20
			WHERE id = $21
		`

		_, err := tx.Exec(
			query,
			contact.VCardData, contact.FullName, contact.GivenName, contact.FamilyName, contact.Nickname,
			contact.Email, contact.Email2, contact.Email3, contact.Phone, contact.Phone2, contact.Phone3,
			contact.Organization, contact.Title, contact.Department, contact.Address, contact.Notes, contact.PhotoURL, birthday,
			contact.ETag, contact.UpdatedAt,
			contact.ID,
		)

		if err != nil {
			return fmt.Errorf("failed to update contact %s: %w", contact.UID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}
