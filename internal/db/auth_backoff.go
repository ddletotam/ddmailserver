package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/authfail"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// Persistence of the "provider rejects the credentials" state, see
// migrations/052_auth_backoff.sql and package authfail. *DB implements
// authfail.Store.

const authBackoffColumns = `subject_kind, subject_id, user_id, failures, first_failure_at,
	last_failure_at, next_attempt_at, last_error, notified_at`

func scanAuthBackoff(r rowScanner) (*models.AuthBackoff, error) {
	st := &models.AuthBackoff{}
	err := r.Scan(&st.SubjectKind, &st.SubjectID, &st.UserID, &st.Failures, &st.FirstFailureAt,
		&st.LastFailureAt, &st.NextAttemptAt, &st.LastError, &st.NotifiedAt)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// GetAuthBackoff returns the state of one subject, nil when its credentials
// are not being rejected.
func (db *DB) GetAuthBackoff(kind string, id int64) (*models.AuthBackoff, error) {
	st, err := scanAuthBackoff(db.QueryRow(`SELECT `+authBackoffColumns+`
		FROM auth_backoff WHERE subject_kind = $1 AND subject_id = $2`, kind, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get auth backoff: %w", err)
	}
	return st, nil
}

// AcquireAuthAttempt implements authfail.Store.
func (db *DB) AcquireAuthAttempt(kind string, id int64) (bool, *models.AuthBackoff, error) {
	tx, err := db.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return false, nil, fmt.Errorf("failed to begin auth attempt: %w", err)
	}
	defer tx.Rollback()

	st, err := scanAuthBackoff(tx.QueryRow(`SELECT `+authBackoffColumns+`
		FROM auth_backoff WHERE subject_kind = $1 AND subject_id = $2 FOR UPDATE`, kind, id))
	if err == sql.ErrNoRows {
		return true, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("failed to read auth backoff: %w", err)
	}

	now := timeutil.Now()
	if st.Paused(now) {
		return false, st, nil
	}
	st.NextAttemptAt = authfail.RetryLease(st, now)
	if _, err := tx.Exec(`UPDATE auth_backoff SET next_attempt_at = $3
		WHERE subject_kind = $1 AND subject_id = $2`, kind, id, st.NextAttemptAt); err != nil {
		return false, nil, fmt.Errorf("failed to lease auth attempt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, nil, fmt.Errorf("failed to commit auth attempt: %w", err)
	}
	return true, st, nil
}

// RecordAuthFailure implements authfail.Store.
func (db *DB) RecordAuthFailure(kind string, id, userID int64, errMsg string) (*models.AuthBackoff, bool, error) {
	tx, err := db.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, false, fmt.Errorf("failed to begin auth failure: %w", err)
	}
	defer tx.Rollback()

	// Make sure the row exists so that FOR UPDATE has something to lock: two
	// paths failing at once must see each other's write, not both insert.
	// failures = 0 marks it as fresh for AfterFailure.
	if _, err := tx.Exec(`INSERT INTO auth_backoff (subject_kind, subject_id, user_id)
		VALUES ($1, $2, $3) ON CONFLICT (subject_kind, subject_id) DO NOTHING`, kind, id, userID); err != nil {
		return nil, false, fmt.Errorf("failed to insert auth backoff: %w", err)
	}
	prev, err := scanAuthBackoff(tx.QueryRow(`SELECT `+authBackoffColumns+`
		FROM auth_backoff WHERE subject_kind = $1 AND subject_id = $2 FOR UPDATE`, kind, id))
	if err != nil {
		return nil, false, fmt.Errorf("failed to read auth backoff: %w", err)
	}

	st := authfail.AfterFailure(prev, kind, id, userID, timeutil.Now(), errMsg)
	counted := prev.Failures == 0 || st.Failures != prev.Failures
	if prev.Failures == 0 {
		st.NotifiedAt = 0
	}
	if _, err := tx.Exec(`UPDATE auth_backoff
		SET failures = $3, first_failure_at = $4, last_failure_at = $5, next_attempt_at = $6,
		    last_error = $7, notified_at = $8
		WHERE subject_kind = $1 AND subject_id = $2`,
		kind, id, st.Failures, st.FirstFailureAt, st.LastFailureAt, st.NextAttemptAt,
		st.LastError, st.NotifiedAt); err != nil {
		return nil, false, fmt.Errorf("failed to update auth backoff: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("failed to commit auth failure: %w", err)
	}
	return st, counted, nil
}

// ClearAuthBackoff implements authfail.Store.
func (db *DB) ClearAuthBackoff(kind string, id int64) (*models.AuthBackoff, error) {
	st, err := scanAuthBackoff(db.QueryRow(`DELETE FROM auth_backoff
		WHERE subject_kind = $1 AND subject_id = $2
		RETURNING `+authBackoffColumns, kind, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to clear auth backoff: %w", err)
	}
	if st.Failures == 0 {
		// Only the placeholder of a failure being recorded right now.
		return nil, nil
	}
	return st, nil
}

// resetAccountAuthBackoff drops the pause of both logins of an account. Called
// whenever the account is edited: new credentials deserve an attempt now, not
// in an hour. q lets the transactional .mobileconfig import share it.
func resetAccountAuthBackoff(q querier, accountID int64) error {
	if _, err := q.Exec(`DELETE FROM auth_backoff WHERE subject_kind IN ($1, $2) AND subject_id = $3`,
		models.AuthSubjectIMAP, models.AuthSubjectSMTP, accountID); err != nil {
		return fmt.Errorf("failed to reset auth backoff: %w", err)
	}
	return nil
}

// ResetAuthBackoff drops the pause of one subject (source edited).
func (db *DB) ResetAuthBackoff(kind string, id int64) error {
	if _, err := db.Exec(`DELETE FROM auth_backoff WHERE subject_kind = $1 AND subject_id = $2`, kind, id); err != nil {
		return fmt.Errorf("failed to reset auth backoff: %w", err)
	}
	return nil
}

// GetAuthPausedIDs returns the subjects of a kind that may not be attempted
// right now. The scheduler filters with it before submitting anything, so a
// paused account does not even produce a "Submitted …" line per tick.
func (db *DB) GetAuthPausedIDs(kind string) (map[int64]bool, error) {
	rows, err := db.Query(`SELECT subject_id FROM auth_backoff
		WHERE subject_kind = $1 AND next_attempt_at > $2`, kind, timeutil.Now())
	if err != nil {
		return nil, fmt.Errorf("failed to get paused auth subjects: %w", err)
	}
	defer rows.Close()
	ids := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan paused auth subject: %w", err)
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// GetAuthBackoffsByUser returns every rejected subject of a user.
func (db *DB) GetAuthBackoffsByUser(userID int64) ([]*models.AuthBackoff, error) {
	return db.queryAuthBackoffs(`SELECT `+authBackoffColumns+`
		FROM auth_backoff WHERE user_id = $1 AND failures > 0
		ORDER BY first_failure_at`, userID)
}

// GetUnnotifiedAuthBackoffs returns subjects that went into the rejected state
// and whose user has not been told yet.
func (db *DB) GetUnnotifiedAuthBackoffs() ([]*models.AuthBackoff, error) {
	return db.queryAuthBackoffs(`SELECT ` + authBackoffColumns + `
		FROM auth_backoff WHERE notified_at = 0 AND failures > 0
		ORDER BY first_failure_at`)
}

// MarkAuthBackoffNotified records that the user was told about the state that
// started at firstFailureAt. Matching on it keeps a notification from being
// credited to a newer incident that replaced the row in between.
func (db *DB) MarkAuthBackoffNotified(kind string, id, firstFailureAt int64) error {
	if _, err := db.Exec(`UPDATE auth_backoff SET notified_at = $4
		WHERE subject_kind = $1 AND subject_id = $2 AND first_failure_at = $3`,
		kind, id, firstFailureAt, timeutil.Now()); err != nil {
		return fmt.Errorf("failed to mark auth backoff notified: %w", err)
	}
	return nil
}

// CleanupOrphanAuthBackoffs removes states of deleted accounts and sources
// (there is no foreign key to them, see the migration).
func (db *DB) CleanupOrphanAuthBackoffs() (int64, error) {
	res, err := db.Exec(`DELETE FROM auth_backoff b WHERE
		(b.subject_kind IN ($1, $2) AND NOT EXISTS (SELECT 1 FROM accounts a WHERE a.id = b.subject_id))
		OR (b.subject_kind = $3 AND NOT EXISTS (SELECT 1 FROM calendar_sources s WHERE s.id = b.subject_id))
		OR (b.subject_kind = $4 AND NOT EXISTS (SELECT 1 FROM contact_sources s WHERE s.id = b.subject_id))`,
		models.AuthSubjectIMAP, models.AuthSubjectSMTP, models.AuthSubjectCalDAV, models.AuthSubjectCardDAV)
	if err != nil {
		return 0, fmt.Errorf("failed to clean up auth backoff: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (db *DB) queryAuthBackoffs(query string, args ...interface{}) ([]*models.AuthBackoff, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query auth backoff: %w", err)
	}
	defer rows.Close()
	var out []*models.AuthBackoff
	for rows.Next() {
		st, err := scanAuthBackoff(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan auth backoff: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// AttachAccountAuthFailures fills Account.AuthFailures for display.
func (db *DB) AttachAccountAuthFailures(userID int64, accounts []*models.Account) error {
	states, err := db.GetAuthBackoffsByUser(userID)
	if err != nil {
		return err
	}
	byID := make(map[int64]*models.Account, len(accounts))
	for _, a := range accounts {
		a.AuthFailures = nil
		byID[a.ID] = a
	}
	for _, st := range states {
		if st.SubjectKind != models.AuthSubjectIMAP && st.SubjectKind != models.AuthSubjectSMTP {
			continue
		}
		if a := byID[st.SubjectID]; a != nil {
			a.AuthFailures = append(a.AuthFailures, st.View())
		}
	}
	return nil
}

// AttachCalendarSourceAuthFailures fills CalendarSource.AuthFailure.
func (db *DB) AttachCalendarSourceAuthFailures(userID int64, sources []*models.CalendarSource) error {
	states, err := db.GetAuthBackoffsByUser(userID)
	if err != nil {
		return err
	}
	for _, s := range sources {
		s.AuthFailure = nil
		for _, st := range states {
			if st.SubjectKind == models.AuthSubjectCalDAV && st.SubjectID == s.ID {
				v := st.View()
				s.AuthFailure = &v
			}
		}
	}
	return nil
}

// AttachContactSourceAuthFailures fills ContactSource.AuthFailure.
func (db *DB) AttachContactSourceAuthFailures(userID int64, sources []*models.ContactSource) error {
	states, err := db.GetAuthBackoffsByUser(userID)
	if err != nil {
		return err
	}
	for _, s := range sources {
		s.AuthFailure = nil
		for _, st := range states {
			if st.SubjectKind == models.AuthSubjectCardDAV && st.SubjectID == s.ID {
				v := st.View()
				s.AuthFailure = &v
			}
		}
	}
	return nil
}
