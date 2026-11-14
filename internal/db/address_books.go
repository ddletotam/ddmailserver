package db

import (
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// CreateAddressBook creates a new address book
func (db *DB) CreateAddressBook(book *models.AddressBook) error {
	book.CreatedAt = timeutil.Now()
	book.UpdatedAt = timeutil.Now()

	query := `
		INSERT INTO address_books (
			user_id, source_id, remote_id, name, description, ctag, can_write,
			reverse_sync, enabled, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id
	`

	err := db.QueryRow(
		query,
		book.UserID, book.SourceID, book.RemoteID,
		book.Name, book.Description, book.CTag, book.CanWrite,
		book.ReverseSync, book.Enabled,
		book.CreatedAt, book.UpdatedAt,
	).Scan(&book.ID)

	if err != nil {
		return fmt.Errorf("failed to create address book: %w", err)
	}

	return nil
}

// GetAddressBooksByUserID retrieves all address books for a user
func (db *DB) GetAddressBooksByUserID(userID int64) ([]*models.AddressBook, error) {
	return db.GetAddressBooksByUserIDFiltered(userID, false)
}

// GetEnabledAddressBooksByUserID retrieves only enabled address books for a user
func (db *DB) GetEnabledAddressBooksByUserID(userID int64) ([]*models.AddressBook, error) {
	return db.GetAddressBooksByUserIDFiltered(userID, true)
}

// GetAddressBooksByUserIDFiltered retrieves address books for a user with optional enabled filter
func (db *DB) GetAddressBooksByUserIDFiltered(userID int64, enabledOnly bool) ([]*models.AddressBook, error) {
	query := `
		SELECT ab.id, ab.user_id, ab.source_id, COALESCE(ab.remote_id, ''),
		       ab.name, COALESCE(ab.description, ''), COALESCE(ab.ctag, ''),
		       ab.can_write, COALESCE(ab.reverse_sync, false), COALESCE(ab.enabled, true),
		       ab.created_at, ab.updated_at, cs.source_type
		FROM address_books ab
		JOIN contact_sources cs ON ab.source_id = cs.id
		WHERE ab.user_id = $1
	`
	if enabledOnly {
		query += " AND COALESCE(ab.enabled, true) = true"
	}
	query += " ORDER BY ab.name ASC"

	rows, err := db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get address books: %w", err)
	}
	defer rows.Close()

	return scanAddressBooks(rows)
}

// GetAddressBooksBySourceID retrieves all address books for a source
func (db *DB) GetAddressBooksBySourceID(sourceID int64) ([]*models.AddressBook, error) {
	query := `
		SELECT ab.id, ab.user_id, ab.source_id, COALESCE(ab.remote_id, ''),
		       ab.name, COALESCE(ab.description, ''), COALESCE(ab.ctag, ''),
		       ab.can_write, COALESCE(ab.reverse_sync, false), COALESCE(ab.enabled, true),
		       ab.created_at, ab.updated_at, cs.source_type
		FROM address_books ab
		JOIN contact_sources cs ON ab.source_id = cs.id
		WHERE ab.source_id = $1
		ORDER BY ab.name ASC
	`

	rows, err := db.Query(query, sourceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get address books: %w", err)
	}
	defer rows.Close()

	return scanAddressBooks(rows)
}

// GetAddressBookByID retrieves an address book by ID
func (db *DB) GetAddressBookByID(id int64) (*models.AddressBook, error) {
	query := `
		SELECT ab.id, ab.user_id, ab.source_id, COALESCE(ab.remote_id, ''),
		       ab.name, COALESCE(ab.description, ''), COALESCE(ab.ctag, ''),
		       ab.can_write, COALESCE(ab.reverse_sync, false), COALESCE(ab.enabled, true),
		       ab.created_at, ab.updated_at, cs.source_type
		FROM address_books ab
		JOIN contact_sources cs ON ab.source_id = cs.id
		WHERE ab.id = $1
	`

	book := &models.AddressBook{}
	err := db.QueryRow(query, id).Scan(
		&book.ID, &book.UserID, &book.SourceID, &book.RemoteID,
		&book.Name, &book.Description, &book.CTag,
		&book.CanWrite, &book.ReverseSync, &book.Enabled,
		&book.CreatedAt, &book.UpdatedAt, &book.SourceType,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("address book not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get address book: %w", err)
	}

	return book, nil
}

// GetAddressBookByRemoteID retrieves an address book by source and remote ID
func (db *DB) GetAddressBookByRemoteID(sourceID int64, remoteID string) (*models.AddressBook, error) {
	query := `
		SELECT ab.id, ab.user_id, ab.source_id, COALESCE(ab.remote_id, ''),
		       ab.name, COALESCE(ab.description, ''), COALESCE(ab.ctag, ''),
		       ab.can_write, COALESCE(ab.reverse_sync, false), COALESCE(ab.enabled, true),
		       ab.created_at, ab.updated_at, cs.source_type
		FROM address_books ab
		JOIN contact_sources cs ON ab.source_id = cs.id
		WHERE ab.source_id = $1 AND ab.remote_id = $2
	`

	book := &models.AddressBook{}
	err := db.QueryRow(query, sourceID, remoteID).Scan(
		&book.ID, &book.UserID, &book.SourceID, &book.RemoteID,
		&book.Name, &book.Description, &book.CTag,
		&book.CanWrite, &book.ReverseSync, &book.Enabled,
		&book.CreatedAt, &book.UpdatedAt, &book.SourceType,
	)

	if err == sql.ErrNoRows {
		return nil, nil // Not found is not an error
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get address book by remote ID: %w", err)
	}

	return book, nil
}

// UpdateAddressBook updates an existing address book
func (db *DB) UpdateAddressBook(book *models.AddressBook) error {
	book.UpdatedAt = timeutil.Now()

	query := `
		UPDATE address_books SET
			name = $1, description = $2, ctag = $3, can_write = $4, updated_at = $5
		WHERE id = $6
	`

	_, err := db.Exec(
		query,
		book.Name, book.Description, book.CTag, book.CanWrite, book.UpdatedAt,
		book.ID,
	)

	if err != nil {
		return fmt.Errorf("failed to update address book: %w", err)
	}

	return nil
}

// RefreshAddressBookCanWrite переносит на известную книгу права, которые
// сервер сообщил при обнаружении (remote.CanWriteKnown). Права меняются:
// каталог стал только для чтения, дали доступ на запись; а до проверки прав
// у всех CardDAV-книг стояло предположенное true. Без ответа сервера ничего
// не трогает.
func (db *DB) RefreshAddressBookCanWrite(book, remote *models.AddressBook) error {
	if !remote.CanWriteKnown || book.CanWrite == remote.CanWrite {
		return nil
	}
	now := timeutil.Now()
	if _, err := db.Exec(`UPDATE address_books SET can_write = $1, updated_at = $2 WHERE id = $3`,
		remote.CanWrite, now, book.ID); err != nil {
		return fmt.Errorf("update can_write: %w", err)
	}
	log.Printf("Address book %d (%s): can_write %v -> %v (server privileges)", book.ID, book.Name, book.CanWrite, remote.CanWrite)
	book.CanWrite = remote.CanWrite
	book.UpdatedAt = now
	return nil
}

// UpdateAddressBookCTag updates the CTag for an address book
func (db *DB) UpdateAddressBookCTag(id int64, ctag string) error {
	query := `UPDATE address_books SET ctag = $1, updated_at = $2 WHERE id = $3`
	_, err := db.Exec(query, ctag, timeutil.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to update address book ctag: %w", err)
	}
	return nil
}

// DeleteAddressBook deletes an address book and all its contacts
func (db *DB) DeleteAddressBook(id int64) error {
	query := `DELETE FROM address_books WHERE id = $1`
	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete address book: %w", err)
	}
	return nil
}

// scanAddressBooks scans multiple address book rows
func scanAddressBooks(rows *sql.Rows) ([]*models.AddressBook, error) {
	var books []*models.AddressBook
	for rows.Next() {
		book := &models.AddressBook{}
		err := rows.Scan(
			&book.ID, &book.UserID, &book.SourceID, &book.RemoteID,
			&book.Name, &book.Description, &book.CTag,
			&book.CanWrite, &book.ReverseSync, &book.Enabled,
			&book.CreatedAt, &book.UpdatedAt, &book.SourceType,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan address book: %w", err)
		}
		books = append(books, book)
	}
	return books, nil
}

// GetOrCreateLocalAddressBook gets or creates a default local address book for a user
func (db *DB) GetOrCreateLocalAddressBook(userID int64, sourceID int64) (*models.AddressBook, error) {
	// Try to find existing local address book
	books, err := db.GetAddressBooksBySourceID(sourceID)
	if err != nil {
		return nil, err
	}

	if len(books) > 0 {
		return books[0], nil
	}

	// Create default address book
	book := &models.AddressBook{
		UserID:   userID,
		SourceID: sourceID,
		Name:     "Contacts",
		CanWrite: true,
		Enabled:  true,
	}

	if err := db.CreateAddressBook(book); err != nil {
		return nil, err
	}

	return book, nil
}

// PruneDirectURLAddressBook is PruneDirectURLCalendar for contacts: it removes
// the placeholder book created while CardDAV discovery was failing, once
// discovery works and real books have been found.
//
// Same guard, same reason — only an EMPTY row is removed, because a server with
// no discovery at all keeps its contacts in exactly such a row.
// Returns whether a row was removed.
func (db *DB) PruneDirectURLAddressBook(sourceID int64, sourceURL string) (bool, error) {
	if strings.TrimSpace(sourceURL) == "" {
		return false, nil
	}

	// The placeholder took the source URL as typed at the time; the source may
	// have been saved with or without the trailing slash since. Both spellings
	// name the same placeholder (one row of a Yandex source on prod stayed for
	// weeks because of exactly this).
	rows, err := db.Query(`
		SELECT ab.id, (SELECT COUNT(*) FROM contacts c WHERE c.address_book_id = ab.id)
		FROM address_books ab
		WHERE ab.source_id = $1 AND rtrim(ab.remote_id, '/') = rtrim($2, '/')
	`, sourceID, strings.TrimSpace(sourceURL))
	if err != nil {
		return false, fmt.Errorf("failed to look up direct-URL address book: %w", err)
	}
	var empty []int64
	for rows.Next() {
		var id int64
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			rows.Close()
			return false, fmt.Errorf("failed to scan direct-URL address book: %w", err)
		}
		if count == 0 {
			empty = append(empty, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("failed to look up direct-URL address book: %w", err)
	}

	for _, id := range empty {
		if _, err := db.Exec(`DELETE FROM address_books WHERE id = $1`, id); err != nil {
			return false, fmt.Errorf("failed to delete direct-URL address book %d: %w", id, err)
		}
	}
	return len(empty) > 0, nil
}

// isDirectURLBook — книга-заглушка, заведённая на адрес источника, когда
// обнаружение не сработало (см. useDirectAddressBookURL). Слэш в конце не в
// счёт: адрес источника сохраняли то с ним, то без.
func isDirectURLBook(remoteID, sourceURL string) bool {
	sourceURL = strings.TrimRight(strings.TrimSpace(sourceURL), "/")
	return sourceURL != "" && strings.TrimRight(remoteID, "/") == sourceURL
}

// sameCollection — один ли это путь коллекции: сервер может отдать href то с
// %40, то с @ (Яндекс отдаёт /addressbook/user%40domain/1/).
func sameCollection(a, b string) bool {
	if a == b {
		return true
	}
	ua, errA := url.PathUnescape(a)
	ub, errB := url.PathUnescape(b)
	return errA == nil && errB == nil && ua == ub
}

// PruneVanishedAddressBooks удаляет книги источника, которых больше нет в
// ответе обнаружения, вместе с их контактами. Возвращает имена удалённых.
//
// Зачем: Яндекс в сентябре 2026 отдал коллекции под другими путями
// (organization:…:0/, user:…:0/ рядом с /1/ и /2/), синк завёл их отдельными
// книгами, а потом пути пропали — книги остались навсегда, не синкались и
// показывались клиентам. Запись в такую «призрачную» книгу уходила PUT'ом по
// старому пути, Яндекс клал карточку в настоящую коллекцию, и следующий синк
// приносил её второй раз — уже в «Personal» (аудит 2026-11-14, №7).
//
// Удаляем, только когда обнаружение ответило по-настоящему: непустой список
// без заглушки на адрес источника (заглушка = обнаружение упало, список ничего
// не говорит о том, каких книг нет). Заглушки не трогаем — их судьба у
// PruneDirectURLAddressBook. Книгу с неотправленными правками
// (contact_sync_queue) не трогаем: правка ещё должна уйти наверх.
func (db *DB) PruneVanishedAddressBooks(sourceID int64, sourceURL string, discovered []*models.AddressBook) ([]string, error) {
	if len(discovered) == 0 {
		return nil, nil
	}
	for _, d := range discovered {
		if d.RemoteID == "" || isDirectURLBook(d.RemoteID, sourceURL) {
			return nil, nil
		}
	}

	books, err := db.GetAddressBooksBySourceID(sourceID)
	if err != nil {
		return nil, err
	}
	var pruned []string
	for _, b := range books {
		if b.RemoteID == "" || isDirectURLBook(b.RemoteID, sourceURL) {
			continue
		}
		found := false
		for _, d := range discovered {
			if sameCollection(b.RemoteID, d.RemoteID) {
				found = true
				break
			}
		}
		if found {
			continue
		}
		res, err := db.Exec(`
			DELETE FROM address_books ab
			WHERE ab.id = $1
			  AND NOT EXISTS (SELECT 1 FROM contact_sync_queue q WHERE q.address_book_id = ab.id)
		`, b.ID)
		if err != nil {
			return pruned, fmt.Errorf("failed to delete vanished address book %d: %w", b.ID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			log.Printf("Address book %d (%s) is gone from the server but has unsent changes — kept for now", b.ID, b.RemoteID)
			continue
		}
		log.Printf("Address book %d (%q, %s) is gone from the server — removed with its contacts", b.ID, b.Name, b.RemoteID)
		pruned = append(pruned, b.Name)
	}
	return pruned, nil
}

// PruneDirectURLAddressBookIfDiscovered is the contacts counterpart of
// PruneDirectURLCalendarIfDiscovered, and exists for the same reason: contact
// sync also has two implementations, and the rule must be stated once.
func (db *DB) PruneDirectURLAddressBookIfDiscovered(sourceID int64, sourceURL string, discovered []*models.AddressBook) (bool, error) {
	worked := false
	for _, b := range discovered {
		if !isDirectURLBook(b.RemoteID, sourceURL) {
			worked = true
			break
		}
	}
	if !worked {
		return false, nil
	}
	return db.PruneDirectURLAddressBook(sourceID, sourceURL)
}
