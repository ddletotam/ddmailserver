package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client of the mail server's bot API (internal/web/bot_api.go).

type mailAPI struct {
	base, secret string
	http         *http.Client
}

func newMailAPI(base, secret string) *mailAPI {
	return &mailAPI{base: strings.TrimRight(base, "/") + "/api/bot/v1", secret: secret, http: &http.Client{Timeout: 20 * time.Second}}
}

// apiError is a refusal the server explained ({"error": code}).
type apiError struct {
	Status int
	Code   string
}

func (e *apiError) Error() string { return fmt.Sprintf("bot api: %d %s", e.Status, e.Code) }

func errCode(err error) string {
	if e, ok := err.(*apiError); ok {
		return e.Code
	}
	return ""
}

type account struct {
	Login    string `json:"login"`
	Address  string `json:"address"`
	Password string `json:"password"`
}

type loginCheck struct {
	Login     string `json:"login"`
	Address   string `json:"address"`
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

func (m *mailAPI) do(ctx context.Context, method, path string, body, out any) error {
	var rd *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("bot api %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return &apiError{Status: resp.StatusCode, Code: e.Error}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (m *mailAPI) getAccount(ctx context.Context, tg int64) (*account, error) {
	var a account
	return &a, m.do(ctx, http.MethodGet, fmt.Sprintf("/accounts/%d", tg), nil, &a)
}

func (m *mailAPI) checkLogin(ctx context.Context, login string) (*loginCheck, error) {
	var c loginCheck
	return &c, m.do(ctx, http.MethodGet, "/logins/"+url.PathEscape(login), nil, &c)
}

func (m *mailAPI) createAccount(ctx context.Context, tg int64, login, lang string) (*account, error) {
	var a account
	return &a, m.do(ctx, http.MethodPost, "/accounts", map[string]any{"tg_user_id": tg, "login": login, "language": lang}, &a)
}

func (m *mailAPI) resetPassword(ctx context.Context, tg int64) (*account, error) {
	var a account
	return &a, m.do(ctx, http.MethodPost, fmt.Sprintf("/accounts/%d/password", tg), nil, &a)
}
