package main

import (
	"context"
	"fmt"
	"html"
	"strconv"
	"strings"
)

// Mailing lists (docs/lunlun-bot-plan.md, «Рассылки»): the owner shares the
// link t.me/<bot>?start=l_<code>; here a person subscribes themselves — their
// own address of the service at once, any other address after confirming it
// by the letter the server sends. /lists — one's subscriptions, unsubscribe.

const listStartPrefix = "l_"

const (
	cbListOwn   = "lsub:"   // + code: to my address of the service
	cbListOther = "lsubo:"  // + code: to another address
	cbListOff   = "lunsub:" // + subscription id
)

func (b *bot) setAwaitingEmail(user int64, code string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if code == "" {
		delete(b.awaitingEmail, user)
	} else {
		b.awaitingEmail[user] = code
	}
}

func (b *bot) awaitedList(user int64) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.awaitingEmail[user]
}

// offerList: the subscribe link was opened.
func (b *bot) offerList(ctx context.Context, chat int64, from tgUser, code string) {
	l, err := b.api.listInfo(ctx, code)
	switch {
	case errCode(err) == "not_found":
		b.say(ctx, chat, "Такой рассылки нет — возможно, её удалили.", nil)
		return
	case err != nil:
		b.fail(ctx, chat, err)
		return
	case !l.Enabled:
		b.say(ctx, chat, "Эта рассылка сейчас не принимает подписчиков.", nil)
		return
	case l.Full:
		b.say(ctx, chat, "В этой рассылке закончились места для подписчиков. Напишите её автору: "+html.EscapeString(l.Address), nil)
		return
	}
	text := fmt.Sprintf("Рассылка <b>%s</b> (<code>%s</code>).\n\nПодписаться? Отписаться можно в любой момент — здесь, командой /lists, или по ссылке в любом письме.",
		html.EscapeString(l.Title), html.EscapeString(l.Address))
	a, aerr := b.api.getAccount(ctx, from.ID)
	if aerr == nil {
		b.say(ctx, chat, text, &keyboard{Rows: [][]button{
			{{Text: "На " + a.Address, Data: cbListOwn + code}},
			{{Text: "На другой адрес", Data: cbListOther + code}},
		}})
		return
	}
	if errCode(aerr) == "banned" {
		b.say(ctx, chat, bannedText, nil)
		return
	}
	b.setAwaitingEmail(from.ID, code)
	b.say(ctx, chat, text+"\n\nПришлите адрес почты, на который присылать письма. На него придёт письмо со ссылкой для подтверждения.", nil)
}

func (b *bot) subscribeTo(ctx context.Context, chat int64, from tgUser, code, email string, own bool) {
	r, err := b.api.subscribe(ctx, code, from.ID, email, own)
	switch errCode(err) {
	case "":
		if err != nil {
			b.fail(ctx, chat, err)
			return
		}
		b.setAwaitingEmail(from.ID, "")
		if r.Status == "active" {
			b.say(ctx, chat, fmt.Sprintf("Готово: рассылка <b>%s</b> будет приходить на <code>%s</code>.",
				html.EscapeString(r.Title), html.EscapeString(r.Email)), nil)
			return
		}
		b.say(ctx, chat, fmt.Sprintf("На <code>%s</code> отправлено письмо со ссылкой — откройте её, чтобы подтвердить подписку на <b>%s</b>. Без подтверждения писем не будет.",
			html.EscapeString(r.Email), html.EscapeString(r.Title)), nil)
	case "invalid_email":
		b.setAwaitingEmail(from.ID, code)
		b.say(ctx, chat, "Это не похоже на адрес почты. Пришлите адрес вида <code>имя@example.com</code>.", nil)
	case "already":
		b.setAwaitingEmail(from.ID, "")
		b.say(ctx, chat, "Этот адрес уже подписан на рассылку.", nil)
	case "pending":
		b.setAwaitingEmail(from.ID, "")
		b.say(ctx, chat, "Письмо для подтверждения на этот адрес уже отправлено — проверьте почту, в том числе папку «Спам».", nil)
	case "too_many":
		b.setAwaitingEmail(from.ID, "")
		b.say(ctx, chat, "Сегодня подписок уже слишком много. Попробуйте завтра.", nil)
	case "full":
		b.setAwaitingEmail(from.ID, "")
		b.say(ctx, chat, "В этой рассылке закончились места для подписчиков.", nil)
	case "off":
		b.setAwaitingEmail(from.ID, "")
		b.say(ctx, chat, "Эта рассылка сейчас не принимает подписчиков.", nil)
	case "not_found":
		b.setAwaitingEmail(from.ID, "")
		b.say(ctx, chat, "Такой рассылки нет — возможно, её удалили.", nil)
	case "banned":
		b.setAwaitingEmail(from.ID, "")
		b.say(ctx, chat, bannedText, nil)
	default:
		b.fail(ctx, chat, err)
	}
}

func (b *bot) showListSubscriptions(ctx context.Context, chat int64, from tgUser) {
	subs, err := b.api.listSubscriptions(ctx, from.ID)
	if err != nil {
		b.fail(ctx, chat, err)
		return
	}
	if len(subs) == 0 {
		b.say(ctx, chat, "Вы не подписаны ни на одну рассылку.", nil)
		return
	}
	var sb strings.Builder
	sb.WriteString("<b>Ваши рассылки</b>\n")
	kb := &keyboard{}
	for _, s := range subs {
		state := ""
		if s.Status == "pending" {
			state = " — ждёт подтверждения по ссылке из письма"
		}
		fmt.Fprintf(&sb, "\n<b>%s</b> (<code>%s</code>) → <code>%s</code>%s", html.EscapeString(s.Title),
			html.EscapeString(s.Address), html.EscapeString(s.Email), state)
		kb.Rows = append(kb.Rows, []button{{Text: "Отписаться от «" + s.Title + "»", Data: cbListOff + strconv.FormatInt(s.ID, 10)}})
	}
	b.say(ctx, chat, sb.String(), kb)
}

func (b *bot) unsubscribeList(ctx context.Context, chat int64, from tgUser, idText string) {
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		return
	}
	err = b.api.unsubscribe(ctx, from.ID, id)
	switch {
	case err == nil:
		b.say(ctx, chat, "Вы отписаны — писем этой рассылки больше не будет.", nil)
	case errCode(err) == "not_found":
		b.say(ctx, chat, "Этой подписки уже нет.", nil)
	default:
		b.fail(ctx, chat, err)
	}
}
