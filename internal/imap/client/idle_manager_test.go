package client

import (
	"errors"
	"testing"
	"time"
)

// TestReconnectDelay фиксирует поведение пауз между IDLE-сессиями.
//
// Повод: на проде сессия яндексового аккаунта прожила 10 часов, перестав при
// этом получать уведомления о новой почте. Лечится плановым пересозданием
// сессии по TTL — и это пересоздание не должно ни выглядеть сбоем, ни копить
// backoff, иначе «лечение» само отложит переподключение на пять минут.
func TestReconnectDelay(t *testing.T) {
	const (
		base = 10 * time.Second
		max  = 5 * time.Minute
	)

	cases := []struct {
		name       string
		err        error
		sessionLen time.Duration
		current    time.Duration
		wantWait   time.Duration
		wantNext   time.Duration
	}{
		{
			name:       "плановое обновление — без паузы и с чистым backoff",
			err:        errIdleRefresh,
			sessionLen: idleSessionTTL,
			current:    2 * time.Minute,
			wantWait:   0,
			wantNext:   base,
		},
		{
			name:       "обёрнутый errIdleRefresh тоже плановый",
			err:        errors.Join(errIdleRefresh, errors.New("контекст")),
			sessionLen: idleSessionTTL,
			current:    max,
			wantWait:   0,
			wantNext:   base,
		},
		{
			name:       "быстрый сбой — удвоение",
			err:        errors.New("idle: connection closed"),
			sessionLen: 5 * time.Second,
			current:    base,
			wantWait:   base,
			wantNext:   2 * base,
		},
		{
			name:       "серия быстрых сбоев упирается в потолок",
			err:        errors.New("idle: connection closed"),
			sessionLen: time.Second,
			current:    4 * time.Minute,
			wantWait:   4 * time.Minute,
			wantNext:   max,
		},
		{
			name:       "разрыв после долгой работы не наследует прошлый backoff",
			err:        errors.New("idle: connection closed"),
			sessionLen: time.Hour,
			current:    max,
			wantWait:   base,
			wantNext:   2 * base,
		},
		{
			name:       "сессия завершилась без ошибки и без TTL — как обычный сбой",
			err:        nil,
			sessionLen: 5 * time.Second,
			current:    base,
			wantWait:   base,
			wantNext:   2 * base,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wait, next := reconnectDelay(c.err, c.sessionLen, c.current, base, max)
			if wait != c.wantWait {
				t.Errorf("пауза = %v, ожидалась %v", wait, c.wantWait)
			}
			if next != c.wantNext {
				t.Errorf("следующий backoff = %v, ожидался %v", next, c.wantNext)
			}
		})
	}
}

// TestIdleSessionTTLUnderServerLimit: RFC 2177 разрешает серверу закрывать
// IDLE после 30 минут, и библиотека перевыпускает команду каждые 25 минут.
// TTL сессии должен быть заметно меньше обоих порогов — иначе пересоздание
// соединения будет случаться уже после того, как сервер потерял к нему
// интерес, и смысл в нём пропадёт.
func TestIdleSessionTTLUnderServerLimit(t *testing.T) {
	if idleSessionTTL >= 25*time.Minute {
		t.Fatalf("idleSessionTTL = %v, должен быть меньше 25 минут (перевыпуск IDLE в библиотеке)", idleSessionTTL)
	}
	if idleSessionTTL < time.Minute {
		t.Fatalf("idleSessionTTL = %v — слишком часто, это переподключение на каждый чих", idleSessionTTL)
	}
}
