package db

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"
)

// Queryable is an interface that both *sql.DB and *sql.Tx implement
type Queryable interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
	Query(query string, args ...interface{}) (*sql.Rows, error)
	QueryRow(query string, args ...interface{}) *sql.Row
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

type DB struct {
	*sql.DB
	encryptionKey string
}

// Tx wraps a sql.Tx with encryption key for DB operations
type Tx struct {
	*sql.Tx
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

// BeginTx starts a transaction with context
func (db *DB) BeginTx(ctx context.Context) (*Tx, error) {
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	return &Tx{Tx: tx, encryptionKey: db.encryptionKey}, nil
}

// EncryptionKey returns the encryption key for the transaction
func (tx *Tx) EncryptionKey() string {
	return tx.encryptionKey
}
