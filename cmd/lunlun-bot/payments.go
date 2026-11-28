package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"strconv"
	"strings"
	"time"
)

// Balance (docs/lunlun-bot-plan.md, решения 2026-11-28): the user tops up a
// balance in Stars here; paid features are subscribed to from the balance and
// renewed from it every 30 days by the server. Unspent top-ups come back on
// request (/refund).

const topUpPrefix = "topup:"

// topUpAmounts are the sums offered for a top-up.
var topUpAmounts = []int{50, 100, 250, 500}

var moscow = time.FixedZone("MSK", 3*3600)

func dateRU(ms int64) string {
	return time.UnixMilli(ms).In(moscow).Format("02.01.2006")
}

// apiTrouble answers the common refusals of the API; true — handled.
func (b *bot) apiTrouble(ctx context.Context, chat int64, from tgUser, err error) bool {
	switch errCode(err) {
	case "":
		if err == nil {
			return false
		}
		b.fail(ctx, chat, err)
	case "not_found":
		b.start(ctx, chat, from)
	case "banned":
		b.say(ctx, chat, bannedText, nil)
	default:
		b.fail(ctx, chat, err)
	}
	return true
}

func (b *bot) showBalance(ctx context.Context, chat int64, from tgUser) {
	bl, err := b.api.billing(ctx, from.ID)
	if b.apiTrouble(ctx, chat, from, err) {
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Баланс: %d ⭐</b>\n\nКаждая возможность подключается отдельно и оплачивается с баланса раз в месяц. Отключить можно в любой момент — она проработает до конца оплаченного месяца.\n", bl.Balance)
	kb := &keyboard{}
	for _, f := range bl.Features {
		fmt.Fprintf(&sb, "\n<b>%s</b> — %d ⭐ в месяц\n%s\n", html.EscapeString(f.Title), f.Price, html.EscapeString(f.Description))
		switch {
		case f.Code == "mailbox":
			sb.WriteString("Подключается в кабинете при создании ящика.\n")
		case f.Status == "active" && f.Working:
			fmt.Fprintf(&sb, "✅ Подключено, следующее списание %s\n", dateRU(f.PaidUntil))
			kb.Rows = append(kb.Rows, []button{{Text: "Отключить «" + f.Title + "»", Data: cbCancel + f.Code}})
		case f.Status == "cancelled" && f.Working:
			fmt.Fprintf(&sb, "Отключено, работает до %s\n", dateRU(f.PaidUntil))
			kb.Rows = append(kb.Rows, []button{{Text: "Возобновить «" + f.Title + "»", Data: cbEnable + f.Code}})
		default:
			kb.Rows = append(kb.Rows, []button{{Text: fmt.Sprintf("Подключить «%s» — %d ⭐", f.Title, f.Price), Data: cbEnable + f.Code}})
		}
	}
	row := []button{}
	for _, a := range topUpAmounts {
		row = append(row, button{Text: fmt.Sprintf("+%d ⭐", a), Data: cbTopUp + strconv.Itoa(a)})
	}
	kb.Rows = append(kb.Rows, row)
	kb.Rows = append(kb.Rows, []button{{Text: "Вернуть неизрасходованное", Data: cbRefund}})
	b.say(ctx, chat, sb.String(), kb)
}

func (b *bot) enable(ctx context.Context, chat int64, from tgUser, code string) {
	r, err := b.api.enable(ctx, from.ID, code)
	if errCode(err) == "insufficient" {
		b.say(ctx, chat, "На балансе не хватает звёзд. Пополните его и подключите снова.", b.topUpKeyboard())
		return
	}
	if b.apiTrouble(ctx, chat, from, err) {
		return
	}
	b.say(ctx, chat, fmt.Sprintf("Подключено. Следующее списание %s, на балансе %d ⭐.", dateRU(r.PaidUntil), r.Balance), nil)
}

func (b *bot) cancelFeature(ctx context.Context, chat int64, from tgUser, code string) {
	until, err := b.api.cancel(ctx, from.ID, code)
	if errCode(err) == "not_subscribed" {
		b.say(ctx, chat, "Эта возможность не подключена.", nil)
		return
	}
	if b.apiTrouble(ctx, chat, from, err) {
		return
	}
	b.say(ctx, chat, fmt.Sprintf("Отключено. Возможность проработает до %s, списаний больше не будет.", dateRU(until)), nil)
}

func (b *bot) topUpKeyboard() *keyboard {
	row := []button{}
	for _, a := range topUpAmounts {
		row = append(row, button{Text: fmt.Sprintf("+%d ⭐", a), Data: cbTopUp + strconv.Itoa(a)})
	}
	return &keyboard{Rows: [][]button{row}}
}

func validTopUp(n int) bool {
	for _, a := range topUpAmounts {
		if a == n {
			return true
		}
	}
	return false
}

func (b *bot) askTopUp(ctx context.Context, chat int64, from tgUser, amount string) {
	n, err := strconv.Atoi(amount)
	if err != nil || !validTopUp(n) {
		return
	}
	if err := b.tg.sendStarsInvoice(ctx, chat, fmt.Sprintf("Пополнение на %d ⭐", n),
		"Звёзды зачисляются на баланс lunlun.email. Неизрасходованное можно вернуть.", topUpPrefix+amount, n); err != nil {
		b.fail(ctx, chat, err)
	}
}

// preCheckout confirms only a top-up of an offered sum by someone with an account.
func (b *bot) preCheckout(ctx context.Context, q *tgPreCheckout) {
	ok, why := b.checkOrder(ctx, q)
	if err := b.tg.answerPreCheckout(ctx, q.ID, ok, why); err != nil {
		log.Printf("answerPreCheckoutQuery: %v", err)
	}
}

func (b *bot) checkOrder(ctx context.Context, q *tgPreCheckout) (bool, string) {
	amount, ok := strings.CutPrefix(q.InvoicePayload, topUpPrefix)
	n, err := strconv.Atoi(amount)
	if !ok || err != nil || q.Currency != "XTR" || n != q.TotalAmount || !validTopUp(n) {
		return false, "Этот счёт устарел. Откройте /balance и пополните заново."
	}
	if _, err := b.api.getAccount(ctx, q.From.ID); err != nil {
		if errCode(err) == "not_found" {
			return false, "Сначала зарегистрируйте адрес: /start"
		}
		log.Printf("pre-checkout %s: %v", q.ID, err)
		return false, "Не получилось проверить заказ. Попробуйте через пару минут."
	}
	return true, ""
}

// paid puts a successful top-up on the balance. If the server refuses it,
// the Stars go back at once.
func (b *bot) paid(ctx context.Context, chat int64, from tgUser, p *tgSuccessfulPayment) {
	bal, err := b.api.topUp(ctx, from.ID, p.TotalAmount, p.TelegramPaymentChargeID)
	if err != nil {
		log.Printf("top-up %s from %d (%d stars): %v", p.TelegramPaymentChargeID, from.ID, p.TotalAmount, err)
		if code := errCode(err); code == "not_found" || code == "banned" || code == "bad_request" {
			if rerr := b.tg.refundStars(ctx, from.ID, p.TelegramPaymentChargeID); rerr != nil {
				log.Printf("refund %s: %v", p.TelegramPaymentChargeID, rerr)
				b.say(ctx, chat, "Пополнение не удалось зачислить. Мы вернём звёзды вручную — напишите /paysupport.", nil)
				return
			}
			b.say(ctx, chat, "Пополнение не удалось зачислить, звёзды возвращены.", nil)
			return
		}
		// Server unreachable: the charge is in Telegram, nothing is lost —
		// it is credited by hand from the log.
		b.say(ctx, chat, "Оплата получена, но зачисление задерживается. Мы зачислим вручную; если баланс не обновится в течение суток — /paysupport.", nil)
		return
	}
	b.say(ctx, chat, fmt.Sprintf("Баланс пополнен: %d ⭐. Подключить возможности — /balance.", bal), nil)
}

func (b *bot) showRefunds(ctx context.Context, chat int64, from tgUser) {
	list, err := b.api.refundable(ctx, from.ID)
	if b.apiTrouble(ctx, chat, from, err) {
		return
	}
	if len(list) == 0 {
		b.say(ctx, chat, "Вернуть нечего: из пополнений уже что-то потрачено. Telegram возвращает только платёж целиком, поэтому возвращаются пополнения, из которых ничего не списано.", nil)
		return
	}
	kb := &keyboard{}
	for _, t := range list {
		kb.Rows = append(kb.Rows, []button{{Text: fmt.Sprintf("Вернуть %d ⭐", t.Amount), Data: cbRefundDo + t.ChargeID}})
	}
	b.say(ctx, chat, "Эти пополнения ещё не тратились — их можно вернуть. Звёзды вернутся на ваш счёт Telegram.", kb)
}

func (b *bot) refund(ctx context.Context, chat int64, from tgUser, charge string) {
	amount, err := b.api.reserveRefund(ctx, from.ID, charge)
	if errCode(err) == "not_refundable" {
		b.say(ctx, chat, "Это пополнение уже нельзя вернуть: из него что-то потрачено или оно уже возвращено.", nil)
		return
	}
	if b.apiTrouble(ctx, chat, from, err) {
		return
	}
	if err := b.tg.refundStars(ctx, from.ID, charge); err != nil {
		log.Printf("refund %s: %v", charge, err)
		if cerr := b.api.cancelRefund(ctx, from.ID, charge); cerr != nil {
			log.Printf("cancel refund %s: %v — fix the balance by hand", charge, cerr)
		}
		b.say(ctx, chat, "Telegram не принял возврат. Баланс не изменился; напишите /paysupport.", nil)
		return
	}
	b.say(ctx, chat, fmt.Sprintf("Возвращено %d ⭐.", amount), nil)
}

func (b *bot) paySupportText() string {
	return "Вопросы по оплате и возвратам — на " + b.cfg.supportEmail +
		". Укажите ваш адрес на lunlun.email и что произошло. Неизрасходованные пополнения можно вернуть самому: /refund."
}
