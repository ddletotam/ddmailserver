package web

import (
	"log"
	"net/http"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
)

// The balance page of the cabinet: what is on the balance, what is paid for
// and every top-up and charge (docs/lunlun-bot-plan.md). Paying is in the bot:
// Telegram Stars exist only there.

// BillingLine is a row of the history.
type BillingLine struct {
	Date   string
	What   string
	Amount int
}

// BillingSub is a paid feature on the page.
type BillingSub struct {
	Title string
	State string
}

// MyBillingData is the page data.
type MyBillingData struct {
	PageData
	Balance int
	Subs    []BillingSub
	History []BillingLine
	BotURL  string
	// Space: one for mail and files (lunlun.io); QuotaMB 0 — no quota.
	MailMB, FilesMB, QuotaMB int64
}

var featureTitles = map[string]string{
	db.FeatureExternalAccounts: "Другие ящики",
	db.FeatureExtraStorage:     "+10 ГБ",
	db.FeatureSendLimits:       "Больше отправки",
	db.FeatureMailbox:          "Ящик",
}

func featureTitle(code string) string {
	if t, ok := featureTitles[code]; ok {
		return t
	}
	return code
}

// HandleMyBillingPage shows the balance, subscriptions and history.
func (s *Server) HandleMyBillingPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil || user.IsAdmin() || user.IsMailbox() {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	data := MyBillingData{PageData: PageData{Title: "Баланс", User: user}, BotURL: "https://t.me/robotohobot"}
	var err error
	if data.Balance, err = s.database.Balance(user.ID); err != nil {
		log.Printf("billing page: %v", err)
	}
	if sp, err := s.database.SpaceOf(user.ID); err == nil {
		data.MailMB, data.QuotaMB = sp.MailBytes>>20, sp.QuotaBytes>>20
		data.FilesMB = s.database.FilesBytes(user.ID) >> 20
	}
	subs, _ := s.database.Subscriptions(user.ID)
	now := time.Now().UnixMilli()
	for _, sub := range subs {
		title := featureTitle(sub.Feature)
		if sub.Feature == db.FeatureMailbox {
			if mb, err := s.database.GetMailboxByID(sub.RefID); err == nil {
				if d, err := s.database.GetDomainByID(mb.DomainID); err == nil {
					title += " " + mb.LocalPart + "@" + d.Domain
				}
			}
		}
		until := time.UnixMilli(sub.PaidUntil).Format("02.01.2006")
		state := ""
		switch {
		case sub.Status == "active" && sub.PaidUntil > now:
			state = "продление " + until
		case sub.Status == "active" && sub.Working(now):
			state = "не хватает звёзд — отключится через несколько дней"
		case sub.Status == "cancelled" && sub.Working(now):
			state = "отключено, работает до " + until
		default:
			continue // ended or suspended long ago — history shows them
		}
		data.Subs = append(data.Subs, BillingSub{Title: title, State: state})
	}
	hist, _ := s.database.BalanceHistory(user.ID, 100)
	for _, h := range hist {
		what := map[string]string{"topup": "Пополнение", "refund": "Возврат"}[h.Kind]
		if h.Kind == "charge" {
			what = "Списание: " + featureTitle(trimRenewal(h.Note))
			if trimRenewal(h.Note) != h.Note {
				what += " (продление)"
			}
		}
		data.History = append(data.History, BillingLine{
			Date: time.UnixMilli(h.CreatedAt).Format("02.01.2006 15:04"), What: what, Amount: h.Amount,
		})
	}
	s.renderTemplate(w, "my_billing.html", data)
}

func trimRenewal(note string) string {
	const suffix = " renewal"
	if len(note) > len(suffix) && note[len(note)-len(suffix):] == suffix {
		return note[:len(note)-len(suffix)]
	}
	return note
}
