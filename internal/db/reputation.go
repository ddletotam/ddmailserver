package db

import (
	"database/sql"
	"fmt"
	"time"
)

// SenderReputation represents a sender's spam/ham statistics
type SenderReputation struct {
	ID        int64
	Email     string
	Domain    string
	IP        string
	SpamCount int
	HamCount  int
	LastSeen  time.Time
}

// GetSenderReputation retrieves reputation for an email/IP combination
func (db *DB) GetSenderReputation(email, ip string) (*SenderReputation, error) {
	rep := &SenderReputation{}
	var lastSeen sql.NullTime

	query := `
		SELECT id, email, domain, ip, spam_count, ham_count, last_seen
		FROM sender_reputation
		WHERE email = $1 AND ip = $2
	`

	err := db.QueryRow(query, email, ip).Scan(
		&rep.ID, &rep.Email, &rep.Domain, &rep.IP,
		&rep.SpamCount, &rep.HamCount, &lastSeen,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get sender reputation: %w", err)
	}

	if lastSeen.Valid {
		rep.LastSeen = lastSeen.Time
	}

	return rep, nil
}

// GetDomainReputation retrieves aggregated reputation for a domain
func (db *DB) GetDomainReputation(domain string) (spamCount, hamCount int, err error) {
	query := `
		SELECT COALESCE(SUM(spam_count), 0), COALESCE(SUM(ham_count), 0)
		FROM sender_reputation
		WHERE domain = $1
	`

	err = db.QueryRow(query, domain).Scan(&spamCount, &hamCount)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get domain reputation: %w", err)
	}

	return spamCount, hamCount, nil
}

// UpdateSenderReputation updates or creates a sender reputation record
func (db *DB) UpdateSenderReputation(email, domain, ip string, isSpam bool) error {
	// Use upsert to handle both insert and update
	query := `
		INSERT INTO sender_reputation (email, domain, ip, spam_count, ham_count, last_seen)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (email, ip) DO UPDATE SET
			spam_count = sender_reputation.spam_count + $4,
			ham_count = sender_reputation.ham_count + $5,
			last_seen = $6
	`

	var spamIncr, hamIncr int
	if isSpam {
		spamIncr = 1
	} else {
		hamIncr = 1
	}

	_, err := db.Exec(query, email, domain, ip, spamIncr, hamIncr, time.Now())
	if err != nil {
		return fmt.Errorf("failed to update sender reputation: %w", err)
	}

	return nil
}

// GetReputationScore calculates a reputation score for a sender
// Returns a score from -1.0 (very bad) to 1.0 (very good), 0.0 for unknown
func (db *DB) GetReputationScore(email, domain, ip string) (float64, error) {
	// First check specific email/IP combination
	rep, err := db.GetSenderReputation(email, ip)
	if err != nil {
		return 0, err
	}

	if rep != nil {
		total := rep.SpamCount + rep.HamCount
		if total > 0 {
			// Calculate score: (ham - spam) / total
			return float64(rep.HamCount-rep.SpamCount) / float64(total), nil
		}
	}

	// Fall back to domain reputation
	spamCount, hamCount, err := db.GetDomainReputation(domain)
	if err != nil {
		return 0, err
	}

	total := spamCount + hamCount
	if total > 0 {
		return float64(hamCount-spamCount) / float64(total), nil
	}

	// Unknown sender
	return 0, nil
}

// RecordSpamFeedback records user feedback about spam classification
func (db *DB) RecordSpamFeedback(userID, messageID int64, action string) error {
	query := `
		INSERT INTO spam_feedback (user_id, message_id, action, created_at)
		VALUES ($1, $2, $3, $4)
	`

	_, err := db.Exec(query, userID, messageID, action, time.Now())
	if err != nil {
		return fmt.Errorf("failed to record spam feedback: %w", err)
	}

	return nil
}

// GetIPReputation gets aggregate reputation for an IP address
func (db *DB) GetIPReputation(ip string) (spamCount, hamCount int, err error) {
	query := `
		SELECT COALESCE(SUM(spam_count), 0), COALESCE(SUM(ham_count), 0)
		FROM sender_reputation
		WHERE ip = $1
	`

	err = db.QueryRow(query, ip).Scan(&spamCount, &hamCount)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get IP reputation: %w", err)
	}

	return spamCount, hamCount, nil
}

// CleanOldReputation removes reputation records older than the specified days
func (db *DB) CleanOldReputation(daysOld int) (int64, error) {
	query := `
		DELETE FROM sender_reputation
		WHERE last_seen < $1
	`

	cutoff := time.Now().AddDate(0, 0, -daysOld)
	result, err := db.Exec(query, cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to clean old reputation: %w", err)
	}

	count, _ := result.RowsAffected()
	return count, nil
}
