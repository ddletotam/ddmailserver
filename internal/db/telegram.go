package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// Sign-up through the Telegram bot (docs/lunlun-bot-plan.md).
var (
	// ErrTelegramNotLinked: this Telegram id has no account.
	ErrTelegramNotLinked = errors.New("telegram account has no mail account")
	// ErrTelegramBanned: this Telegram id is banned and may not sign up again.
	ErrTelegramBanned = errors.New("telegram account is banned")
	// ErrTelegramRegistered: this Telegram id already has its free account.
	ErrTelegramRegistered = errors.New("telegram account already has a mail account")
	// ErrLoginTaken: the login (or the address it makes) is in use.
	ErrLoginTaken = errors.New("login is taken")
)

// TelegramAccount is the account a Telegram user signed up with.
type TelegramAccount struct {
	TgUserID  int64
	UserID    int64
	Username  string
	Banned    bool
	CreatedAt int64
}

// GetTelegramAccount returns the account linked to a Telegram user.
// ErrTelegramNotLinked — none (never signed up, or the account was deleted);
// a banned id is returned with Banned set, whether or not it has an account.
func (db *DB) GetTelegramAccount(tgUserID int64) (*TelegramAccount, error) {
	a := &TelegramAccount{TgUserID: tgUserID}
	var userID sql.NullInt64
	var username sql.NullString
	err := db.QueryRow(`
		SELECT l.user_id, u.username, l.banned, l.created_at
		FROM telegram_links l LEFT JOIN users u ON u.id = l.user_id
		WHERE l.tg_user_id = $1`, tgUserID).Scan(&userID, &username, &a.Banned, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTelegramNotLinked
	}
	if err != nil {
		return nil, fmt.Errorf("telegram account %d: %w", tgUserID, err)
	}
	a.UserID, a.Username = userID.Int64, username.String
	if !userID.Valid && !a.Banned {
		return nil, ErrTelegramNotLinked
	}
	return a, nil
}

// LoginTaken reports whether login can't be given out on the domain: a user
// with that name, an account of a mailbox with that address, or a mailbox with
// that local part already exists.
func (db *DB) LoginTaken(login, domain string, domainID int64) (bool, error) {
	return loginTaken(db, login, domain, domainID)
}

func loginTaken(q querier, login, domain string, domainID int64) (bool, error) {
	var taken bool
	err := q.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM users WHERE lower(username) IN ($1, $2))
		    OR EXISTS (SELECT 1 FROM mailboxes WHERE domain_id = $3 AND lower(local_part) = $1)`,
		login, login+"@"+domain, domainID).Scan(&taken)
	if err != nil {
		return false, fmt.Errorf("check login %q: %w", login, err)
	}
	return taken, nil
}

// CreateTelegramAccount signs a Telegram user up: an ordinary user named
// login, the address login@domain delivered to it, and the link to the
// Telegram id — all or nothing. Errors: ErrTelegramBanned,
// ErrTelegramRegistered, ErrLoginTaken.
func (db *DB) CreateTelegramAccount(tgUserID int64, login, domain string, domainID int64, passwordHash, language string) (*models.User, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var linked sql.NullInt64
	var banned bool
	err = tx.QueryRow(`SELECT user_id, banned FROM telegram_links WHERE tg_user_id = $1 FOR UPDATE`, tgUserID).Scan(&linked, &banned)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, fmt.Errorf("telegram link %d: %w", tgUserID, err)
	case banned:
		return nil, ErrTelegramBanned
	case linked.Valid:
		return nil, ErrTelegramRegistered
	}

	taken, err := loginTaken(tx, login, domain, domainID)
	if err != nil {
		return nil, err
	}
	if taken {
		return nil, ErrLoginTaken
	}

	now := timeutil.Now()
	u := &models.User{Username: login, PasswordHash: passwordHash, Language: language, Kind: "user", CreatedAt: now, UpdatedAt: now}
	// is_admin is explicit: CreateUser makes the very first user an admin,
	// which a sign-up must never be. recovery_key_hash is '' rather than NULL:
	// the user lookups scan it into a string, and NULL there breaks sign-in.
	// Recovery goes through the bot instead of a recovery key.
	err = tx.QueryRow(`
		INSERT INTO users (username, password_hash, recovery_key_hash, language, is_admin, approved, kind, created_at, updated_at)
		VALUES ($1, $2, '', $3, false, true, 'user', $4, $4) RETURNING id`,
		login, passwordHash, language, now).Scan(&u.ID)
	if isUniqueViolation(err) {
		return nil, ErrLoginTaken
	}
	if err != nil {
		return nil, fmt.Errorf("create user %q: %w", login, err)
	}
	if _, err := tx.Exec(`
		INSERT INTO mailboxes (user_id, domain_id, local_part, enabled, created_at)
		VALUES ($1, $2, $3, true, $4)`, u.ID, domainID, login, now); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrLoginTaken
		}
		return nil, fmt.Errorf("create mailbox %s@%s: %w", login, domain, err)
	}
	if _, err := tx.Exec(`
		INSERT INTO telegram_links (tg_user_id, user_id, terms_accepted_at, created_at)
		VALUES ($1, $2, $3, $3)
		ON CONFLICT (tg_user_id) DO UPDATE SET user_id = EXCLUDED.user_id, terms_accepted_at = EXCLUDED.terms_accepted_at`,
		tgUserID, u.ID, now); err != nil {
		return nil, fmt.Errorf("link telegram %d: %w", tgUserID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return u, nil
}
