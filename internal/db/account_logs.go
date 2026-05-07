package db

import (
	"fmt"

	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// AddAccountLog writes a log entry for an account
func (db *DB) AddAccountLog(accountID int64, level, message string) error {
	query := `INSERT INTO account_logs (account_id, level, message, created_at) VALUES ($1, $2, $3, $4)`
	_, err := db.Exec(query, accountID, level, message, timeutil.Now())
	if err != nil {
		return fmt.Errorf("failed to add account log: %w", err)
	}
	return nil
}

// GetAccountLogs retrieves log entries for an account, newest first.
// If errorsOnly is true, only "error" level entries are returned.
func (db *DB) GetAccountLogs(accountID int64, errorsOnly bool, limit int) ([]*models.AccountLog, error) {
	query := `SELECT id, account_id, level, message, created_at FROM account_logs WHERE account_id = $1`
	if errorsOnly {
		query += ` AND level = 'error'`
	}
	query += ` ORDER BY created_at DESC LIMIT $2`

	rows, err := db.Query(query, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get account logs: %w", err)
	}
	defer rows.Close()

	var logs []*models.AccountLog
	for rows.Next() {
		l := &models.AccountLog{}
		if err := rows.Scan(&l.ID, &l.AccountID, &l.Level, &l.Message, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan account log: %w", err)
		}
		logs = append(logs, l)
	}
	return logs, nil
}

// CleanupAccountLogs deletes log entries older than the given number of days.
func (db *DB) CleanupAccountLogs(days int) (int64, error) {
	cutoffMs := timeutil.Now() - int64(days)*24*60*60*1000
	result, err := db.Exec(`DELETE FROM account_logs WHERE created_at < $1`, cutoffMs)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup account logs: %w", err)
	}
	return result.RowsAffected()
}
