package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// CreateUser creates a new user
func (db *DB) CreateUser(username, passwordHash, email string) (*models.User, error) {
	user := &models.User{
		Username:     username,
		PasswordHash: passwordHash,
		Email:        email,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	query := `
		INSERT INTO users (username, password_hash, email, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`

	err := db.QueryRow(query, user.Username, user.PasswordHash, user.Email, user.CreatedAt, user.UpdatedAt).Scan(&user.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	return user, nil
}

// GetUserByUsername retrieves a user by username
func (db *DB) GetUserByUsername(username string) (*models.User, error) {
	user := &models.User{}
	query := `
		SELECT id, username, password_hash, email, created_at, updated_at
		FROM users
		WHERE username = $1
	`

	err := db.QueryRow(query, username).Scan(
		&user.ID, &user.Username, &user.PasswordHash, &user.Email, &user.CreatedAt, &user.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("user not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}

	return user, nil
}

// GetUserByID retrieves a user by ID
func (db *DB) GetUserByID(id int64) (*models.User, error) {
	user := &models.User{}
	query := `
		SELECT id, username, password_hash, email, created_at, updated_at
		FROM users
		WHERE id = $1
	`

	err := db.QueryRow(query, id).Scan(
		&user.ID, &user.Username, &user.PasswordHash, &user.Email, &user.CreatedAt, &user.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("user not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}

	return user, nil
}

// UpdateUser updates user information
func (db *DB) UpdateUser(user *models.User) error {
	user.UpdatedAt = time.Now()
	query := `
		UPDATE users
		SET username = $1, password_hash = $2, email = $3, updated_at = $4
		WHERE id = $5
	`

	_, err := db.Exec(query, user.Username, user.PasswordHash, user.Email, user.UpdatedAt, user.ID)
	if err != nil {
		return fmt.Errorf("failed to update user: %w", err)
	}

	return nil
}

// DeleteUser deletes a user
func (db *DB) DeleteUser(id int64) error {
	query := `DELETE FROM users WHERE id = $1`
	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete user: %w", err)
	}
	return nil
}
