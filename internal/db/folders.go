package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// CreateFolder creates a new folder
func (db *DB) CreateFolder(folder *models.Folder) error {
	folder.CreatedAt = time.Now()
	folder.UpdatedAt = time.Now()

	query := `
		INSERT INTO folders (user_id, account_id, name, path, type, parent_id, uid_next, uid_validity, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id
	`

	var parentID sql.NullInt64
	if folder.ParentID > 0 {
		parentID.Int64 = folder.ParentID
		parentID.Valid = true
	}

	var accountID sql.NullInt64
	if folder.AccountID > 0 {
		accountID.Int64 = folder.AccountID
		accountID.Valid = true
	}

	err := db.QueryRow(
		query,
		folder.UserID, accountID, folder.Name, folder.Path, folder.Type,
		parentID, folder.UIDNext, folder.UIDValidity, folder.CreatedAt, folder.UpdatedAt,
	).Scan(&folder.ID)

	if err != nil {
		return fmt.Errorf("failed to create folder: %w", err)
	}

	return nil
}

// GetFoldersByUser retrieves all folders for a user
func (db *DB) GetFoldersByUser(userID int64) ([]*models.Folder, error) {
	query := `
		SELECT id, user_id, COALESCE(account_id, 0), name, path, type, COALESCE(parent_id, 0), uid_next, COALESCE(uid_validity, 0), created_at, updated_at
		FROM folders
		WHERE user_id = $1
		ORDER BY path
	`

	rows, err := db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get folders: %w", err)
	}
	defer rows.Close()

	return scanFolders(rows)
}

// GetLocalFoldersByUser retrieves only local folders (account_id IS NULL) for a user
func (db *DB) GetLocalFoldersByUser(userID int64) ([]*models.Folder, error) {
	query := `
		SELECT id, user_id, COALESCE(account_id, 0), name, path, type, COALESCE(parent_id, 0), uid_next, COALESCE(uid_validity, 0), created_at, updated_at
		FROM folders
		WHERE user_id = $1 AND account_id IS NULL
		ORDER BY path
	`

	rows, err := db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get local folders: %w", err)
	}
	defer rows.Close()

	return scanFolders(rows)
}

// GetLocalFolderByName retrieves a local folder by name for a user
func (db *DB) GetLocalFolderByName(userID int64, name string) (*models.Folder, error) {
	folder := &models.Folder{}
	query := `
		SELECT id, user_id, COALESCE(account_id, 0), name, path, type, COALESCE(parent_id, 0), uid_next, COALESCE(uid_validity, 0), created_at, updated_at
		FROM folders
		WHERE user_id = $1 AND account_id IS NULL AND name = $2
	`

	err := db.QueryRow(query, userID, name).Scan(
		&folder.ID, &folder.UserID, &folder.AccountID, &folder.Name, &folder.Path,
		&folder.Type, &folder.ParentID, &folder.UIDNext, &folder.UIDValidity, &folder.CreatedAt, &folder.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("folder not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get folder: %w", err)
	}

	return folder, nil
}

// GetFoldersByAccount retrieves folders for a specific account
func (db *DB) GetFoldersByAccount(accountID int64) ([]*models.Folder, error) {
	query := `
		SELECT id, user_id, COALESCE(account_id, 0), name, path, type, COALESCE(parent_id, 0), uid_next, COALESCE(uid_validity, 0), created_at, updated_at
		FROM folders
		WHERE account_id = $1
		ORDER BY path
	`

	rows, err := db.Query(query, accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to get folders: %w", err)
	}
	defer rows.Close()

	return scanFolders(rows)
}

// GetFolderByID retrieves a folder by ID
func (db *DB) GetFolderByID(id int64) (*models.Folder, error) {
	folder := &models.Folder{}
	query := `
		SELECT id, user_id, COALESCE(account_id, 0), name, path, type, COALESCE(parent_id, 0), uid_next, COALESCE(uid_validity, 0), created_at, updated_at
		FROM folders
		WHERE id = $1
	`

	err := db.QueryRow(query, id).Scan(
		&folder.ID, &folder.UserID, &folder.AccountID, &folder.Name, &folder.Path,
		&folder.Type, &folder.ParentID, &folder.UIDNext, &folder.UIDValidity, &folder.CreatedAt, &folder.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("folder not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get folder: %w", err)
	}

	return folder, nil
}

// GetFolderByPath retrieves a folder by path
func (db *DB) GetFolderByPath(accountID int64, path string) (*models.Folder, error) {
	folder := &models.Folder{}
	query := `
		SELECT id, user_id, COALESCE(account_id, 0), name, path, type, COALESCE(parent_id, 0), uid_next, COALESCE(uid_validity, 0), created_at, updated_at
		FROM folders
		WHERE account_id = $1 AND path = $2
	`

	err := db.QueryRow(query, accountID, path).Scan(
		&folder.ID, &folder.UserID, &folder.AccountID, &folder.Name, &folder.Path,
		&folder.Type, &folder.ParentID, &folder.UIDNext, &folder.UIDValidity, &folder.CreatedAt, &folder.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("folder not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get folder: %w", err)
	}

	return folder, nil
}

// GetLocalInboxByUser retrieves the local inbox folder for a user (account_id IS NULL)
func (db *DB) GetLocalInboxByUser(userID int64) (*models.Folder, error) {
	folder := &models.Folder{}
	query := `
		SELECT id, user_id, COALESCE(account_id, 0), name, path, type, COALESCE(parent_id, 0), uid_next, COALESCE(uid_validity, 0), created_at, updated_at
		FROM folders
		WHERE user_id = $1 AND account_id IS NULL AND type = 'inbox'
	`

	err := db.QueryRow(query, userID).Scan(
		&folder.ID, &folder.UserID, &folder.AccountID, &folder.Name, &folder.Path,
		&folder.Type, &folder.ParentID, &folder.UIDNext, &folder.UIDValidity, &folder.CreatedAt, &folder.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("local inbox not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get local inbox: %w", err)
	}

	return folder, nil
}

// GetOrCreateLocalInbox gets or creates the local INBOX folder for a user (for MX delivery)
func (db *DB) GetOrCreateLocalInbox(userID int64) (*models.Folder, error) {
	// Try to find existing local INBOX
	folder, err := db.GetLocalInboxByUser(userID)
	if err == nil {
		return folder, nil
	}

	// Create new local INBOX
	inbox := &models.Folder{
		UserID:    userID,
		AccountID: 0, // Will be converted to NULL
		Name:      "INBOX",
		Path:      "INBOX",
		Type:      "inbox",
		UIDNext:   1,
	}

	if err := db.CreateFolder(inbox); err != nil {
		return nil, err
	}

	return inbox, nil
}

// UpdateFolder updates a folder
func (db *DB) UpdateFolder(folder *models.Folder) error {
	folder.UpdatedAt = time.Now()

	query := `
		UPDATE folders
		SET name = $1, path = $2, type = $3, parent_id = $4, uid_next = $5, uid_validity = $6, updated_at = $7
		WHERE id = $8
	`

	var parentID sql.NullInt64
	if folder.ParentID > 0 {
		parentID.Int64 = folder.ParentID
		parentID.Valid = true
	}

	_, err := db.Exec(query, folder.Name, folder.Path, folder.Type, parentID, folder.UIDNext, folder.UIDValidity, folder.UpdatedAt, folder.ID)
	if err != nil {
		return fmt.Errorf("failed to update folder: %w", err)
	}

	return nil
}

// UpdateFolderUIDInfo updates the UID tracking fields for a folder
func (db *DB) UpdateFolderUIDInfo(folderID int64, uidNext, uidValidity uint32) error {
	query := `
		UPDATE folders
		SET uid_next = $1, uid_validity = $2, updated_at = $3
		WHERE id = $4
	`

	_, err := db.Exec(query, uidNext, uidValidity, time.Now(), folderID)
	if err != nil {
		return fmt.Errorf("failed to update folder UID info: %w", err)
	}

	return nil
}

// DeleteFolder deletes a folder
func (db *DB) DeleteFolder(id int64) error {
	query := `DELETE FROM folders WHERE id = $1`
	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete folder: %w", err)
	}
	return nil
}

// GetOrCreateInbox gets or creates the INBOX folder for an account
func (db *DB) GetOrCreateInbox(userID, accountID int64) (*models.Folder, error) {
	// Try to find existing INBOX
	folder, err := db.GetFolderByPath(accountID, "INBOX")
	if err == nil {
		return folder, nil
	}

	// Create new INBOX
	inbox := &models.Folder{
		UserID:    userID,
		AccountID: accountID,
		Name:      "INBOX",
		Path:      "INBOX",
		Type:      "inbox",
		UIDNext:   1,
	}

	if err := db.CreateFolder(inbox); err != nil {
		return nil, err
	}

	return inbox, nil
}

// Helper function to scan multiple folders
func scanFolders(rows *sql.Rows) ([]*models.Folder, error) {
	var folders []*models.Folder

	for rows.Next() {
		folder := &models.Folder{}
		err := rows.Scan(
			&folder.ID, &folder.UserID, &folder.AccountID, &folder.Name, &folder.Path,
			&folder.Type, &folder.ParentID, &folder.UIDNext, &folder.UIDValidity, &folder.CreatedAt, &folder.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan folder: %w", err)
		}
		folders = append(folders, folder)
	}

	return folders, nil
}
