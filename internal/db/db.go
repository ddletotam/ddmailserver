package db

import (
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"
)

type DB struct {
	*sql.DB
	encryptionKey string
}

// Connect establishes a connection to PostgreSQL
func Connect(dsn string) (*DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Test the connection
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Set connection pool settings
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)

	return &DB{DB: db}, nil
}

// Close closes the database connection
func (db *DB) Close() error {
	return db.DB.Close()
}

// SetEncryptionKey sets the encryption key for encrypting/decrypting passwords
func (db *DB) SetEncryptionKey(key string) {
	db.encryptionKey = key
}

// RunMigrations applies database migrations
func (db *DB) RunMigrations() error {
	// TODO: Implement proper migration runner
	// For now, we'll manually run the SQL files
	return nil
}
