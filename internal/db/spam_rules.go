package db

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// SpamRule represents a user-defined spam rule (whitelist/blacklist)
type SpamRule struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	RuleType  string `json:"rule_type"`  // 'address', 'domain'
	RuleValue string `json:"rule_value"` // email or domain
	Action    string `json:"action"`     // 'spam', 'allow'
	CreatedAt int64  `json:"created_at"`
}

// DisabledSpamCheck represents a disabled system spam check for a user
type DisabledSpamCheck struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	CheckName  string `json:"check_name"`
	DisabledAt int64  `json:"disabled_at"`
}

// CreateSpamRule creates a new spam rule
func (db *DB) CreateSpamRule(rule *SpamRule) error {
	rule.CreatedAt = timeutil.Now()
	rule.RuleValue = strings.ToLower(rule.RuleValue)

	query := `
		INSERT INTO user_spam_rules (user_id, rule_type, rule_value, action, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, rule_type, rule_value) DO UPDATE SET
			action = EXCLUDED.action
		RETURNING id
	`

	err := db.QueryRow(query, rule.UserID, rule.RuleType, rule.RuleValue, rule.Action, rule.CreatedAt).Scan(&rule.ID)
	if err != nil {
		return fmt.Errorf("failed to create spam rule: %w", err)
	}

	return nil
}

// GetSpamRulesByUserID retrieves all spam rules for a user
func (db *DB) GetSpamRulesByUserID(userID int64) ([]*SpamRule, error) {
	query := `
		SELECT id, user_id, rule_type, rule_value, action, created_at
		FROM user_spam_rules
		WHERE user_id = $1
		ORDER BY action DESC, rule_type, rule_value
	`

	rows, err := db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get spam rules: %w", err)
	}
	defer rows.Close()

	var rules []*SpamRule
	for rows.Next() {
		rule := &SpamRule{}
		err := rows.Scan(&rule.ID, &rule.UserID, &rule.RuleType, &rule.RuleValue, &rule.Action, &rule.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan spam rule: %w", err)
		}
		rules = append(rules, rule)
	}

	return rules, nil
}

// GetSpamRuleByID retrieves a spam rule by ID
func (db *DB) GetSpamRuleByID(id int64) (*SpamRule, error) {
	query := `
		SELECT id, user_id, rule_type, rule_value, action, created_at
		FROM user_spam_rules
		WHERE id = $1
	`

	rule := &SpamRule{}
	err := db.QueryRow(query, id).Scan(&rule.ID, &rule.UserID, &rule.RuleType, &rule.RuleValue, &rule.Action, &rule.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get spam rule: %w", err)
	}

	return rule, nil
}

// DeleteSpamRule deletes a spam rule
func (db *DB) DeleteSpamRule(id int64) error {
	query := `DELETE FROM user_spam_rules WHERE id = $1`
	_, err := db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete spam rule: %w", err)
	}
	return nil
}

// CheckSpamRules checks if an email matches any user spam rules
// Returns: action ("spam", "allow", or ""), matchedRule
func (db *DB) CheckSpamRules(userID int64, fromEmail string) (string, *SpamRule, error) {
	fromEmail = strings.ToLower(fromEmail)

	// Extract domain from email
	parts := strings.SplitN(fromEmail, "@", 2)
	var fromDomain string
	if len(parts) == 2 {
		fromDomain = parts[1]
	}

	// Check address rules first (more specific), then domain rules
	// Check 'allow' rules before 'spam' rules (whitelist takes priority)
	query := `
		SELECT id, user_id, rule_type, rule_value, action, created_at
		FROM user_spam_rules
		WHERE user_id = $1
		AND (
			(rule_type = 'address' AND rule_value = $2)
			OR (rule_type = 'domain' AND rule_value = $3)
		)
		ORDER BY
			CASE action WHEN 'allow' THEN 0 ELSE 1 END,
			CASE rule_type WHEN 'address' THEN 0 ELSE 1 END
		LIMIT 1
	`

	rule := &SpamRule{}
	err := db.QueryRow(query, userID, fromEmail, fromDomain).Scan(
		&rule.ID, &rule.UserID, &rule.RuleType, &rule.RuleValue, &rule.Action, &rule.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("failed to check spam rules: %w", err)
	}

	return rule.Action, rule, nil
}

// DisableSpamCheck disables a system spam check for a user
func (db *DB) DisableSpamCheck(userID int64, checkName string) error {
	query := `
		INSERT INTO user_disabled_spam_checks (user_id, check_name, disabled_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, check_name) DO NOTHING
	`

	_, err := db.Exec(query, userID, checkName, timeutil.Now())
	if err != nil {
		return fmt.Errorf("failed to disable spam check: %w", err)
	}

	return nil
}

// EnableSpamCheck re-enables a previously disabled spam check
func (db *DB) EnableSpamCheck(userID int64, checkName string) error {
	query := `DELETE FROM user_disabled_spam_checks WHERE user_id = $1 AND check_name = $2`
	_, err := db.Exec(query, userID, checkName)
	if err != nil {
		return fmt.Errorf("failed to enable spam check: %w", err)
	}
	return nil
}

// GetDisabledSpamChecks retrieves all disabled spam checks for a user
func (db *DB) GetDisabledSpamChecks(userID int64) ([]string, error) {
	query := `SELECT check_name FROM user_disabled_spam_checks WHERE user_id = $1`

	rows, err := db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get disabled spam checks: %w", err)
	}
	defer rows.Close()

	var checks []string
	for rows.Next() {
		var checkName string
		if err := rows.Scan(&checkName); err != nil {
			return nil, fmt.Errorf("failed to scan check name: %w", err)
		}
		checks = append(checks, checkName)
	}

	return checks, nil
}

// IsSpamCheckDisabled checks if a specific spam check is disabled for a user
func (db *DB) IsSpamCheckDisabled(userID int64, checkName string) (bool, error) {
	query := `SELECT EXISTS(SELECT 1 FROM user_disabled_spam_checks WHERE user_id = $1 AND check_name = $2)`

	var exists bool
	err := db.QueryRow(query, userID, checkName).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check if spam check disabled: %w", err)
	}

	return exists, nil
}

// GetDisabledSpamChecksMap returns a map for fast lookup
func (db *DB) GetDisabledSpamChecksMap(userID int64) (map[string]bool, error) {
	checks, err := db.GetDisabledSpamChecks(userID)
	if err != nil {
		return nil, err
	}

	result := make(map[string]bool)
	for _, check := range checks {
		result[check] = true
	}

	return result, nil
}

// MarkMessageAsSpam marks a message as spam
func (db *DB) MarkMessageAsSpam(messageID int64, ruleID *int64) error {
	query := `UPDATE messages SET is_spam = true, spam_rule_id = $1 WHERE id = $2`
	_, err := db.Exec(query, ruleID, messageID)
	if err != nil {
		return fmt.Errorf("failed to mark message as spam: %w", err)
	}
	return nil
}

// UnmarkMessageAsSpam removes spam flag from a message
func (db *DB) UnmarkMessageAsSpam(messageID int64) error {
	query := `UPDATE messages SET is_spam = false, spam_rule_id = NULL WHERE id = $1`
	_, err := db.Exec(query, messageID)
	if err != nil {
		return fmt.Errorf("failed to unmark message as spam: %w", err)
	}
	return nil
}

// GetSpamMessages retrieves spam messages for a user
func (db *DB) GetSpamMessages(userID int64, limit, offset int) ([]*models.Message, int, error) {
	// Get total count
	countQuery := `SELECT COUNT(*) FROM messages WHERE user_id = $1 AND is_spam = true`
	var total int
	if err := db.QueryRow(countQuery, userID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("failed to count spam messages: %w", err)
	}

	// Get messages
	query := `
		SELECT id, COALESCE(account_id, 0), user_id, folder_id, uid, message_id, subject,
		       from_addr, to_addr, cc, reply_to, date, body, body_html, size,
		       seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, created_at, updated_at,
		       COALESCE(spam_score, 0), COALESCE(spam_status, 'clean'), COALESCE(spam_reasons, ''),
		       COALESCE(is_spam, false), spam_rule_id
		FROM messages
		WHERE user_id = $1 AND is_spam = true
		ORDER BY date DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := db.Query(query, userID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get spam messages: %w", err)
	}
	defer rows.Close()

	var messages []*models.Message
	for rows.Next() {
		msg := &models.Message{}
		var spamRuleID sql.NullInt64

		err := rows.Scan(
			&msg.ID, &msg.AccountID, &msg.UserID, &msg.FolderID, &msg.UID,
			&msg.MessageID, &msg.Subject, &msg.From, &msg.To, &msg.Cc,
			&msg.ReplyTo, &msg.Date, &msg.Body, &msg.BodyHTML, &msg.Size,
			&msg.Seen, &msg.Flagged, &msg.Answered, &msg.Draft, &msg.Deleted,
			&msg.InReplyTo, &msg.MessageReferences, &msg.CreatedAt, &msg.UpdatedAt,
			&msg.SpamScore, &msg.SpamStatus, &msg.SpamReasons, &msg.IsSpam, &spamRuleID,
		)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to scan spam message: %w", err)
		}

		if spamRuleID.Valid {
			msg.SpamRuleID = &spamRuleID.Int64
		}

		messages = append(messages, msg)
	}

	return messages, total, nil
}

// DeleteOldSpamMessages deletes spam messages older than the specified days
func (db *DB) DeleteOldSpamMessages(daysOld int) (int64, error) {
	query := `
		DELETE FROM messages
		WHERE is_spam = true AND date < $1
	`

	cutoffMs := timeutil.Now() - int64(daysOld)*24*60*60*1000
	result, err := db.Exec(query, cutoffMs)
	if err != nil {
		return 0, fmt.Errorf("failed to delete old spam messages: %w", err)
	}

	count, _ := result.RowsAffected()
	return count, nil
}

// RestoreFromSpam moves a message from spam back to inbox
func (db *DB) RestoreFromSpam(messageID, userID int64) error {
	// Get user's inbox folder
	folder, err := db.GetOrCreateLocalInbox(userID)
	if err != nil {
		return fmt.Errorf("failed to get inbox: %w", err)
	}

	// Get next UID for inbox
	nextUID, err := db.GetNextUIDForFolder(folder.ID)
	if err != nil {
		return fmt.Errorf("failed to get next UID: %w", err)
	}

	// Update message: remove spam flag, move to inbox
	query := `
		UPDATE messages
		SET is_spam = false, spam_rule_id = NULL, folder_id = $1, uid = $2
		WHERE id = $3 AND user_id = $4
	`

	result, err := db.Exec(query, folder.ID, nextUID, messageID, userID)
	if err != nil {
		return fmt.Errorf("failed to restore message from spam: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("message not found or not owned by user")
	}

	return nil
}

// GetSpamMessageCount returns count of spam messages for a user
func (db *DB) GetSpamMessageCount(userID int64) (int, error) {
	query := `SELECT COUNT(*) FROM messages WHERE user_id = $1 AND is_spam = true`
	var count int
	if err := db.QueryRow(query, userID).Scan(&count); err != nil {
		return 0, fmt.Errorf("failed to count spam messages: %w", err)
	}
	return count, nil
}

// SenderStats holds statistics about a sender
type SenderStats struct {
	TotalMessages   int
	SpamMessages    int
	FirstMessageAt  int64
	LastMessageAt   int64
	HasWhitelist    bool
	HasBlacklist    bool
	MatchedRuleType string // "address" or "domain"
	MatchedRuleID   int64
}

// GetSenderStats returns statistics about a sender for a user
func (db *DB) GetSenderStats(userID int64, senderEmail, senderDomain string) (*SenderStats, error) {
	stats := &SenderStats{}

	// Get message counts by sender email
	query := `
		SELECT
			COUNT(*) as total,
			COUNT(*) FILTER (WHERE is_spam = true) as spam,
			MIN(date) as first_msg,
			MAX(date) as last_msg
		FROM messages
		WHERE user_id = $1 AND LOWER(from_addr) LIKE $2
	`
	pattern := "%" + strings.ToLower(senderEmail) + "%"
	err := db.QueryRow(query, userID, pattern).Scan(
		&stats.TotalMessages,
		&stats.SpamMessages,
		&stats.FirstMessageAt,
		&stats.LastMessageAt,
	)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("failed to get sender stats: %w", err)
	}

	// Check if sender matches any rules
	ruleQuery := `
		SELECT id, rule_type, action
		FROM user_spam_rules
		WHERE user_id = $1 AND (
			(rule_type = 'address' AND rule_value = $2)
			OR (rule_type = 'domain' AND rule_value = $3)
		)
		ORDER BY
			CASE rule_type WHEN 'address' THEN 0 ELSE 1 END
		LIMIT 1
	`
	var ruleID int64
	var ruleType, action string
	err = db.QueryRow(ruleQuery, userID, strings.ToLower(senderEmail), strings.ToLower(senderDomain)).Scan(
		&ruleID, &ruleType, &action,
	)
	if err == nil {
		stats.MatchedRuleID = ruleID
		stats.MatchedRuleType = ruleType
		if action == "allow" {
			stats.HasWhitelist = true
		} else if action == "spam" {
			stats.HasBlacklist = true
		}
	}

	return stats, nil
}

// GetDomainStats returns statistics about a domain for a user
func (db *DB) GetDomainStats(userID int64, domain string) (totalMessages int, spamMessages int, err error) {
	query := `
		SELECT
			COUNT(*) as total,
			COUNT(*) FILTER (WHERE is_spam = true) as spam
		FROM messages
		WHERE user_id = $1 AND LOWER(from_addr) LIKE $2
	`
	pattern := "%@" + strings.ToLower(domain) + "%"
	err = db.QueryRow(query, userID, pattern).Scan(&totalMessages, &spamMessages)
	if err != nil && err != sql.ErrNoRows {
		return 0, 0, fmt.Errorf("failed to get domain stats: %w", err)
	}
	return totalMessages, spamMessages, nil
}
