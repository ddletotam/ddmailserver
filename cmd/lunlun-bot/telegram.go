package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// A minimal Telegram Bot API client: only the calls the bot makes.

type telegram struct {
	base string
	http *http.Client
}

func newTelegram(token string) *telegram {
	return &telegram{
		base: "https://api.telegram.org/bot" + token + "/",
		http: &http.Client{Timeout: 70 * time.Second},
	}
}

type tgUser struct {
	ID           int64  `json:"id"`
	LanguageCode string `json:"language_code"`
	IsBot        bool   `json:"is_bot"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type tgMessage struct {
	MessageID         int64                `json:"message_id"`
	From              *tgUser              `json:"from"`
	Chat              tgChat               `json:"chat"`
	Text              string               `json:"text"`
	SuccessfulPayment *tgSuccessfulPayment `json:"successful_payment"`
}

type tgSuccessfulPayment struct {
	Currency                   string `json:"currency"`
	TotalAmount                int    `json:"total_amount"`
	InvoicePayload             string `json:"invoice_payload"`
	SubscriptionExpirationDate int64  `json:"subscription_expiration_date"`
	IsRecurring                bool   `json:"is_recurring"`
	IsFirstRecurring           bool   `json:"is_first_recurring"`
	TelegramPaymentChargeID    string `json:"telegram_payment_charge_id"`
}

type tgPreCheckout struct {
	ID             string `json:"id"`
	From           tgUser `json:"from"`
	Currency       string `json:"currency"`
	TotalAmount    int    `json:"total_amount"`
	InvoicePayload string `json:"invoice_payload"`
}

type tgCallback struct {
	ID      string     `json:"id"`
	From    tgUser     `json:"from"`
	Message *tgMessage `json:"message"`
	Data    string     `json:"data"`
}

type tgUpdate struct {
	UpdateID      int64          `json:"update_id"`
	Message       *tgMessage     `json:"message"`
	CallbackQuery *tgCallback    `json:"callback_query"`
	PreCheckout   *tgPreCheckout `json:"pre_checkout_query"`
}

type button struct {
	Text string `json:"text"`
	Data string `json:"callback_data,omitempty"`
	URL  string `json:"url,omitempty"`
}

type keyboard struct {
	Rows [][]button `json:"inline_keyboard"`
}

// call posts a method and decodes "result" into out (may be nil).
func (t *telegram) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		// The URL carries the token: never let it reach a log line.
		return fmt.Errorf("%s: request failed", method)
	}
	defer resp.Body.Close()
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("%s: decode: %w", method, err)
	}
	if !env.OK {
		return fmt.Errorf("%s: %s", method, env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func (t *telegram) getUpdates(ctx context.Context, offset int64) ([]tgUpdate, error) {
	var ups []tgUpdate
	err := t.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": 50, "allowed_updates": []string{"message", "callback_query", "pre_checkout_query"},
	}, &ups)
	return ups, err
}

// send posts an HTML message and returns its id.
func (t *telegram) send(ctx context.Context, chatID int64, text string, kb *keyboard) (int64, error) {
	p := map[string]any{"chat_id": chatID, "text": text, "parse_mode": "HTML", "link_preview_options": map[string]bool{"is_disabled": true}}
	if kb != nil {
		p["reply_markup"] = kb
	}
	var m tgMessage
	err := t.call(ctx, "sendMessage", p, &m)
	return m.MessageID, err
}

func (t *telegram) deleteMessage(ctx context.Context, chatID, messageID int64) error {
	return t.call(ctx, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": messageID}, nil)
}

// dropKeyboard removes the buttons under a message once one was pressed.
func (t *telegram) dropKeyboard(ctx context.Context, chatID, messageID int64) error {
	return t.call(ctx, "editMessageReplyMarkup", map[string]any{
		"chat_id": chatID, "message_id": messageID, "reply_markup": keyboard{Rows: [][]button{}},
	}, nil)
}

func (t *telegram) answerCallback(ctx context.Context, id string) error {
	return t.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id}, nil)
}

type command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

func (t *telegram) setCommands(ctx context.Context, cmds []command) error {
	return t.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}

// subscriptionPeriod is the only period Telegram allows for Stars subscriptions.
const subscriptionPeriod = 30 * 24 * 3600

// createSubscriptionLink returns a link to a monthly Stars subscription.
func (t *telegram) createSubscriptionLink(ctx context.Context, title, description, payload string, stars int) (string, error) {
	var link string
	err := t.call(ctx, "createInvoiceLink", map[string]any{
		"title": title, "description": description, "payload": payload, "currency": "XTR",
		"prices": []map[string]any{{"label": title, "amount": stars}}, "subscription_period": subscriptionPeriod,
	}, &link)
	return link, err
}

func (t *telegram) answerPreCheckout(ctx context.Context, id string, ok bool, errText string) error {
	p := map[string]any{"pre_checkout_query_id": id, "ok": ok}
	if !ok {
		p["error_message"] = errText
	}
	return t.call(ctx, "answerPreCheckoutQuery", p, nil)
}

func (t *telegram) refundStars(ctx context.Context, user int64, chargeID string) error {
	return t.call(ctx, "refundStarPayment", map[string]any{"user_id": user, "telegram_payment_charge_id": chargeID}, nil)
}
