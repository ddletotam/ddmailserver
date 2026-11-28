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

type notice struct {
	ID       int64  `json:"id"`
	TgUserID int64  `json:"tg_user_id"`
	Kind     string `json:"kind"`
	Text     string `json:"text"`
}

func (m *mailAPI) notices(ctx context.Context) ([]notice, error) {
	var out struct {
		Notices []notice `json:"notices"`
	}
	// Not «return out.Notices, m.do(...)»: the operands are evaluated left
	// to right, so that would return the list before the request filled it.
	err := m.do(ctx, http.MethodGet, "/notices", nil, &out)
	return out.Notices, err
}

func (m *mailAPI) noticeSent(ctx context.Context, id int64) error {
	return m.do(ctx, http.MethodPost, fmt.Sprintf("/notices/%d/sent", id), nil, nil)
}

type feature struct {
	Code        string `json:"code"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Price       int    `json:"price"`
	Status      string `json:"status"`
	PaidUntil   int64  `json:"paid_until"` // ms
	Working     bool   `json:"working"`
}

type billing struct {
	Balance  int       `json:"balance"`
	Features []feature `json:"features"`
}

func (m *mailAPI) billing(ctx context.Context, tg int64) (*billing, error) {
	var b billing
	err := m.do(ctx, http.MethodGet, fmt.Sprintf("/accounts/%d/billing", tg), nil, &b)
	return &b, err
}

type enableResult struct {
	Balance   int   `json:"balance"`
	PaidUntil int64 `json:"paid_until"`
}

// enable subscribes to a feature from the balance; errCode "insufficient" —
// not enough Stars.
func (m *mailAPI) enable(ctx context.Context, tg int64, code string) (*enableResult, error) {
	var r enableResult
	err := m.do(ctx, http.MethodPost, fmt.Sprintf("/accounts/%d/features/%s", tg, url.PathEscape(code)), nil, &r)
	return &r, err
}

func (m *mailAPI) cancel(ctx context.Context, tg int64, code string) (int64, error) {
	var r struct {
		PaidUntil int64 `json:"paid_until"`
	}
	err := m.do(ctx, http.MethodDelete, fmt.Sprintf("/accounts/%d/features/%s", tg, url.PathEscape(code)), nil, &r)
	return r.PaidUntil, err
}

func (m *mailAPI) topUp(ctx context.Context, tg int64, amount int, charge string) (int, error) {
	var r struct {
		Balance int `json:"balance"`
	}
	err := m.do(ctx, http.MethodPost, "/topups", map[string]any{"tg_user_id": tg, "amount": amount, "charge_id": charge}, &r)
	return r.Balance, err
}

type topUp struct {
	Amount   int    `json:"amount"`
	ChargeID string `json:"charge_id"`
}

func (m *mailAPI) refundable(ctx context.Context, tg int64) ([]topUp, error) {
	var r struct {
		TopUps []topUp `json:"topups"`
	}
	err := m.do(ctx, http.MethodGet, fmt.Sprintf("/accounts/%d/refunds", tg), nil, &r)
	return r.TopUps, err
}

func (m *mailAPI) reserveRefund(ctx context.Context, tg int64, charge string) (int, error) {
	var r struct {
		Amount int `json:"amount"`
	}
	err := m.do(ctx, http.MethodPost, fmt.Sprintf("/accounts/%d/refunds/%s", tg, url.PathEscape(charge)), nil, &r)
	return r.Amount, err
}

func (m *mailAPI) cancelRefund(ctx context.Context, tg int64, charge string) error {
	return m.do(ctx, http.MethodDelete, fmt.Sprintf("/accounts/%d/refunds/%s", tg, url.PathEscape(charge)), nil, nil)
}
