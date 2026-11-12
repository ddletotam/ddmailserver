package db

import (
	"context"
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// MaxPushTokensPerUser ограничивает число устройств пользователя: при
// регистрации сверх него уходят самые давно не обновлявшиеся. Честный
// пользователь до него не доберётся, а забытые устройства (приложение
// удалили, не выйдя) не копятся вечно.
const MaxPushTokensPerUser = 20

// UpsertPushToken регистрирует устройство пользователя. Повтор той же пары
// (пользователь, токен) обновляет окружение, topic и updated_at — регистрация
// идемпотентна.
func (db *DB) UpsertPushToken(ctx context.Context, t models.PushToken) error {
	now := timeutil.Now()
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("push token: begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO push_tokens (user_id, kind, token, environment, topic, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
		ON CONFLICT (user_id, kind, token) DO UPDATE
		SET environment = EXCLUDED.environment, topic = EXCLUDED.topic, updated_at = EXCLUDED.updated_at`,
		t.UserID, t.Kind, t.Token, t.Environment, t.Topic, now,
	); err != nil {
		return fmt.Errorf("push token: upsert: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM push_tokens WHERE user_id = $1 AND id NOT IN (
			SELECT id FROM push_tokens WHERE user_id = $1
			ORDER BY updated_at DESC, id DESC LIMIT $2)`,
		t.UserID, MaxPushTokensPerUser,
	); err != nil {
		return fmt.Errorf("push token: trim: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("push token: commit: %w", err)
	}
	return nil
}

// DeleteUserPushToken — выход из учётки на устройстве. Ответ — была ли строка.
func (db *DB) DeleteUserPushToken(ctx context.Context, userID int64, kind, token string) (bool, error) {
	res, err := db.ExecContext(ctx,
		`DELETE FROM push_tokens WHERE user_id = $1 AND kind = $2 AND token = $3`,
		userID, kind, token)
	if err != nil {
		return false, fmt.Errorf("push token: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("push token: delete: %w", err)
	}
	return n > 0, nil
}

// DeletePushToken удаляет токен, который служба доставки назвала
// недействительным, — у всех пользователей: он больше ничей.
func (db *DB) DeletePushToken(ctx context.Context, kind, token string) error {
	if _, err := db.ExecContext(ctx,
		`DELETE FROM push_tokens WHERE kind = $1 AND token = $2`, kind, token); err != nil {
		return fmt.Errorf("push token: drop rejected: %w", err)
	}
	return nil
}

// PushTokensForUser — устройства пользователя.
func (db *DB) PushTokensForUser(ctx context.Context, userID int64) ([]models.PushToken, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, user_id, kind, token, environment, topic, created_at, updated_at
		FROM push_tokens WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("push tokens: query: %w", err)
	}
	defer rows.Close()
	var out []models.PushToken
	for rows.Next() {
		var t models.PushToken
		if err := rows.Scan(&t.ID, &t.UserID, &t.Kind, &t.Token, &t.Environment, &t.Topic, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("push tokens: scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("push tokens: rows: %w", err)
	}
	return out, nil
}

// MarkPushTokenSent запоминает время последней удачной доставки
// (диагностика: видно, какие устройства живы).
func (db *DB) MarkPushTokenSent(ctx context.Context, id int64) error {
	if _, err := db.ExecContext(ctx,
		`UPDATE push_tokens SET last_sent_at = $2 WHERE id = $1`, id, timeutil.Now()); err != nil {
		return fmt.Errorf("push token: mark sent: %w", err)
	}
	return nil
}
