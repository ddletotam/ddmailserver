package db

import (
	"database/sql"
	"fmt"
	"strings"

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
			reverse_sync, enabled, created_at, updated_at, supported_components
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING id
	`

	err := db.QueryRow(
		query,
		cal.SourceID, cal.UserID, remoteID, cal.Name, cal.Description,
		cal.Color, cal.Timezone, cal.CTag, cal.CanWrite,
		cal.ReverseSync, cal.Enabled, cal.CreatedAt, cal.UpdatedAt, supportedComponents(cal),
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
		       COALESCE(s.identity_email, ''), COALESCE(c.supported_components, 'VEVENT')
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
			&cal.IdentityEmail, &cal.SupportedComponents,
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
		       COALESCE(s.identity_email, ''), COALESCE(c.supported_components, 'VEVENT')
		FROM calendars c
		JOIN calendar_sources s ON c.source_id = s.id
		WHERE c.id = $1
	`

	err := db.QueryRow(query, id).Scan(
		&cal.ID, &cal.SourceID, &cal.UserID, &cal.RemoteID, &cal.Name,
		&cal.Description, &cal.Color, &cal.Timezone,
		&cal.CTag, &cal.CanWrite, &cal.ReverseSync,
		&cal.Enabled, &cal.CreatedAt, &cal.UpdatedAt, &cal.SourceType,
		&cal.IdentityEmail, &cal.SupportedComponents,
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
		       s.source_type, COALESCE(s.identity_email, ''), COALESCE(c.supported_components, 'VEVENT')
		FROM calendars c
		JOIN calendar_sources s ON c.source_id = s.id
		WHERE c.source_id = $1 AND c.remote_id = $2
	`

	err := db.QueryRow(query, sourceID, remoteID).Scan(
		&cal.ID, &cal.SourceID, &cal.UserID, &cal.RemoteID, &cal.Name,
		&cal.Description, &cal.Color, &cal.Timezone,
		&cal.CTag, &cal.CanWrite, &cal.ReverseSync,
		&cal.Enabled, &cal.CreatedAt, &cal.UpdatedAt,
		&cal.SourceType, &cal.IdentityEmail, &cal.SupportedComponents,
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

// UpdateCalendarSupportedComponents records what a collection accepts, as
// reported by discovery.
//
// Kept separate from the general calendar update because it is the remote's
// answer, not the user's preference: nothing in the UI should be able to widen
// it. Claiming a component the collection will not store is precisely the
// mistake this column exists to prevent.
func (db *DB) UpdateCalendarSupportedComponents(calendarID int64, components string) error {
	if strings.TrimSpace(components) == "" {
		components = models.ComponentEvent
	}
	_, err := db.Exec(
		`UPDATE calendars SET supported_components = $1, updated_at = $2 WHERE id = $3`,
		components, timeutil.Now(), calendarID,
	)
	if err != nil {
		return fmt.Errorf("failed to update supported components: %w", err)
	}
	return nil
}

// supportedComponents is what a new calendar row records as acceptable.
//
// Defaults to events only rather than to "whatever the caller left empty":
// claiming component support that does not exist is the exact mistake that had
// iOS filing Reminders into an event calendar and the reverse sync pushing them
// at iCloud forever. See migrations/048.
func supportedComponents(cal *models.Calendar) string {
	if strings.TrimSpace(cal.SupportedComponents) == "" {
		return models.ComponentEvent
	}
	return cal.SupportedComponents
}

// PruneDirectURLCalendar removes the placeholder row that discovery falls back
// to when it cannot enumerate a server's collections.
//
// That fallback stores the SOURCE URL as the calendar's remote_id — for a SOGo
// server that is the principal, not a collection, so the row can never sync
// anything. It is correct while discovery is broken (Yandex genuinely needs it,
// and there the direct URL IS a calendar), and becomes dead weight the moment
// discovery starts working: nothing returns it any more, so nothing syncs it,
// and it lingers in the calendar list as a permanently empty entry. Two of
// these accumulated in one day — one calendar, one address book — after a TLS
// fix let discovery through.
//
// Only ever called once discovery has demonstrably succeeded, and only removes
// an EMPTY row. A fallback holding events is the working calendar of a server
// with no discovery at all; deleting that would cascade the events away.
// Returns whether a row was removed.
func (db *DB) PruneDirectURLCalendar(sourceID int64, sourceURL string) (bool, error) {
	if strings.TrimSpace(sourceURL) == "" {
		return false, nil
	}

	var id int64
	var count int
	err := db.QueryRow(`
		SELECT c.id, (SELECT COUNT(*) FROM calendar_events e WHERE e.calendar_id = c.id)
		FROM calendars c
		WHERE c.source_id = $1 AND c.remote_id = $2
	`, sourceID, sourceURL).Scan(&id, &count)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to look up direct-URL calendar: %w", err)
	}

	if count > 0 {
		// Not ours to remove: something is using it.
		return false, nil
	}

	if _, err := db.Exec(`DELETE FROM calendars WHERE id = $1`, id); err != nil {
		return false, fmt.Errorf("failed to delete direct-URL calendar %d: %w", id, err)
	}
	return true, nil
}

// PruneDirectURLCalendarIfDiscovered removes the placeholder row when the
// discovered set shows that discovery is working again.
//
// The decision lives here rather than at the call sites because there are two
// of them — the background worker and the web handler each implement calendar
// sync separately — and "did discovery work?" answered differently in the two
// places is how one of them silently stops cleaning up. That has already
// happened once with this very fix.
//
// Working means: something came back that is not the source URL itself. A
// transient failure returns only the placeholder, and then nothing is removed.
func (db *DB) PruneDirectURLCalendarIfDiscovered(sourceID int64, sourceURL string, discovered []*models.Calendar) (bool, error) {
	worked := false
	for _, c := range discovered {
		if c.RemoteID != sourceURL {
			worked = true
			break
		}
	}
	if !worked {
		return false, nil
	}
	return db.PruneDirectURLCalendar(sourceID, sourceURL)
}
