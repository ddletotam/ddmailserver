package db

import (
	"fmt"
	"strings"

	"github.com/yourusername/mailserver/internal/models"
)

// ConflictStrategy is what to do when the profile describes an account that
// already exists. accounts carries UNIQUE (user_id, email), so this is not a
// preference — one of these has to be chosen before anything can be written.
type ConflictStrategy string

const (
	// ConflictAbort cancels the whole import. Nothing is written: not the
	// account, not the calendar source, not the contacts. The database is left
	// exactly as it was.
	ConflictAbort ConflictStrategy = "abort"

	// ConflictReplace updates the existing account in place, keeping its id —
	// and with it every folder, message and linked source already hanging off
	// that id. Deleting and re-creating would cascade all of it away.
	ConflictReplace ConflictStrategy = "replace"

	// ConflictRename imports as a separate account under a different address,
	// leaving the existing one untouched.
	ConflictRename ConflictStrategy = "rename"
)

// ImportPlan is a resolved intent to import: what to write, and what to do
// about the account that is already there.
type ImportPlan struct {
	UserID int64

	// Identity every created row is attributed to. Required — the desktop
	// client is identity-keyed and this server has no orphan sources.
	IdentityEmail string

	Name string

	Account        *models.Account
	CalendarSource *models.CalendarSource
	ContactSource  *models.ContactSource

	Strategy ConflictStrategy

	// RenameEmail is the address to import under when Strategy is
	// ConflictRename.
	RenameEmail string
}

// ImportResult reports what an applied plan actually did.
type ImportResult struct {
	AccountID        int64  `json:"account_id,omitempty"`
	AccountAction    string `json:"account_action"` // "created", "replaced", "skipped"
	CalendarSourceID int64  `json:"calendar_source_id,omitempty"`
	ContactSourceID  int64  `json:"contact_source_id,omitempty"`
	IdentityEmail    string `json:"identity_email"`
}

// ErrImportAborted is returned when a conflict was found and the chosen
// strategy was to abort. Nothing was written.
var ErrImportAborted = fmt.Errorf("import aborted: an account with this address already exists")

// FindAccountByEmail returns the user's account for an address, or nil. Used to
// detect the conflict before anything is written, so the question reaches the
// user while the answer can still change the outcome.
func (db *DB) FindAccountByEmail(userID int64, email string) (*models.Account, error) {
	accounts, err := db.GetAccountsByUserID(userID)
	if err != nil {
		return nil, err
	}
	for _, a := range accounts {
		if strings.EqualFold(a.Email, email) {
			return a, nil
		}
	}
	return nil, nil
}

// ApplyImportPlan writes a parsed profile.
//
// Everything happens in one transaction. The promise this keeps is that a
// profile never lands half-applied: no calendar without its mailbox, no
// contacts pointing at an account that failed to insert. On any error — a
// conflict resolved as abort, a constraint, a dropped connection — the whole
// thing rolls back and the database is byte-for-byte as it was.
func (db *DB) ApplyImportPlan(plan *ImportPlan) (*ImportResult, error) {
	if plan.IdentityEmail == "" {
		return nil, fmt.Errorf("identity email is required")
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	wrapped := &Tx{Tx: tx, encryptionKey: db.encryptionKey}
	result := &ImportResult{
		IdentityEmail: plan.IdentityEmail,
		AccountAction: "skipped",
	}

	if plan.Account != nil {
		accountID, action, err := applyAccount(wrapped, db, plan)
		if err != nil {
			return nil, err
		}
		result.AccountID = accountID
		result.AccountAction = action
	}

	// Sources link to the account when there is one, so invites can be sent
	// from the right mailbox.
	var accountRef *int64
	if result.AccountID != 0 {
		id := result.AccountID
		accountRef = &id
	}

	if plan.CalendarSource != nil {
		plan.CalendarSource.UserID = plan.UserID
		plan.CalendarSource.IdentityEmail = plan.IdentityEmail
		plan.CalendarSource.AccountID = accountRef
		if err := wrapped.CreateCalendarSource(plan.CalendarSource); err != nil {
			return nil, fmt.Errorf("failed to create calendar source: %w", err)
		}
		result.CalendarSourceID = plan.CalendarSource.ID
	}

	if plan.ContactSource != nil {
		plan.ContactSource.UserID = plan.UserID
		plan.ContactSource.IdentityEmail = plan.IdentityEmail
		if err := wrapped.CreateContactSource(plan.ContactSource); err != nil {
			return nil, fmt.Errorf("failed to create contact source: %w", err)
		}
		result.ContactSourceID = plan.ContactSource.ID
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit import: %w", err)
	}

	return result, nil
}

// applyAccount resolves the conflict and writes the account row.
func applyAccount(tx *Tx, db *DB, plan *ImportPlan) (int64, string, error) {
	plan.Account.UserID = plan.UserID

	existing, err := db.FindAccountByEmail(plan.UserID, plan.Account.Email)
	if err != nil {
		return 0, "", fmt.Errorf("failed to check for an existing account: %w", err)
	}

	if existing == nil {
		if err := tx.CreateAccount(plan.Account); err != nil {
			return 0, "", fmt.Errorf("failed to create account: %w", err)
		}
		return plan.Account.ID, "created", nil
	}

	switch plan.Strategy {
	case ConflictReplace:
		// Keep the row and its id. Folders, messages and any calendar source
		// already pointing at this account survive; a delete-and-recreate
		// would cascade every one of them away, which is not what "replace the
		// account settings" means to anyone.
		plan.Account.ID = existing.ID
		plan.Account.CreatedAt = existing.CreatedAt
		if plan.Account.Aliases == "" {
			plan.Account.Aliases = existing.Aliases
		}
		if err := tx.UpdateAccount(plan.Account); err != nil {
			return 0, "", fmt.Errorf("failed to replace account: %w", err)
		}
		return existing.ID, "replaced", nil

	case ConflictRename:
		newEmail := strings.TrimSpace(plan.RenameEmail)
		if newEmail == "" {
			return 0, "", fmt.Errorf("rename was chosen but no new address was given")
		}
		if strings.EqualFold(newEmail, existing.Email) {
			return 0, "", fmt.Errorf("the new address is the same as the existing one")
		}
		// The new address must be free too, or the insert simply fails on the
		// same constraint one step later.
		clash, err := db.FindAccountByEmail(plan.UserID, newEmail)
		if err != nil {
			return 0, "", fmt.Errorf("failed to check the new address: %w", err)
		}
		if clash != nil {
			return 0, "", fmt.Errorf("an account for %s already exists too", newEmail)
		}

		plan.Account.Email = newEmail
		if err := tx.CreateAccount(plan.Account); err != nil {
			return 0, "", fmt.Errorf("failed to create renamed account: %w", err)
		}
		return plan.Account.ID, "created", nil

	default:
		// Abort, and anything unrecognised: refuse rather than guess which of
		// the user's accounts to overwrite.
		return 0, "", ErrImportAborted
	}
}

// UpdateAccount updates an account inside a transaction.
func (tx *Tx) UpdateAccount(account *models.Account) error {
	return updateAccount(tx, tx.encryptionKey, account)
}
