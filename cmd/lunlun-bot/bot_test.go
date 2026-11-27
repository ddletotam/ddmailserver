package main

import (
	"context"
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
	sent    []sent
	deleted []int64
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
	accounts map[int64]*account
	taken    map[string]bool
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

func newTestBot() (*bot, *fakeTG, *fakeAPI) {
	tg := &fakeTG{}
	api := &fakeAPI{accounts: map[int64]*account{}, taken: map[string]bool{"busy": true}}
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
	if api.accounts[user] == nil || !strings.Contains(tg.last().text, "Secret123secret4") || b.awaiting(user) {
		t.Fatalf("create: %+v", tg.last())
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
