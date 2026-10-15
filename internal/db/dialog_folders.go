package db

import (
	"database/sql"
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// GetDialogFolders returns the user's desktop dialog-folders document and its
// version. A user who never saved one gets ("", 0, nil).
func (db *DB) GetDialogFolders(userID int64) (string, int64, error) {
	var doc string
	var version int64
	err := db.QueryRow(
		`SELECT doc, version FROM dialog_folders WHERE user_id = $1`, userID,
	).Scan(&doc, &version)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("failed to get dialog folders: %w", err)
	}
	return doc, version, nil
}

// SaveDialogFolders writes the document if the stored version is still
// baseVersion (0 = nothing stored yet). Returns the new version and true on
// success; on a version mismatch nothing is written and it returns the
// current version and false, so the caller can refetch and reapply.
func (db *DB) SaveDialogFolders(userID int64, doc string, baseVersion int64) (int64, bool, error) {
	now := timeutil.Now()
	next := baseVersion + 1
	var res sql.Result
	var err error
	if baseVersion == 0 {
		res, err = db.Exec(
			`INSERT INTO dialog_folders (user_id, doc, version, updated_at)
			 VALUES ($1, $2, 1, $3)
			 ON CONFLICT (user_id) DO NOTHING`,
			userID, doc, now,
		)
	} else {
		res, err = db.Exec(
			`UPDATE dialog_folders SET doc = $2, version = $3, updated_at = $4
			 WHERE user_id = $1 AND version = $5`,
			userID, doc, next, now, baseVersion,
		)
	}
	if err != nil {
		return 0, false, fmt.Errorf("failed to save dialog folders: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("failed to save dialog folders: %w", err)
	}
	if n == 1 {
		return next, true, nil
	}
	_, current, err := db.GetDialogFolders(userID)
	if err != nil {
		return 0, false, err
	}
	return current, false, nil
}
