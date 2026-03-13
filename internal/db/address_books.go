package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// CreateAddressBook creates a new address book
func (db *DB) CreateAddressBook(book *models.AddressBook) error {
	book.CreatedAt = time.Now()
	book.UpdatedAt = time.Now()

	query := `
		INSERT INTO address_books (
			user_id, source_id, remote_id, name, description, ctag, can_write, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id
	`

	err := db.QueryRow(
		query,
		book.UserID, book.SourceID, book.RemoteID,
		book.Name, book.Description, book.CTag, book.CanWrite,
		book.CreatedAt, book.UpdatedAt,
	).Scan(&book.ID)

	if err != nil {
		return fmt.Errorf("failed to create address book: %w", err)
	}

	return nil
}

// GetAddressBooksByUserID retrieves all address books for a user
func (db *DB) GetAddressBooksByUserID(userID int64) ([]*models.AddressBook, error) {
	query := `
		SELECT ab.id, ab.user_id, ab.source_id, COALESCE(ab.remote_id, ''),
		       ab.name, COALESCE(ab.description, ''), COALESCE(ab.ctag, ''),
		       ab.can_write, ab.created_at, ab.updated_at,
		       cs.source_type
		FROM address_books ab
		JOIN contact_sources cs ON ab.source_id = cs.id
		WHERE ab.user_id = $1
		ORDER BY ab.name ASC
	`

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
		       ab.can_write, ab.created_at, ab.updated_at,
		       cs.source_type
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
		       ab.can_write, ab.created_at, ab.updated_at,
		       cs.source_type
		FROM address_books ab
		JOIN contact_sources cs ON ab.source_id = cs.id
		WHERE ab.id = $1
	`

	book := &models.AddressBook{}
	err := db.QueryRow(query, id).Scan(
		&book.ID, &book.UserID, &book.SourceID, &book.RemoteID,
		&book.Name, &book.Description, &book.CTag,
		&book.CanWrite, &book.CreatedAt, &book.UpdatedAt,
		&book.SourceType,
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
		       ab.can_write, ab.created_at, ab.updated_at,
		       cs.source_type
		FROM address_books ab
		JOIN contact_sources cs ON ab.source_id = cs.id
		WHERE ab.source_id = $1 AND ab.remote_id = $2
	`

	book := &models.AddressBook{}
	err := db.QueryRow(query, sourceID, remoteID).Scan(
		&book.ID, &book.UserID, &book.SourceID, &book.RemoteID,
		&book.Name, &book.Description, &book.CTag,
		&book.CanWrite, &book.CreatedAt, &book.UpdatedAt,
		&book.SourceType,
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
	book.UpdatedAt = time.Now()

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

// UpdateAddressBookCTag updates the CTag for an address book
func (db *DB) UpdateAddressBookCTag(id int64, ctag string) error {
	query := `UPDATE address_books SET ctag = $1, updated_at = $2 WHERE id = $3`
	_, err := db.Exec(query, ctag, time.Now(), id)
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
			&book.CanWrite, &book.CreatedAt, &book.UpdatedAt,
			&book.SourceType,
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
	}

	if err := db.CreateAddressBook(book); err != nil {
		return nil, err
	}

	return book, nil
}
