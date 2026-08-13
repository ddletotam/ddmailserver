package db

import (
	"database/sql"
	"fmt"

	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// CreateCalendar creates a new calendar
func (db *DB) CreateCalendar(cal *models.Calendar) error {
	cal.CreatedAt = timeutil.Now()
	cal.UpdatedAt = timeutil.Now()

	var remoteID sql.NullString
	if cal.RemoteID != "" {
		remoteID = sql.NullString{String: cal.RemoteID, Valid: true}
	}

	query := `
		INSERT INTO calendars (
			source_id, user_id, remote_id, name, description, color, timezone, ctag, can_write,
			reverse_sync, enabled, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id
	`

	err := db.QueryRow(
		query,
		cal.SourceID, cal.UserID, remoteID, cal.Name, cal.Description,
		cal.Color, cal.Timezone, cal.CTag, cal.CanWrite,
		cal.ReverseSync, cal.Enabled, cal.CreatedAt, cal.UpdatedAt,
	).Scan(&cal.ID)

	if err != nil {
		return fmt.Errorf("failed to create calendar: %w", err)
	}

	return nil
}

// GetCalendarsByUserID retrieves all calendars for a user
func (db *DB) GetCalendarsByUserID(userID int64) ([]*models.Calendar, error) {
	return db.GetCalendarsByUserIDFiltered(userID, false)
}

// SetCalendarEnabled flips a calendar's master switch.
//
// `enabled` is the whole of it: a disabled calendar is not handed to clients, is
// not synced with its source, and pushes nothing back to it. So this is the only
// place that needs to change for a calendar to go quiet, and it is deliberately
// separate from UpdateCalendar, which rewrites presentation fields and must not
// be able to turn a calendar off as a side effect.
//
// Scoped by user so a guessed id cannot reach somebody else's calendar.
func (db *DB) SetCalendarEnabled(id, userID int64, enabled bool) error {
	result, err := db.Exec(
		`UPDATE calendars SET enabled = $1, updated_at = $2 WHERE id = $3 AND user_id = $4`,
		enabled, timeutil.Now(), id, userID,
	)
	if err != nil {
		return fmt.Errorf("failed to set calendar enabled: %w", err)
	}
	if affected, err := result.RowsAffected(); err == nil && affected == 0 {
		return fmt.Errorf("calendar %d not found for this user", id)
	}
	return nil
}

// GetEnabledCalendarsByUserID retrieves only enabled calendars for a user
func (db *DB) GetEnabledCalendarsByUserID(userID int64) ([]*models.Calendar, error) {
	return db.GetCalendarsByUserIDFiltered(userID, true)
}

// GetCalendarsByUserIDFiltered retrieves calendars for a user with optional enabled filter
func (db *DB) GetCalendarsByUserIDFiltered(userID int64, enabledOnly bool) ([]*models.Calendar, error) {
	query := `
		SELECT c.id, c.source_id, c.user_id, COALESCE(c.remote_id, ''), c.name,
		       COALESCE(c.description, ''), COALESCE(c.color, s.color), c.timezone,
		       COALESCE(c.ctag, ''), c.can_write, COALESCE(c.reverse_sync, false),
		       COALESCE(c.enabled, true), c.created_at, c.updated_at, s.source_type,
		       COALESCE(s.identity_email, '')
		FROM calendars c
		JOIN calendar_sources s ON c.source_id = s.id
		WHERE c.user_id = $1
	`
	if enabledOnly {
		query += " AND COALESCE(c.enabled, true) = true"
	}
	query += " ORDER BY c.created_at DESC"

	rows, err := db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get calendars: %w", err)
	}
	defer rows.Close()

	var calendars []*models.Calendar
	for rows.Next() {
		cal := &models.Calendar{}

		err := rows.Scan(
			&cal.ID, &cal.SourceID, &cal.UserID, &cal.RemoteID, &cal.Name,
			&cal.Description, &cal.Color, &cal.Timezone,
			&cal.CTag, &cal.CanWrite, &cal.ReverseSync,
			&cal.Enabled, &cal.CreatedAt, &cal.UpdatedAt, &cal.SourceType,
			&cal.IdentityEmail,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan calendar: %w", err)
		}

		calendars = append(calendars, cal)
	}

	return calendars, nil
}

// GetCalendarByID retrieves a calendar by ID
func (db *DB) GetCalendarByID(id int64) (*models.Calendar, error) {
	cal := &models.Calendar{}

	query := `
		SELECT c.id, c.source_id, c.user_id, COALESCE(c.remote_id, ''), c.name,
		       COALESCE(c.description, ''), COALESCE(c.color, s.color), c.timezone,
		       COALESCE(c.ctag, ''), c.can_write, COALESCE(c.reverse_sync, false),
		       COALESCE(c.enabled, true), c.created_at, c.updated_at, s.source_type,
		       COALESCE(s.identity_email, '')
		FROM calendars c
		JOIN calendar_sources s ON c.source_id = s.id
		WHERE c.id = $1
	`

	err := db.QueryRow(query, id).Scan(
		&cal.ID, &cal.SourceID, &cal.UserID, &cal.RemoteID, &cal.Name,
		&cal.Description, &cal.Color, &cal.Timezone,
		&cal.CTag, &cal.CanWrite, &cal.ReverseSync,
		&cal.Enabled, &cal.CreatedAt, &cal.UpdatedAt, &cal.SourceType,
		&cal.IdentityEmail,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("calendar not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get calendar: %w", err)
	}

	return cal, nil
}

// GetCalendarsBySourceID retrieves all calendars for a source
func (db *DB) GetCalendarsBySourceID(sourceID int64) ([]*models.Calendar, error) {
	query := `
		SELECT c.id, c.source_id, c.user_id, COALESCE(c.remote_id, ''), c.name,
		       COALESCE(c.description, ''), COALESCE(c.color, s.color), c.timezone,
		       COALESCE(c.ctag, ''), c.can_write, COALESCE(c.reverse_sync, false),
		       COALESCE(c.enabled, true), c.created_at, c.updated_at, s.source_type
		FROM calendars c
		JOIN calendar_sources s ON c.source_id = s.id
		WHERE c.source_id = $1
		ORDER BY c.name
	`

	rows, err := db.Query(query, sourceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get calendars: %w", err)
	}
	defer rows.Close()

	var calendars []*models.Calendar
	for rows.Next() {
		cal := &models.Calendar{}

		err := rows.Scan(
			&cal.ID, &cal.SourceID, &cal.UserID, &cal.RemoteID, &cal.Name,
			&cal.Description, &cal.Color, &cal.Timezone,
			&cal.CTag, &cal.CanWrite, &cal.ReverseSync,
			&cal.Enabled, &cal.CreatedAt, &cal.UpdatedAt, &cal.SourceType,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan calendar: %w", err)
		}

		calendars = append(calendars, cal)
	}

	return calendars, nil
}

// GetCalendarByRemoteID retrieves a calendar by source and remote ID
func (db *DB) GetCalendarByRemoteID(sourceID int64, remoteID string) (*models.Calendar, error) {
	cal := &models.Calendar{}

	query := `
		SELECT c.id, c.source_id, c.user_id, COALESCE(c.remote_id, ''), c.name,
		       COALESCE(c.description, ''), COALESCE(c.color, s.color), c.timezone,
		       COALESCE(c.ctag, ''), c.can_write, COALESCE(c.reverse_sync, false),
		       COALESCE(c.enabled, true), c.created_at, c.updated_at,
		       s.source_type, COALESCE(s.identity_email, '')
		FROM calendars c
		JOIN calendar_sources s ON c.source_id = s.id
		WHERE c.source_id = $1 AND c.remote_id = $2
	`

	err := db.QueryRow(query, sourceID, remoteID).Scan(
		&cal.ID, &cal.SourceID, &cal.UserID, &cal.RemoteID, &cal.Name,
		&cal.Description, &cal.Color, &cal.Timezone,
		&cal.CTag, &cal.CanWrite, &cal.ReverseSync,
		&cal.Enabled, &cal.CreatedAt, &cal.UpdatedAt,
		&cal.SourceType, &cal.IdentityEmail,
	)

	if err == sql.ErrNoRows {
		return nil, nil // Not found, return nil without error
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get calendar: %w", err)
	}

	return cal, nil
}

// UpdateCalendar updates a calendar
func (db *DB) UpdateCalendar(cal *models.Calendar) error {
	cal.UpdatedAt = timeutil.Now()

	query := `
		UPDATE calendars
		SET name = $1, description = $2, color = $3, timezone = $4, ctag = $5,
		    can_write = $6, updated_at = $7
		WHERE id = $8
	`

	_, err := db.Exec(
		query,
		cal.Name, cal.Description, cal.Color, cal.Timezone, cal.CTag,
		cal.CanWrite, cal.UpdatedAt, cal.ID,
	)

	if err != nil {
		return fmt.Errorf("failed to update calendar: %w", err)
	}

	return nil
}

// DeleteCalendar deletes a calendar (cascades to events)
func (db *DB) DeleteCalendar(id int64) error {
	query := `DELETE FROM calendars WHERE id = $1`
	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete calendar: %w", err)
	}
	return nil
}

// UpdateCalendarCTag updates the CTag for sync detection
func (db *DB) UpdateCalendarCTag(calendarID int64, ctag string) error {
	query := `UPDATE calendars SET ctag = $1, updated_at = $2 WHERE id = $3`
	_, err := db.Exec(query, ctag, timeutil.Now(), calendarID)
	if err != nil {
		return fmt.Errorf("failed to update calendar ctag: %w", err)
	}
	return nil
}
