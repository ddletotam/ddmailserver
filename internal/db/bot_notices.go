package db

import (
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// BotNotice is a message for a person, delivered by the Telegram bot.
type BotNotice struct {
	ID       int64  `json:"id"`
	TgUserID int64  `json:"tg_user_id"`
	Kind     string `json:"kind"`
	Text     string `json:"text"`
}

// notifyUserOnce queues a notice for the Telegram account of userID, unless a
// notice of the same kind was queued for it within the last window ms. Users
// without a Telegram link are skipped silently.
func notifyUserOnce(q querier, userID int64, kind, text string, window int64) error {
	now := timeutil.Now()
	_, err := q.Exec(`
		INSERT INTO bot_notices (tg_user_id, kind, text, created_at)
		SELECT l.tg_user_id, $2::varchar, $3::text, $4::bigint FROM telegram_links l
		WHERE l.user_id = $1 AND NOT EXISTS (
			SELECT 1 FROM bot_notices n WHERE n.tg_user_id = l.tg_user_id AND n.kind = $2::varchar AND n.created_at > $5::bigint)`,
		userID, kind, text, now, now-window)
	if err != nil {
		return fmt.Errorf("queue notice %s for user %d: %w", kind, userID, err)
	}
	return nil
}

// PendingBotNotices returns up to limit notices the bot has not delivered yet.
func (db *DB) PendingBotNotices(limit int) ([]BotNotice, error) {
	rows, err := db.Query(`SELECT id, tg_user_id, kind, text FROM bot_notices
		WHERE sent_at IS NULL ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("pending notices: %w", err)
	}
	defer rows.Close()
	out := []BotNotice{}
	for rows.Next() {
		var n BotNotice
		if err := rows.Scan(&n.ID, &n.TgUserID, &n.Kind, &n.Text); err != nil {
			return nil, fmt.Errorf("pending notices: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkBotNoticeSent records that the bot delivered (or gave up on) a notice.
func (db *DB) MarkBotNoticeSent(id int64) error {
	if _, err := db.Exec(`UPDATE bot_notices SET sent_at = $2 WHERE id = $1 AND sent_at IS NULL`, id, timeutil.Now()); err != nil {
		return fmt.Errorf("mark notice %d: %w", id, err)
	}
	return nil
}
