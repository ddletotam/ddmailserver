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

func (f *fakeTG) sendStarsInvoice(_ context.Context, _ int64, _, _, payload string, stars int) error {
	f.links = append(f.links, fmt.Sprintf("%s/%d", payload, stars))
	return nil
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
	balance     int
	topups      map[string]int
	enabled     map[string]bool
	reserved    []string
}

func (f *fakeAPI) billing(_ context.Context, tg int64) (*billing, error) {
	if _, ok := f.accounts[tg]; !ok {
		return nil, &apiError{404, "not_found"}
	}
	f1 := feature{Code: "external_accounts", Title: "Другие ящики", Price: 50, PaidUntil: time.Now().Add(720 * time.Hour).UnixMilli()}
	if f.enabled["external_accounts"] {
		f1.Status, f1.Working = "active", true
	}
	return &billing{Balance: f.balance, Features: []feature{f1, {Code: "mailbox", Title: "Ещё ящик", Price: 25}}}, nil
}
func (f *fakeAPI) enable(_ context.Context, _ int64, code string) (*enableResult, error) {
	if f.balance < 50 {
		return nil, &apiError{402, "insufficient"}
	}
	f.balance -= 50
	f.enabled[code] = true
	return &enableResult{Balance: f.balance, PaidUntil: time.Now().Add(720 * time.Hour).UnixMilli()}, nil
}
func (f *fakeAPI) cancel(_ context.Context, _ int64, code string) (int64, error) {
	if !f.enabled[code] {
		return 0, &apiError{404, "not_subscribed"}
	}
	f.enabled[code] = false
	return time.Now().UnixMilli(), nil
}
func (f *fakeAPI) topUp(_ context.Context, tg int64, amount int, charge string) (int, error) {
	if _, ok := f.accounts[tg]; !ok {
		return 0, &apiError{404, "not_found"}
	}
	if _, seen := f.topups[charge]; !seen {
		f.topups[charge] = amount
		f.balance += amount
	}
	return f.balance, nil
}
func (f *fakeAPI) refundable(context.Context, int64) ([]topUp, error) {
	var out []topUp
	for c, a := range f.topups {
		out = append(out, topUp{Amount: a, ChargeID: c})
	}
	return out, nil
}
func (f *fakeAPI) reserveRefund(_ context.Context, _ int64, charge string) (int, error) {
	a, ok := f.topups[charge]
	if !ok {
		return 0, &apiError{409, "not_refundable"}
	}
	delete(f.topups, charge)
	f.balance -= a
	f.reserved = append(f.reserved, charge)
	return a, nil
}
func (f *fakeAPI) cancelRefund(context.Context, int64, string) error { return nil }

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
	api := &fakeAPI{accounts: map[int64]*account{}, taken: map[string]bool{"busy": true}, topups: map[string]int{}, enabled: map[string]bool{}}
	b := newBot(config{siteURL: "https://lunlun.email", mailHost: "mail.lunlun.email", credsTTL: time.Hour}, tg, api)
	b.del = loadDeletions("") // in memory
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

func TestBalanceFlow(t *testing.T) {
	b, tg, api := newTestBot()
	const user = 5
	api.accounts[user] = &account{Login: "pay", Address: "pay@lunlun.email"}

	step(b, msg(user, "/balance"))
	if kb := tg.last().kb; kb == nil || kb.Rows[0][0].Data != cbEnable+"external_accounts" || !strings.Contains(tg.last().text, "Баланс: 0") {
		t.Fatalf("balance: %+v", tg.last())
	}
	step(b, press(user, cbEnable+"external_accounts"))
	if kb := tg.last().kb; kb == nil || kb.Rows[0][0].Data != cbTopUp+"50" {
		t.Fatalf("insufficient: %+v", tg.last())
	}
	step(b, press(user, cbTopUp+"100"))
	if len(tg.links) != 1 || tg.links[0] != "topup:100/100" {
		t.Fatalf("invoice: %v", tg.links)
	}
	pre := func(payload string, amount int) bool {
		b.handle(context.Background(), tgUpdate{PreCheckout: &tgPreCheckout{ID: "p", From: tgUser{ID: user}, Currency: "XTR", TotalAmount: amount, InvoicePayload: payload}})
		return tg.preAnswer[len(tg.preAnswer)-1]
	}
	if !pre("topup:100", 100) || pre("topup:100", 99) || pre("topup:7", 7) {
		t.Fatalf("pre-checkout answers: %v", tg.preAnswer)
	}
	paidMsg := func(from int64, charge string, amount int) tgUpdate {
		return tgUpdate{Message: &tgMessage{From: &tgUser{ID: from}, Chat: tgChat{ID: from, Type: "private"},
			SuccessfulPayment: &tgSuccessfulPayment{Currency: "XTR", TotalAmount: amount, InvoicePayload: fmt.Sprintf("topup:%d", amount), TelegramPaymentChargeID: charge}}}
	}
	step(b, paidMsg(user, "ch1", 100))
	if api.balance != 100 || !strings.Contains(tg.last().text, "100 ⭐") {
		t.Fatalf("topped up: %d %q", api.balance, tg.last().text)
	}
	step(b, press(user, cbEnable+"external_accounts"))
	if !api.enabled["external_accounts"] || api.balance != 50 {
		t.Fatalf("enable: %v %d", api.enabled, api.balance)
	}
	step(b, msg(user, "/balance"))
	if kb := tg.last().kb; kb == nil || kb.Rows[0][0].Data != cbCancel+"external_accounts" {
		t.Fatalf("balance with a feature on: %+v", tg.last())
	}
	step(b, press(user, cbCancel+"external_accounts"))
	if api.enabled["external_accounts"] {
		t.Fatal("not cancelled")
	}
	step(b, press(user, cbRefundDo+"ch1"))
	if len(api.reserved) != 1 || len(tg.refunded) != 1 || tg.refunded[0] != "ch1" {
		t.Fatalf("refund: %v %v", api.reserved, tg.refunded)
	}
	// A top-up from someone without an account is given back.
	step(b, paidMsg(999, "stray", 50))
	if tg.refunded[len(tg.refunded)-1] != "stray" {
		t.Fatalf("stray top-up not refunded: %v", tg.refunded)
	}
}

func TestDeletionsSurviveRestart(t *testing.T) {
	path := t.TempDir() + "/deletions.json"
	d := loadDeletions(path)
	now := time.Now()
	d.add(1, 10, now.Add(-time.Minute))
	d.add(1, 11, now.Add(time.Hour))
	// «Restart»: a fresh process reads the file.
	d2 := loadDeletions(path)
	due := d2.due(now)
	if len(due) != 1 || due[0].Message != 10 {
		t.Fatalf("due after restart: %+v", due)
	}
	if left := loadDeletions(path).due(now.Add(2 * time.Hour)); len(left) != 1 || left[0].Message != 11 {
		t.Fatalf("left: %+v", left)
	}
}
