package db

import (
	"database/sql"
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// CreateAdmin creates an administrator account: it manages the server in
// /admin and has no mail of its own (migration 061). passwordHash is a
// bcrypt hash. recovery_key_hash is ” rather than NULL: the user queries scan
// it into a string, and a NULL there made every lookup of the account fail.
func (db *DB) CreateAdmin(username, passwordHash string) (*models.User, error) {
	now := timeutil.Now()
	u := &models.User{Username: username, PasswordHash: passwordHash, IsAdminFlag: true, CreatedAt: now, UpdatedAt: now}
	err := db.QueryRow(`
		INSERT INTO users (username, password_hash, recovery_key_hash, is_admin, created_at, updated_at)
		VALUES ($1, $2, '', true, $3, $3)
		RETURNING id`, username, passwordHash, now).Scan(&u.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to create admin: %w", err)
	}
	return u, nil
}

// AdminAction is one row of the administrator audit log.
type AdminAction struct {
	ID         int64
	AdminID    int64 // 0 — the administrator account was deleted since
	AdminName  string
	Action     string
	TargetType string
	TargetID   int64 // 0 — no target
	Details    string
	IP         string
	CreatedAt  int64
}

// LogAdminAction records what an administrator did.
func (db *DB) LogAdminAction(a AdminAction) error {
	var target sql.NullInt64
	if a.TargetID != 0 {
		target = sql.NullInt64{Int64: a.TargetID, Valid: true}
	}
	if a.CreatedAt == 0 {
		a.CreatedAt = timeutil.Now()
	}
	_, err := db.Exec(`
		INSERT INTO admin_audit_log (admin_id, admin_name, action, target_type, target_id, details, ip, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		a.AdminID, a.AdminName, a.Action, a.TargetType, target, a.Details, a.IP, a.CreatedAt)
	if err != nil {
		return fmt.Errorf("failed to log admin action: %w", err)
	}
	return nil
}

// ListAdminActions returns the newest entries of the audit log.
func (db *DB) ListAdminActions(limit int) ([]AdminAction, error) {
	rows, err := db.Query(`
		SELECT id, COALESCE(admin_id, 0), admin_name, action, target_type, COALESCE(target_id, 0), details, ip, created_at
		FROM admin_audit_log ORDER BY created_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list admin actions: %w", err)
	}
	defer rows.Close()
	var out []AdminAction
	for rows.Next() {
		var a AdminAction
		if err := rows.Scan(&a.ID, &a.AdminID, &a.AdminName, &a.Action, &a.TargetType, &a.TargetID, &a.Details, &a.IP, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan admin action: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
