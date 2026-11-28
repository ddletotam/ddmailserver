package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// Mailing lists (docs/lunlun-bot-plan.md, «Рассылки»; migrations/071). Spam is
// the whole risk of the feature, hence: people subscribe only themselves
// (through the bot, confirming the address by a link), every letter carries a
// one-click unsubscribe, only the owner posts and not often, and a list whose
// letters bounce is stopped. The owner pays by the subscriber ceiling.

// FeatureMailingList is one list (ref_id), priced by its tier.
const FeatureMailingList = "mailing_list"

// ListTiers are the subscriber ceilings a list is sold with.
var ListTiers = []int{100, 500, 2000}

// ListTierPriceKey is the bot_api.prices key of a tier.
func ListTierPriceKey(tier int) string { return fmt.Sprintf("%s_%d", FeatureMailingList, tier) }

const (
	// ListPostsPerDay: letters the owner may send to one list in 24 hours.
	ListPostsPerDay = 5
	// ListBounceStop: addresses that turned out not to exist within 24
	// hours, after which the list stops for a day.
	ListBounceStop = 5
	// ListConfirmsPerDay: confirmation letters one Telegram user may cause in
	// 24 hours — subscribing other people's addresses is how this is abused.
	ListConfirmsPerDay = 10
	// listMaxAttempts: a letter that fails temporarily this many times is
	// given up.
	listMaxAttempts = 8
	dayMS           = int64(24 * 3600 * 1000)
)

var (
	ErrListNotFound      = errors.New("mailing list not found")
	ErrListAddressTaken  = errors.New("address taken")
	ErrListFull          = errors.New("mailing list is full")
	ErrListOff           = errors.New("mailing list is off")
	ErrListNotOwner      = errors.New("only the list owner may post")
	ErrListPostLimit     = errors.New("too many letters to the list today")
	ErrAlreadySubscribed = errors.New("already subscribed")
	ErrConfirmPending    = errors.New("confirmation already sent")
	ErrTooManyConfirms   = errors.New("too many subscriptions today")
	ErrSubscriberUnknown = errors.New("subscriber not found")
)

// MailingList is one list with its subscriber counts.
type MailingList struct {
	ID           int64
	UserID       int64
	DomainID     int64
	Domain       string
	LocalPart    string
	Title        string
	Code         string
	Tier         int
	Enabled      bool
	StoppedUntil int64
	CreatedAt    int64
	Active       int
	Pending      int
}

// Address is the list's e-mail address.
func (l *MailingList) Address() string { return l.LocalPart + "@" + l.Domain }

// ListSubscriber is one address on a list.
type ListSubscriber struct {
	ID          int64
	ListID      int64
	Email       string
	TgUserID    int64
	Status      string
	Token       string
	CreatedAt   int64
	ConfirmedAt int64
}

const listCols = `l.id, l.user_id, l.domain_id, d.domain, l.local_part, l.title, l.code, l.tier, l.enabled,
	l.stopped_until, l.created_at,
	(SELECT COUNT(*) FROM list_subscribers s WHERE s.list_id = l.id AND s.status = 'active'),
	(SELECT COUNT(*) FROM list_subscribers s WHERE s.list_id = l.id AND s.status = 'pending')`

func scanList(row interface{ Scan(...any) error }) (*MailingList, error) {
	var l MailingList
	err := row.Scan(&l.ID, &l.UserID, &l.DomainID, &l.Domain, &l.LocalPart, &l.Title, &l.Code, &l.Tier, &l.Enabled,
		&l.StoppedUntil, &l.CreatedAt, &l.Active, &l.Pending)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrListNotFound
	}
	return &l, err
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CreateMailingList makes a list at local@domain. The address must be free of
// mailboxes, users and other lists. Code and timestamps are filled in.
func (db *DB) CreateMailingList(l *MailingList) error {
	code := make([]byte, 6)
	if _, err := rand.Read(code); err != nil {
		return err
	}
	l.Code = hex.EncodeToString(code)
	l.CreatedAt = timeutil.Now()
	l.Enabled = true
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var domain string
	if err := tx.QueryRow(`SELECT domain FROM domains WHERE id = $1`, l.DomainID).Scan(&domain); err != nil {
		return fmt.Errorf("list domain %d: %w", l.DomainID, err)
	}
	taken, err := loginTaken(tx, strings.ToLower(l.LocalPart), domain, l.DomainID)
	if err != nil {
		return err
	}
	if taken {
		return ErrListAddressTaken
	}
	if err := tx.QueryRow(`INSERT INTO mailing_lists (user_id, domain_id, local_part, title, code, tier, enabled, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, true, $7) ON CONFLICT DO NOTHING RETURNING id`,
		l.UserID, l.DomainID, strings.ToLower(l.LocalPart), l.Title, l.Code, l.Tier, l.CreatedAt).Scan(&l.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrListAddressTaken
		}
		return fmt.Errorf("create list: %w", err)
	}
	l.Domain = domain
	return tx.Commit()
}

// MailingListsOf returns the user's lists, oldest first.
func (db *DB) MailingListsOf(userID int64) ([]*MailingList, error) {
	rows, err := db.Query(`SELECT `+listCols+` FROM mailing_lists l JOIN domains d ON d.id = l.domain_id
		WHERE l.user_id = $1 ORDER BY l.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("lists of user %d: %w", userID, err)
	}
	defer rows.Close()
	var out []*MailingList
	for rows.Next() {
		l, err := scanList(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// MailingListByID returns a list by id.
func (db *DB) MailingListByID(id int64) (*MailingList, error) {
	return scanList(db.QueryRow(`SELECT `+listCols+` FROM mailing_lists l JOIN domains d ON d.id = l.domain_id
		WHERE l.id = $1`, id))
}

// MailingListByCode returns a list by its subscribe-link code.
func (db *DB) MailingListByCode(code string) (*MailingList, error) {
	return scanList(db.QueryRow(`SELECT `+listCols+` FROM mailing_lists l JOIN domains d ON d.id = l.domain_id
		WHERE l.code = $1`, code))
}

// MailingListByAddress returns the list at an address; ErrListNotFound — none.
func (db *DB) MailingListByAddress(addr string) (*MailingList, error) {
	local, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(addr)), "@")
	if !ok {
		return nil, ErrListNotFound
	}
	return scanList(db.QueryRow(`SELECT `+listCols+` FROM mailing_lists l JOIN domains d ON d.id = l.domain_id
		WHERE l.local_part = $1 AND lower(d.domain) = $2 AND d.verified`, local, domain))
}

// DeleteMailingList deletes the user's list with its subscribers and letters.
func (db *DB) DeleteMailingList(userID, id int64) error {
	res, err := db.Exec(`DELETE FROM mailing_lists WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete list %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrListNotFound
	}
	return nil
}

// ListSubscribers returns the subscribers of a list, newest first.
func (db *DB) ListSubscribers(listID int64) ([]ListSubscriber, error) {
	rows, err := db.Query(`SELECT id, list_id, email, tg_user_id, status, token, created_at, confirmed_at
		FROM list_subscribers WHERE list_id = $1 ORDER BY id DESC`, listID)
	if err != nil {
		return nil, fmt.Errorf("subscribers of list %d: %w", listID, err)
	}
	defer rows.Close()
	var out []ListSubscriber
	for rows.Next() {
		var s ListSubscriber
		if err := rows.Scan(&s.ID, &s.ListID, &s.Email, &s.TgUserID, &s.Status, &s.Token, &s.CreatedAt, &s.ConfirmedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Subscribe puts email on the list of code for Telegram user tg. confirmed —
// the address is known to be theirs (their own address of the service): it is
// active at once; otherwise it waits for the confirmation letter queued here.
func (db *DB) Subscribe(code string, tg int64, email string, confirmed bool) (*ListSubscriber, *MailingList, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	tx, err := db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	l, err := scanList(tx.QueryRow(`SELECT `+listCols+` FROM mailing_lists l JOIN domains d ON d.id = l.domain_id
		WHERE l.code = $1 FOR UPDATE OF l`, code))
	if err != nil {
		return nil, nil, err
	}
	if !l.Enabled {
		return nil, l, ErrListOff
	}
	now := timeutil.Now()
	var s ListSubscriber
	err = tx.QueryRow(`SELECT id, list_id, email, tg_user_id, status, token, created_at, confirmed_at
		FROM list_subscribers WHERE list_id = $1 AND email = $2`, l.ID, email).
		Scan(&s.ID, &s.ListID, &s.Email, &s.TgUserID, &s.Status, &s.Token, &s.CreatedAt, &s.ConfirmedAt)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, l, fmt.Errorf("subscriber lookup: %w", err)
	}
	if exists && s.Status == "active" {
		return &s, l, ErrAlreadySubscribed
	}
	if !exists || s.Status != "pending" {
		if l.Active+l.Pending >= l.Tier {
			return nil, l, ErrListFull
		}
	}
	if !confirmed {
		if exists && s.Status == "pending" && s.TgUserID == tg {
			var recent bool
			if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM list_deliveries WHERE subscriber_id = $1
				AND issue_id IS NULL AND created_at > $2)`, s.ID, now-dayMS).Scan(&recent); err != nil {
				return nil, l, err
			}
			if recent {
				return &s, l, ErrConfirmPending
			}
		}
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM list_deliveries ld JOIN list_subscribers s ON s.id = ld.subscriber_id
			WHERE s.tg_user_id = $1 AND ld.issue_id IS NULL AND ld.created_at > $2`, tg, now-dayMS).Scan(&n); err != nil {
			return nil, l, err
		}
		if n >= ListConfirmsPerDay {
			return nil, l, ErrTooManyConfirms
		}
	}
	status := "pending"
	var confirmedAt int64
	if confirmed {
		status, confirmedAt = "active", now
	}
	if exists {
		if _, err := tx.Exec(`UPDATE list_subscribers SET tg_user_id = $2, status = $3, confirmed_at = $4, changed_at = $5
			WHERE id = $1`, s.ID, tg, status, confirmedAt, now); err != nil {
			return nil, l, err
		}
	} else {
		tok, err := randomToken(24)
		if err != nil {
			return nil, l, err
		}
		s = ListSubscriber{ListID: l.ID, Email: email, Token: tok, CreatedAt: now}
		if err := tx.QueryRow(`INSERT INTO list_subscribers (list_id, email, tg_user_id, status, token, created_at, confirmed_at, changed_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $6) RETURNING id`, l.ID, email, tg, status, tok, now, confirmedAt).Scan(&s.ID); err != nil {
			return nil, l, fmt.Errorf("subscribe: %w", err)
		}
	}
	s.TgUserID, s.Status, s.ConfirmedAt = tg, status, confirmedAt
	if !confirmed {
		if _, err := tx.Exec(`INSERT INTO list_deliveries (list_id, subscriber_id, next_at, created_at) VALUES ($1, $2, $3, $3)`,
			l.ID, s.ID, now); err != nil {
			return nil, l, fmt.Errorf("queue confirmation: %w", err)
		}
	}
	return &s, l, tx.Commit()
}

// SubscriberByToken returns the subscriber of a confirm/unsubscribe link with
// its list.
func (db *DB) SubscriberByToken(token string) (*ListSubscriber, *MailingList, error) {
	var s ListSubscriber
	err := db.QueryRow(`SELECT id, list_id, email, tg_user_id, status, token, created_at, confirmed_at
		FROM list_subscribers WHERE token = $1`, token).
		Scan(&s.ID, &s.ListID, &s.Email, &s.TgUserID, &s.Status, &s.Token, &s.CreatedAt, &s.ConfirmedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrSubscriberUnknown
	}
	if err != nil {
		return nil, nil, err
	}
	l, err := db.MailingListByID(s.ListID)
	return &s, l, err
}

// ConfirmSubscriber activates a pending subscription by its link.
func (db *DB) ConfirmSubscriber(token string) error {
	now := timeutil.Now()
	res, err := db.Exec(`UPDATE list_subscribers SET status = 'active', confirmed_at = $2, changed_at = $2
		WHERE token = $1 AND status = 'pending'`, token, now)
	if err != nil {
		return fmt.Errorf("confirm subscriber: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSubscriberUnknown
	}
	return nil
}

// UnsubscribeToken takes an address off its list by the link from a letter.
// Letters already queued to it are dropped.
func (db *DB) UnsubscribeToken(token string) error {
	return db.unsubscribe(`token = $1`, token)
}

// UnsubscribeTg takes the Telegram user's subscription off its list.
func (db *DB) UnsubscribeTg(tg, subscriberID int64) error {
	return db.unsubscribe(`id = $1 AND tg_user_id = $2`, subscriberID, tg)
}

// RemoveSubscriber: the list's owner takes an address off.
func (db *DB) RemoveSubscriber(ownerID, listID, subscriberID int64) error {
	return db.unsubscribe(`id = $1 AND list_id = $2 AND list_id IN (SELECT id FROM mailing_lists WHERE user_id = $3)`,
		subscriberID, listID, ownerID)
}

func (db *DB) unsubscribe(where string, args ...any) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var id int64
	err = tx.QueryRow(`UPDATE list_subscribers SET status = 'unsubscribed', changed_at = `+
		fmt.Sprint(timeutil.Now())+` WHERE `+where+` AND status IN ('active', 'pending') RETURNING id`, args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSubscriberUnknown
	}
	if err != nil {
		return fmt.Errorf("unsubscribe: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM list_deliveries WHERE subscriber_id = $1 AND status = 'pending'`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// TgListSubscription is a subscription of a Telegram user, for the bot.
type TgListSubscription struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Address string `json:"address"`
	Email   string `json:"email"`
	Status  string `json:"status"`
}

// ListSubscriptionsOfTg returns the user's active and pending subscriptions.
func (db *DB) ListSubscriptionsOfTg(tg int64) ([]TgListSubscription, error) {
	rows, err := db.Query(`SELECT s.id, l.title, l.local_part || '@' || d.domain, s.email, s.status
		FROM list_subscribers s JOIN mailing_lists l ON l.id = s.list_id JOIN domains d ON d.id = l.domain_id
		WHERE s.tg_user_id = $1 AND s.status IN ('active', 'pending') ORDER BY s.id`, tg)
	if err != nil {
		return nil, fmt.Errorf("subscriptions of tg %d: %w", tg, err)
	}
	defer rows.Close()
	out := []TgListSubscription{}
	for rows.Next() {
		var s TgListSubscription
		if err := rows.Scan(&s.ID, &s.Title, &s.Address, &s.Email, &s.Status); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListIssue is a letter the owner sent to a list, with delivery counts.
type ListIssue struct {
	ID        int64
	Subject   string
	CreatedAt int64
	Sent      int
	Pending   int
	Failed    int
}

// ListIssues returns the latest letters of a list.
func (db *DB) ListIssues(listID int64, limit int) ([]ListIssue, error) {
	rows, err := db.Query(`SELECT i.id, i.subject, i.created_at,
		COUNT(*) FILTER (WHERE ld.status = 'sent'), COUNT(*) FILTER (WHERE ld.status = 'pending'),
		COUNT(*) FILTER (WHERE ld.status = 'failed')
		FROM list_issues i LEFT JOIN list_deliveries ld ON ld.issue_id = i.id
		WHERE i.list_id = $1 GROUP BY i.id ORDER BY i.id DESC LIMIT $2`, listID, limit)
	if err != nil {
		return nil, fmt.Errorf("issues of list %d: %w", listID, err)
	}
	defer rows.Close()
	var out []ListIssue
	for rows.Next() {
		var i ListIssue
		if err := rows.Scan(&i.ID, &i.Subject, &i.CreatedAt, &i.Sent, &i.Pending, &i.Failed); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// PostToList queues a letter the user sent to a list address for every
// active subscriber. Posting the same outbox message again (a send retry)
// changes nothing. Errors: ErrListNotFound (not a list address),
// ErrListNotOwner, ErrListOff, ErrListPostLimit.
func (db *DB) PostToList(addr string, senderUserID, outboxID int64, subject string, raw []byte) (*MailingList, error) {
	l, err := db.MailingListByAddress(addr)
	if err != nil {
		return nil, err
	}
	owner, err := billingOwner(db, senderUserID)
	if err != nil {
		return l, err
	}
	if owner != l.UserID {
		return l, ErrListNotOwner
	}
	now := timeutil.Now()
	if !l.Enabled || l.StoppedUntil > now {
		return l, ErrListOff
	}
	tx, err := db.Begin()
	if err != nil {
		return l, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.Exec(`SELECT id FROM mailing_lists WHERE id = $1 FOR UPDATE`, l.ID); err != nil {
		return l, err
	}
	var seen bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM list_issues WHERE list_id = $1 AND outbox_id = $2)`,
		l.ID, outboxID).Scan(&seen); err != nil {
		return l, err
	}
	if seen {
		return l, nil
	}
	var today int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM list_issues WHERE list_id = $1 AND created_at > $2`,
		l.ID, now-dayMS).Scan(&today); err != nil {
		return l, err
	}
	if today >= ListPostsPerDay {
		return l, ErrListPostLimit
	}
	var issueID int64
	if err := tx.QueryRow(`INSERT INTO list_issues (list_id, outbox_id, subject, raw, created_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`, l.ID, outboxID, subject, raw, now).Scan(&issueID); err != nil {
		return l, fmt.Errorf("post to list: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO list_deliveries (list_id, issue_id, subscriber_id, next_at, created_at)
		SELECT $1, $2, id, $3, $3 FROM list_subscribers WHERE list_id = $1 AND status = 'active'`,
		l.ID, issueID, now); err != nil {
		return l, fmt.Errorf("queue issue: %w", err)
	}
	return l, tx.Commit()
}

// ListDelivery is one letter to send.
type ListDelivery struct {
	ID         int64
	ListID     int64
	IssueID    int64 // 0 — the confirmation request
	Attempts   int
	Email      string
	Token      string
	ListTitle  string
	ListAddr   string
	ListUserID int64
}

// listLease: a claimed letter is not handed out again for this long, so a
// crash mid-send delays it rather than losing it.
const listLease = int64(15 * 60 * 1000)

// ClaimListDeliveries hands out up to limit letters that are due, from lists
// that are on and not stopped.
func (db *DB) ClaimListDeliveries(limit int) ([]ListDelivery, error) {
	now := timeutil.Now()
	rows, err := db.Query(`UPDATE list_deliveries ld SET next_at = $2, attempts = ld.attempts + 1
		FROM list_subscribers s, mailing_lists l, domains d
		WHERE ld.id IN (
			SELECT x.id FROM list_deliveries x JOIN mailing_lists xl ON xl.id = x.list_id
			WHERE x.status = 'pending' AND x.next_at <= $1 AND xl.enabled AND xl.stopped_until <= $1
			ORDER BY x.next_at, x.id LIMIT $3 FOR UPDATE OF x SKIP LOCKED)
		AND s.id = ld.subscriber_id AND l.id = ld.list_id AND d.id = l.domain_id
		RETURNING ld.id, ld.list_id, COALESCE(ld.issue_id, 0), ld.attempts, s.email, s.token, l.title,
			l.local_part || '@' || d.domain, l.user_id`, now, now+listLease, limit)
	if err != nil {
		return nil, fmt.Errorf("claim list deliveries: %w", err)
	}
	defer rows.Close()
	var out []ListDelivery
	for rows.Next() {
		var d ListDelivery
		if err := rows.Scan(&d.ID, &d.ListID, &d.IssueID, &d.Attempts, &d.Email, &d.Token, &d.ListTitle,
			&d.ListAddr, &d.ListUserID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListIssueRaw returns the letter of an issue as the owner sent it.
func (db *DB) ListIssueRaw(issueID int64) ([]byte, error) {
	var raw []byte
	if err := db.QueryRow(`SELECT raw FROM list_issues WHERE id = $1`, issueID).Scan(&raw); err != nil {
		return nil, fmt.Errorf("issue %d: %w", issueID, err)
	}
	return raw, nil
}

// FinishListDelivery records how a letter went: sent (sendErr nil), failed
// for good (permanent — the address does not exist: the subscriber is
// dropped, and a list collecting ListBounceStop of those in a day is stopped
// for a day), or failed for now — tried again later.
func (db *DB) FinishListDelivery(d ListDelivery, sendErr error, permanent bool) error {
	now := timeutil.Now()
	if sendErr == nil {
		_, err := db.Exec(`UPDATE list_deliveries SET status = 'sent', done_at = $2, last_error = '' WHERE id = $1`, d.ID, now)
		return err
	}
	msg := sendErr.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	if !permanent && d.Attempts < listMaxAttempts {
		backoff := int64(d.Attempts) * int64(d.Attempts) * 5 * 60 * 1000 // 5, 20, 45 min ...
		_, err := db.Exec(`UPDATE list_deliveries SET next_at = $2, last_error = $3 WHERE id = $1`, d.ID, now+backoff, msg)
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.Exec(`UPDATE list_deliveries SET status = 'failed', permanent = $2, done_at = $3, last_error = $4
		WHERE id = $1`, d.ID, permanent, now, msg); err != nil {
		return err
	}
	if permanent {
		if _, err := tx.Exec(`UPDATE list_subscribers SET status = 'bounced', changed_at = $2
			WHERE id = (SELECT subscriber_id FROM list_deliveries WHERE id = $1)`, d.ID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM list_deliveries WHERE status = 'pending'
			AND subscriber_id = (SELECT subscriber_id FROM list_deliveries WHERE id = $1)`, d.ID); err != nil {
			return err
		}
		var bounced int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM list_deliveries WHERE list_id = $1 AND permanent AND done_at > $2`,
			d.ListID, now-dayMS).Scan(&bounced); err != nil {
			return err
		}
		if bounced >= ListBounceStop {
			res, err := tx.Exec(`UPDATE mailing_lists SET stopped_until = $2 WHERE id = $1 AND stopped_until <= $3`,
				d.ListID, now+dayMS, now)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				if err := notifyUserOnce(tx, d.ListUserID, "list_stopped", fmt.Sprintf(
					"Рассылка %s остановлена на сутки: за последние 24 часа %d адресов подписчиков оказались несуществующими. "+
						"Неотправленные письма уйдут после остановки; такие адреса из рассылки уже убраны.", d.ListAddr, bounced),
					dayMS); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}
