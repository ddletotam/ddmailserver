package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// CreateUser creates a new user with recovery key. The first user ever created
// on the installation is automatically promoted to admin so a fresh deployment
// has someone who can configure OAuth and manage other users.
func (db *DB) CreateUser(username, passwordHash, email, recoveryKeyHash string) (*models.User, error) {
	user := &models.User{
		Username:        username,
		PasswordHash:    passwordHash,
		Email:           email,
		RecoveryKeyHash: recoveryKeyHash,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}

	var emailVal sql.NullString
	if email != "" {
		emailVal = sql.NullString{String: email, Valid: true}
	}

	// First-user-as-admin: derive is_admin from a SELECT inside the same statement
	// so two concurrent first-time registrations can't both end up admin.
	query := `
		INSERT INTO users (username, password_hash, email, recovery_key_hash, is_admin, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NOT EXISTS (SELECT 1 FROM users), $5, $6)
		RETURNING id, is_admin
	`

	err := db.QueryRow(query, user.Username, user.PasswordHash, emailVal, user.RecoveryKeyHash, user.CreatedAt, user.UpdatedAt).Scan(&user.ID, &user.IsAdminFlag)
	if err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	return user, nil
}

// GetUserByUsername retrieves a user by username
func (db *DB) GetUserByUsername(username string) (*models.User, error) {
	user := &models.User{}
	query := `
		SELECT id, username, password_hash, email, language, recovery_key_hash, COALESCE(is_admin, false), COALESCE(is_banned, false), created_at, updated_at
		FROM users
		WHERE username = $1
	`

	var email, language sql.NullString
	err := db.QueryRow(query, username).Scan(
		&user.ID, &user.Username, &user.PasswordHash, &email, &language, &user.RecoveryKeyHash, &user.IsAdminFlag, &user.IsBannedFlag, &user.CreatedAt, &user.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("user not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}

	if email.Valid {
		user.Email = email.String
	}
	if language.Valid {
		user.Language = language.String
	}

	return user, nil
}

// GetUserByID retrieves a user by ID
func (db *DB) GetUserByID(id int64) (*models.User, error) {
	user := &models.User{}
	query := `
		SELECT id, username, password_hash, email, language, recovery_key_hash, COALESCE(is_admin, false), COALESCE(is_banned, false), created_at, updated_at
		FROM users
		WHERE id = $1
	`

	var email, language sql.NullString
	err := db.QueryRow(query, id).Scan(
		&user.ID, &user.Username, &user.PasswordHash, &email, &language, &user.RecoveryKeyHash, &user.IsAdminFlag, &user.IsBannedFlag, &user.CreatedAt, &user.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("user not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}

	if email.Valid {
		user.Email = email.String
	}
	if language.Valid {
		user.Language = language.String
	}

	return user, nil
}

// UpdateUser updates user information
func (db *DB) UpdateUser(user *models.User) error {
	user.UpdatedAt = time.Now()
	query := `
		UPDATE users
		SET username = $1, password_hash = $2, email = $3, recovery_key_hash = $4, updated_at = $5
		WHERE id = $6
	`

	var emailVal sql.NullString
	if user.Email != "" {
		emailVal = sql.NullString{String: user.Email, Valid: true}
	}

	_, err := db.Exec(query, user.Username, user.PasswordHash, emailVal, user.RecoveryKeyHash, user.UpdatedAt, user.ID)
	if err != nil {
		return fmt.Errorf("failed to update user: %w", err)
	}

	return nil
}

// UpdatePasswordByRecoveryKey updates password using recovery key
func (db *DB) UpdatePasswordByRecoveryKey(username, newPasswordHash string) error {
	query := `
		UPDATE users
		SET password_hash = $1, updated_at = $2
		WHERE username = $3
	`

	_, err := db.Exec(query, newPasswordHash, time.Now(), username)
	if err != nil {
		return fmt.Errorf("failed to update password: %w", err)
	}

	return nil
}

// UpdatePassword updates user's password
func (db *DB) UpdatePassword(userID int64, newPasswordHash string) error {
	query := `
		UPDATE users
		SET password_hash = $1, updated_at = $2
		WHERE id = $3
	`

	_, err := db.Exec(query, newPasswordHash, time.Now(), userID)
	if err != nil {
		return fmt.Errorf("failed to update password: %w", err)
	}

	return nil
}

// UpdateLanguage updates user's language preference
func (db *DB) UpdateLanguage(userID int64, language string) error {
	query := `
		UPDATE users
		SET language = $1, updated_at = $2
		WHERE id = $3
	`

	_, err := db.Exec(query, language, time.Now(), userID)
	if err != nil {
		return fmt.Errorf("failed to update language: %w", err)
	}

	return nil
}

// DeleteUser deletes a user and all associated data
func (db *DB) DeleteUser(id int64) error {
	// Delete user (CASCADE should handle related data)
	query := `DELETE FROM users WHERE id = $1`
	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete user: %w", err)
	}
	return nil
}

// ListUsers returns all users ordered by id. Admin-only consumer.
func (db *DB) ListUsers() ([]*models.User, error) {
	query := `
		SELECT id, username, email, language, COALESCE(is_admin, false), COALESCE(is_banned, false), created_at, updated_at
		FROM users
		ORDER BY id ASC
	`
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to list users: %w", err)
	}
	defer rows.Close()

	var users []*models.User
	for rows.Next() {
		u := &models.User{}
		var email, language sql.NullString
		if err := rows.Scan(&u.ID, &u.Username, &email, &language, &u.IsAdminFlag, &u.IsBannedFlag, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan user: %w", err)
		}
		if email.Valid {
			u.Email = email.String
		}
		if language.Valid {
			u.Language = language.String
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration: %w", err)
	}
	return users, nil
}

// SetUserAdmin flips the admin flag on a user.
func (db *DB) SetUserAdmin(id int64, isAdmin bool) error {
	_, err := db.Exec(`UPDATE users SET is_admin = $1, updated_at = $2 WHERE id = $3`, isAdmin, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to update is_admin: %w", err)
	}
	return nil
}

// SetUserBanned flips the banned flag on a user. A banned user can't log in
// but their data is preserved (use DeleteUser to wipe).
func (db *DB) SetUserBanned(id int64, isBanned bool) error {
	_, err := db.Exec(`UPDATE users SET is_banned = $1, updated_at = $2 WHERE id = $3`, isBanned, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to update is_banned: %w", err)
	}
	return nil
}

// CountAdmins returns how many active admins exist. Used to refuse demoting/
// deleting the last admin so the installation never ends up unmanageable.
func (db *DB) CountAdmins() (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE COALESCE(is_admin, false) = true AND COALESCE(is_banned, false) = false`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("failed to count admins: %w", err)
	}
	return n, nil
}
