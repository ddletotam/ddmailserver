package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"strings"
	"time"
)

// Paid features: each a monthly Stars subscription (docs/lunlun-bot-plan.md).
// Prices and the catalog live on the mail server; the bot only sells.

const payloadPrefix = "f:"

var moscow = time.FixedZone("MSK", 3*3600)

func dateRU(ms int64) string {
	return time.UnixMilli(ms).In(moscow).Format("02.01.2006")
}

func (b *bot) showFeatures(ctx context.Context, chat int64, from tgUser) {
	fs, err := b.api.features(ctx, from.ID)
	switch errCode(err) {
	case "":
		if err != nil {
			b.fail(ctx, chat, err)
			return
		}
	case "not_found":
		b.start(ctx, chat, from)
		return
	case "banned":
		b.say(ctx, chat, bannedText, nil)
		return
	default:
		b.fail(ctx, chat, err)
		return
	}
	if len(fs) == 0 {
		b.say(ctx, chat, "Платных возможностей пока нет.", nil)
		return
	}
	var sb strings.Builder
	sb.WriteString("<b>Платные возможности</b>\nКаждая подключается отдельно и оплачивается раз в месяц звёздами Telegram. Отменить можно в любой момент — возможность работает до конца оплаченного месяца.\n")
	kb := &keyboard{}
	now := time.Now().UnixMilli()
	for _, f := range fs {
		fmt.Fprintf(&sb, "\n<b>%s</b> — %d ⭐ в месяц\n%s\n", html.EscapeString(f.Title), f.Price, html.EscapeString(f.Description))
		if f.Until > now {
			fmt.Fprintf(&sb, "✅ Подключено до %s\n", dateRU(f.Until))
			continue
		}
		if f.Until > 0 {
			fmt.Fprintf(&sb, "Закончилось %s\n", dateRU(f.Until))
		}
		kb.Rows = append(kb.Rows, []button{{Text: fmt.Sprintf("Подключить «%s» — %d ⭐", f.Title, f.Price), Data: cbBuy + f.Code}})
	}
	if len(kb.Rows) == 0 {
		kb = nil
	}
	b.say(ctx, chat, sb.String(), kb)
}

// buy sends a link to a monthly subscription for one feature.
func (b *bot) buy(ctx context.Context, chat int64, from tgUser, code string) {
	fs, err := b.api.features(ctx, from.ID)
	if err != nil {
		b.fail(ctx, chat, err)
		return
	}
	for _, f := range fs {
		if f.Code != code {
			continue
		}
		if f.Until > time.Now().UnixMilli() {
			b.say(ctx, chat, fmt.Sprintf("«%s» уже подключено до %s.", html.EscapeString(f.Title), dateRU(f.Until)), nil)
			return
		}
		link, err := b.tg.createSubscriptionLink(ctx, f.Title, f.Description, payloadPrefix+f.Code, f.Price)
		if err != nil {
			b.fail(ctx, chat, err)
			return
		}
		b.say(ctx, chat, fmt.Sprintf("«%s» — %d ⭐ в месяц. Подписка продлевается сама; отменить её можно в Telegram, в разделе звёзд и подписок.",
			html.EscapeString(f.Title), f.Price),
			&keyboard{Rows: [][]button{{{Text: fmt.Sprintf("Оплатить %d ⭐", f.Price), URL: link}}}})
		return
	}
	b.say(ctx, chat, "Эта возможность сейчас не продаётся. Откройте список заново: /features", nil)
}

// preCheckout confirms a payment only for a feature on sale at that price,
// and only for someone with an account.
func (b *bot) preCheckout(ctx context.Context, q *tgPreCheckout) {
	ok, why := b.checkOrder(ctx, q)
	if err := b.tg.answerPreCheckout(ctx, q.ID, ok, why); err != nil {
		log.Printf("answerPreCheckoutQuery: %v", err)
	}
}

func (b *bot) checkOrder(ctx context.Context, q *tgPreCheckout) (bool, string) {
	code, ok := strings.CutPrefix(q.InvoicePayload, payloadPrefix)
	if !ok || q.Currency != "XTR" {
		return false, "Этот счёт устарел. Откройте /features и подключите заново."
	}
	fs, err := b.api.features(ctx, q.From.ID)
	if err != nil {
		if errCode(err) == "not_found" {
			return false, "Сначала зарегистрируйте адрес: /start"
		}
		log.Printf("pre-checkout %s: %v", q.ID, err)
		return false, "Не получилось проверить заказ. Попробуйте через пару минут."
	}
	for _, f := range fs {
		if f.Code == code {
			if f.Price != q.TotalAmount {
				return false, "Цена изменилась. Откройте /features и подключите заново."
			}
			return true, ""
		}
	}
	return false, "Эта возможность сейчас не продаётся."
}

// paid records a successful payment (first charge or monthly renewal). If the
// server refuses it, the Stars go back at once.
func (b *bot) paid(ctx context.Context, chat int64, from tgUser, p *tgSuccessfulPayment) {
	code, _ := strings.CutPrefix(p.InvoicePayload, payloadPrefix)
	until, err := b.api.recordPayment(ctx, payment{
		TgUserID: from.ID, Feature: code, Amount: p.TotalAmount, ChargeID: p.TelegramPaymentChargeID,
		Recurring: p.IsRecurring && !p.IsFirstRecurring, ExpiresAt: p.SubscriptionExpirationDate,
	})
	if err != nil {
		log.Printf("payment %s from %d (%s, %d stars): %v", p.TelegramPaymentChargeID, from.ID, code, p.TotalAmount, err)
		switch errCode(err) {
		case "unknown_feature", "wrong_amount", "not_found", "banned":
			if rerr := b.tg.refundStars(ctx, from.ID, p.TelegramPaymentChargeID); rerr != nil {
				log.Printf("refund %s: %v", p.TelegramPaymentChargeID, rerr)
				b.say(ctx, chat, "Оплату не удалось применить. Мы вернём звёзды вручную — напишите /paysupport.", nil)
				return
			}
			b.say(ctx, chat, "Оплату не удалось применить, звёзды возвращены.", nil)
		default:
			// Server unreachable: the charge is in Telegram, nothing is lost —
			// it is applied by hand from the log.
			b.say(ctx, chat, "Оплата получена, но подключение задерживается. Мы подключим вручную; если не заработает в течение суток — /paysupport.", nil)
		}
		return
	}
	if p.IsRecurring && !p.IsFirstRecurring {
		b.say(ctx, chat, fmt.Sprintf("Подписка продлена до %s.", dateRU(until)), nil)
		return
	}
	b.say(ctx, chat, fmt.Sprintf("Готово! Подключено до %s, дальше подписка продлевается сама. Список возможностей — /features.", dateRU(until)), nil)
}

func (b *bot) paySupportText() string {
	return "Вопросы по оплате и возвратам — на " + b.cfg.supportEmail +
		". Укажите ваш адрес на lunlun.email и что произошло. Подписку на возможность можно отменить в Telegram, в разделе звёзд и подписок: она проработает до конца оплаченного месяца."
}
