package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// CreateCalendar creates a new calendar
func (db *DB) CreateCalendar(cal *models.Calendar) error {
	cal.CreatedAt = time.Now()
	cal.UpdatedAt = time.Now()

	var remoteID sql.NullString
	if cal.RemoteID != "" {
		remoteID = sql.NullString{String: cal.RemoteID, Valid: true}
	}

	query := `
		INSERT INTO calendars (
			source_id, user_id, remote_id, name, description, color, timezone, ctag, can_write,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id
	`

	err := db.QueryRow(
		query,
		cal.SourceID, cal.UserID, remoteID, cal.Name, cal.Description,
		cal.Color, cal.Timezone, cal.CTag, cal.CanWrite,
		cal.CreatedAt, cal.UpdatedAt,
	).Scan(&cal.ID)

	if err != nil {
		return fmt.Errorf("failed to create calendar: %w", err)
	}

	return nil
}

// GetCalendarsByUserID retrieves all calendars for a user
func (db *DB) GetCalendarsByUserID(userID int64) ([]*models.Calendar, error) {
	query := `
		SELECT c.id, c.source_id, c.user_id, COALESCE(c.remote_id, ''), c.name,
		       COALESCE(c.description, ''), COALESCE(c.color, s.color), c.timezone,
		       COALESCE(c.ctag, ''), c.can_write, c.created_at, c.updated_at,
		       s.source_type
		FROM calendars c
		JOIN calendar_sources s ON c.source_id = s.id
		WHERE c.user_id = $1
		ORDER BY c.created_at DESC
	`

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
			&cal.CTag, &cal.CanWrite, &cal.CreatedAt, &cal.UpdatedAt,
			&cal.SourceType,
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
		       COALESCE(c.ctag, ''), c.can_write, c.created_at, c.updated_at,
		       s.source_type
		FROM calendars c
		JOIN calendar_sources s ON c.source_id = s.id
		WHERE c.id = $1
	`

	err := db.QueryRow(query, id).Scan(
		&cal.ID, &cal.SourceID, &cal.UserID, &cal.RemoteID, &cal.Name,
		&cal.Description, &cal.Color, &cal.Timezone,
		&cal.CTag, &cal.CanWrite, &cal.CreatedAt, &cal.UpdatedAt,
		&cal.SourceType,
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
		       COALESCE(c.ctag, ''), c.can_write, c.created_at, c.updated_at,
		       s.source_type
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
			&cal.CTag, &cal.CanWrite, &cal.CreatedAt, &cal.UpdatedAt,
			&cal.SourceType,
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
		       COALESCE(c.ctag, ''), c.can_write, c.created_at, c.updated_at,
		       s.source_type
		FROM calendars c
		JOIN calendar_sources s ON c.source_id = s.id
		WHERE c.source_id = $1 AND c.remote_id = $2
	`

	err := db.QueryRow(query, sourceID, remoteID).Scan(
		&cal.ID, &cal.SourceID, &cal.UserID, &cal.RemoteID, &cal.Name,
		&cal.Description, &cal.Color, &cal.Timezone,
		&cal.CTag, &cal.CanWrite, &cal.CreatedAt, &cal.UpdatedAt,
		&cal.SourceType,
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
	cal.UpdatedAt = time.Now()

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
	_, err := db.Exec(query, ctag, time.Now(), calendarID)
	if err != nil {
		return fmt.Errorf("failed to update calendar ctag: %w", err)
	}
	return nil
}
