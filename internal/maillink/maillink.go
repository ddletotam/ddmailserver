// Package maillink — связка почты lunlun.email с хранилищем lunlun.io
// (docs/lunlun-bot-plan.md, решения 2026-11-28): учётка в lunlun создаётся при
// регистрации в боте, переход между кабинетом почты и файлами — без повторного
// входа (пропуск, подписанный общим секретом), место у пользователя одно.
//
// Пропуск совместим с lunlun/internal/maillink: base64url(JSON{u,e,n}) "."
// base64url(HMAC-SHA256).
package maillink

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TicketTTL — сколько живёт пропуск входа.
const TicketTTL = 60 * time.Second

var ErrBadTicket = errors.New("bad or expired ticket")

type ticket struct {
	User  int64  `json:"u"`
	Exp   int64  `json:"e"`
	Nonce string `json:"n"`
}

func mac(secret []byte, payload string) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(payload))
	return h.Sum(nil)
}

// Sign выдаёт пропуск для учётки user (id на принимающей стороне).
func Sign(secret []byte, user int64, now time.Time) string {
	nb := make([]byte, 12)
	_, _ = rand.Read(nb)
	raw, _ := json.Marshal(ticket{User: user, Exp: now.Add(TicketTTL).Unix(), Nonce: hex.EncodeToString(nb)})
	p := base64.RawURLEncoding.EncodeToString(raw)
	return p + "." + base64.RawURLEncoding.EncodeToString(mac(secret, p))
}

// Verifier принимает каждый пропуск один раз.
type Verifier struct {
	secret []byte
	mu     sync.Mutex
	used   map[string]int64
}

func NewVerifier(secret []byte) *Verifier { return &Verifier{secret: secret, used: map[string]int64{}} }

// Verify возвращает id учётки из пропуска.
func (v *Verifier) Verify(token string, now time.Time) (int64, error) {
	p, sig, ok := strings.Cut(token, ".")
	if !ok || len(v.secret) == 0 {
		return 0, ErrBadTicket
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac(v.secret, p)) {
		return 0, ErrBadTicket
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return 0, ErrBadTicket
	}
	var t ticket
	if json.Unmarshal(raw, &t) != nil || t.User <= 0 || t.Nonce == "" || now.Unix() > t.Exp {
		return 0, ErrBadTicket
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for n, exp := range v.used {
		if now.Unix() > exp {
			delete(v.used, n)
		}
	}
	if _, seen := v.used[t.Nonce]; seen {
		return 0, ErrBadTicket
	}
	v.used[t.Nonce] = t.Exp
	return t.User, nil
}

// Lunlun — вызовы внутреннего API lunlun (127.0.0.1:3200, мимо nginx).
type Lunlun struct {
	Base   string
	Secret []byte
	SSO    string // https://lunlun.io/sso
	HTTP   *http.Client

	mu    sync.Mutex
	usage map[int64]cached
}

type cached struct {
	bytes int64
	at    time.Time
}

func (l *Lunlun) do(ctx context.Context, method, path string, body, out any) error {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(l.Base, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+string(l.Secret))
	req.Header.Set("Content-Type", "application/json")
	hc := l.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("lunlun %s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// CreateUser заводит (или обновляет пароль) учётку lunlun пользователю почты.
func (l *Lunlun) CreateUser(ctx context.Context, login, password string, mailUser int64) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := l.do(ctx, http.MethodPost, "/internal/v1/users", map[string]any{"login": login, "password": password, "mail_user_id": mailUser}, &out)
	return out.ID, err
}

// SetPassword меняет пароль учётки lunlun пользователя почты.
func (l *Lunlun) SetPassword(ctx context.Context, mailUser int64, password string) error {
	return l.do(ctx, http.MethodPost, fmt.Sprintf("/internal/v1/users/%d/password", mailUser), map[string]any{"password": password}, nil)
}

// FilesBytes — сколько места заняли файлы пользователя почты в lunlun (кэш на 5 минут).
func (l *Lunlun) FilesBytes(ctx context.Context, mailUser int64) (int64, error) {
	l.mu.Lock()
	if c, ok := l.usage[mailUser]; ok && time.Since(c.at) < 5*time.Minute {
		l.mu.Unlock()
		return c.bytes, nil
	}
	l.mu.Unlock()
	var out struct {
		FilesBytes int64 `json:"files_bytes"`
	}
	if err := l.do(ctx, http.MethodGet, fmt.Sprintf("/internal/v1/usage/%d", mailUser), nil, &out); err != nil {
		return 0, err
	}
	l.mu.Lock()
	if l.usage == nil {
		l.usage = map[int64]cached{}
	}
	l.usage[mailUser] = cached{bytes: out.FilesBytes, at: time.Now()}
	l.mu.Unlock()
	return out.FilesBytes, nil
}
