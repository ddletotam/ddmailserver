package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

type sent struct {
	chat int64
	text string
	kb   *keyboard
}

type fakeTG struct {
	sent      []sent
	deleted   []int64
	links     []string
	preAnswer []bool
	refunded  []string
}

func (f *fakeTG) createSubscriptionLink(_ context.Context, _, _, payload string, stars int) (string, error) {
	f.links = append(f.links, fmt.Sprintf("%s/%d", payload, stars))
	return "https://t.me/$invoice", nil
}
func (f *fakeTG) answerPreCheckout(_ context.Context, _ string, ok bool, _ string) error {
	f.preAnswer = append(f.preAnswer, ok)
	return nil
}
func (f *fakeTG) refundStars(_ context.Context, _ int64, charge string) error {
	f.refunded = append(f.refunded, charge)
	return nil
}

func (f *fakeTG) getUpdates(context.Context, int64) ([]tgUpdate, error) { return nil, nil }
func (f *fakeTG) send(_ context.Context, chat int64, text string, kb *keyboard) (int64, error) {
	f.sent = append(f.sent, sent{chat, text, kb})
	return int64(len(f.sent)), nil
}
func (f *fakeTG) deleteMessage(_ context.Context, _, id int64) error {
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *fakeTG) dropKeyboard(context.Context, int64, int64) error { return nil }
func (f *fakeTG) answerCallback(context.Context, string) error     { return nil }
func (f *fakeTG) setCommands(context.Context, []command) error     { return nil }

func (f *fakeTG) last() sent { return f.sent[len(f.sent)-1] }

type fakeAPI struct {
	accounts    map[int64]*account
	taken       map[string]bool
	pending     []notice
	sentNotices []int64
	until       map[string]int64
	payments    []payment
}

func (f *fakeAPI) features(_ context.Context, tg int64) ([]feature, error) {
	if _, ok := f.accounts[tg]; !ok {
		return nil, &apiError{404, "not_found"}
	}
	return []feature{
		{Code: "external_accounts", Title: "Другие ящики", Price: 100, Until: f.until["external_accounts"]},
		{Code: "send_limits", Title: "Отправка без лимитов", Price: 50, Until: f.until["send_limits"]},
	}, nil
}
func (f *fakeAPI) recordPayment(_ context.Context, p payment) (int64, error) {
	if p.Feature != "external_accounts" && p.Feature != "send_limits" {
		return 0, &apiError{400, "unknown_feature"}
	}
	f.payments = append(f.payments, p)
	u := time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	f.until[p.Feature] = u
	return u, nil
}

func (f *fakeAPI) getAccount(_ context.Context, tg int64) (*account, error) {
	if a, ok := f.accounts[tg]; ok {
		return a, nil
	}
	return nil, &apiError{404, "not_found"}
}
func (f *fakeAPI) checkLogin(_ context.Context, login string) (*loginCheck, error) {
	login = strings.ToLower(login)
	c := &loginCheck{Login: login, Address: login + "@lunlun.email", Available: !f.taken[login]}
	if !c.Available {
		c.Reason = "taken"
	}
	return c, nil
}
func (f *fakeAPI) createAccount(_ context.Context, tg int64, login, _ string) (*account, error) {
	if _, ok := f.accounts[tg]; ok {
		return nil, &apiError{409, "registered"}
	}
	if f.taken[login] {
		return nil, &apiError{409, "taken"}
	}
	a := &account{Login: login, Address: login + "@lunlun.email", Password: "Secret123secret4"}
	f.accounts[tg], f.taken[login] = a, true
	return a, nil
}
func (f *fakeAPI) resetPassword(_ context.Context, tg int64) (*account, error) {
	a, ok := f.accounts[tg]
	if !ok {
		return nil, &apiError{404, "not_found"}
	}
	return &account{Login: a.Login, Address: a.Address, Password: "NewSecret123new4"}, nil
}

func (f *fakeAPI) notices(context.Context) ([]notice, error) {
	ns := f.pending
	f.pending = nil
	return ns, nil
}
func (f *fakeAPI) noticeSent(_ context.Context, id int64) error {
	f.sentNotices = append(f.sentNotices, id)
	return nil
}

func TestNoticesDelivered(t *testing.T) {
	b, tg, api := newTestBot()
	api.pending = []notice{{ID: 7, TgUserID: 42, Kind: "send_stopped", Text: "Отправка <остановлена>"}}
	b.deliverNoticesOnce(context.Background())
	if len(tg.sent) != 1 || tg.sent[0].chat != 42 || !strings.Contains(tg.sent[0].text, "&lt;остановлена&gt;") {
		t.Fatalf("sent: %+v", tg.sent)
	}
	if len(api.sentNotices) != 1 || api.sentNotices[0] != 7 {
		t.Fatalf("marked: %v", api.sentNotices)
	}
}

func newTestBot() (*bot, *fakeTG, *fakeAPI) {
	tg := &fakeTG{}
	api := &fakeAPI{accounts: map[int64]*account{}, taken: map[string]bool{"busy": true}, until: map[string]int64{}}
	b := newBot(config{siteURL: "https://lunlun.email", mailHost: "mail.lunlun.email", credsTTL: time.Hour}, tg, api)
	return b, tg, api
}

func msg(user int64, text string) tgUpdate {
	return tgUpdate{Message: &tgMessage{From: &tgUser{ID: user}, Chat: tgChat{ID: user, Type: "private"}, Text: text}}
}

func press(user int64, data string) tgUpdate {
	return tgUpdate{CallbackQuery: &tgCallback{ID: "q", From: tgUser{ID: user}, Data: data,
		Message: &tgMessage{MessageID: 1, Chat: tgChat{ID: user, Type: "private"}}}}
}

// step runs one update; the per-user throttle is reset so steps run back to back.
func step(b *bot, u tgUpdate) {
	b.lastAction = map[int64]time.Time{}
	b.handle(context.Background(), u)
}

func TestSignUpFlow(t *testing.T) {
	b, tg, api := newTestBot()
	const user = 42

	step(b, msg(user, "/start"))
	if kb := tg.last().kb; kb == nil || kb.Rows[0][0].Data != cbAccept || !strings.Contains(tg.last().text, "/terms.html") {
		t.Fatalf("start: %+v", tg.last())
	}
	step(b, press(user, cbAccept))
	if !b.awaiting(user) {
		t.Fatal("after accept the bot waits for a login")
	}
	step(b, msg(user, "busy"))
	if !strings.Contains(tg.last().text, "занят") {
		t.Fatalf("taken login: %q", tg.last().text)
	}
	step(b, msg(user, "Vasya"))
	if kb := tg.last().kb; kb == nil || kb.Rows[0][0].Data != cbCreate+"vasya" {
		t.Fatalf("free login: %+v", tg.last())
	}
	step(b, press(user, cbCreate+"vasya"))
	creds := tg.sent[len(tg.sent)-2]
	if api.accounts[user] == nil || !strings.Contains(creds.text, "Secret123secret4") || b.awaiting(user) {
		t.Fatalf("create: %+v", creds)
	}
	if kb := tg.last().kb; kb == nil || kb.Rows[0][0].Data != cbFeatures {
		t.Fatalf("menu after sign-up: %+v", tg.last())
	}

	// Back again: the account, not the terms.
	step(b, msg(user, "/start"))
	if !strings.Contains(tg.last().text, "vasya@lunlun.email") {
		t.Fatalf("returning user: %q", tg.last().text)
	}
	// A second free address is refused.
	step(b, press(user, cbCreate+"other"))
	if api.taken["other"] {
		t.Fatal("second account created")
	}

	step(b, msg(user, "/password"))
	step(b, press(user, cbPasswordOK))
	if !strings.Contains(tg.last().text, "NewSecret123new4") {
		t.Fatalf("reset: %q", tg.last().text)
	}
}

func TestAllowList(t *testing.T) {
	b, tg, _ := newTestBot()
	b.cfg.allow = map[int64]bool{1: true}
	step(b, msg(2, "/start"))
	if len(tg.sent) != 0 {
		t.Fatal("answered a user outside the allow-list")
	}
	step(b, msg(1, "/start"))
	if len(tg.sent) != 1 {
		t.Fatal("did not answer an allowed user")
	}
}

func TestGroupsIgnored(t *testing.T) {
	b, tg, _ := newTestBot()
	u := msg(7, "/start")
	u.Message.Chat.Type = "group"
	step(b, u)
	if len(tg.sent) != 0 {
		t.Fatalf("answered in a group: %+v", tg.sent)
	}
}

func TestPaymentFlow(t *testing.T) {
	b, tg, api := newTestBot()
	const user = 5
	api.accounts[user] = &account{Login: "pay", Address: "pay@lunlun.email"}

	step(b, msg(user, "/features"))
	if kb := tg.last().kb; kb == nil || len(kb.Rows) != 2 || kb.Rows[0][0].Data != cbBuy+"external_accounts" {
		t.Fatalf("features: %+v", tg.last())
	}
	step(b, press(user, cbBuy+"external_accounts"))
	if len(tg.links) != 1 || tg.links[0] != "f:external_accounts/100" || tg.last().kb.Rows[0][0].URL == "" {
		t.Fatalf("invoice: %v %+v", tg.links, tg.last())
	}

	pre := func(payload string, amount int) bool {
		b.handle(context.Background(), tgUpdate{PreCheckout: &tgPreCheckout{ID: "p", From: tgUser{ID: user}, Currency: "XTR", TotalAmount: amount, InvoicePayload: payload}})
		return tg.preAnswer[len(tg.preAnswer)-1]
	}
	if !pre("f:external_accounts", 100) || pre("f:external_accounts", 99) || pre("f:nothing", 100) {
		t.Fatalf("pre-checkout answers: %v", tg.preAnswer)
	}

	paidMsg := func(payload, charge string) tgUpdate {
		return tgUpdate{Message: &tgMessage{From: &tgUser{ID: user}, Chat: tgChat{ID: user, Type: "private"},
			SuccessfulPayment: &tgSuccessfulPayment{Currency: "XTR", TotalAmount: 100, InvoicePayload: payload,
				TelegramPaymentChargeID: charge, IsRecurring: true, IsFirstRecurring: true}}}
	}
	step(b, paidMsg("f:external_accounts", "ch1"))
	if len(api.payments) != 1 || api.payments[0].Recurring || !strings.Contains(tg.last().text, "Подключено до") {
		t.Fatalf("paid: %+v %q", api.payments, tg.last().text)
	}
	// Already paid: no buy button for it any more.
	step(b, msg(user, "/features"))
	if kb := tg.last().kb; kb == nil || len(kb.Rows) != 1 || kb.Rows[0][0].Data != cbBuy+"send_limits" {
		t.Fatalf("features after payment: %+v", tg.last())
	}
	// A payment the server refuses is refunded.
	step(b, paidMsg("f:nothing", "ch2"))
	if len(tg.refunded) != 1 || tg.refunded[0] != "ch2" {
		t.Fatalf("refunds: %v", tg.refunded)
	}
}
