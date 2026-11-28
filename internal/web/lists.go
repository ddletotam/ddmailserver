package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/signup"
	"github.com/gorilla/mux"
)

// Mailing lists (docs/lunlun-bot-plan.md, «Рассылки»): the owner makes a list
// in the cabinet and pays by its subscriber ceiling; people subscribe through
// the bot and confirm the address by a link; every letter has a one-click
// unsubscribe link (pages /l/...). Sending — internal/lists.

// ---- bot API ----

func (s *Server) handleBotList(w http.ResponseWriter, r *http.Request) {
	l, err := s.database.MailingListByCode(mux.Vars(r)["code"])
	if errors.Is(err, db.ErrListNotFound) {
		botError(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"title": l.Title, "address": l.Address(), "enabled": l.Enabled, "full": l.Active+l.Pending >= l.Tier,
	})
}

type botSubscribeRequest struct {
	TgUserID int64  `json:"tg_user_id"`
	Email    string `json:"email"`
	Own      bool   `json:"own"` // to the user's own address of the service
}

// validSubscriberEmail: a bare addr-spec with a dotted domain.
func validSubscriberEmail(e string) bool {
	if len(e) > 254 || strings.ContainsAny(e, " \t<>\"") {
		return false
	}
	a, err := mail.ParseAddress(e)
	if err != nil || !strings.EqualFold(a.Address, e) {
		return false
	}
	_, domain, _ := strings.Cut(e, "@")
	return strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}

func (s *Server) handleBotSubscribe(w http.ResponseWriter, r *http.Request) {
	var req botSubscribeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.TgUserID <= 0 {
		botError(w, http.StatusBadRequest, "bad_request")
		return
	}
	own := ""
	switch a, err := s.database.GetTelegramAccount(req.TgUserID); {
	case err == nil && a.Banned:
		botError(w, http.StatusForbidden, "banned")
		return
	case err == nil:
		own = strings.ToLower(a.Username + "@" + s.bot.domain)
	case !errors.Is(err, db.ErrTelegramNotLinked):
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if req.Own {
		if own == "" {
			botError(w, http.StatusNotFound, "no_account")
			return
		}
		email = own
	} else if !validSubscriberEmail(email) {
		botError(w, http.StatusBadRequest, "invalid_email")
		return
	}
	// The user's own address of the service is known to be theirs.
	sub, l, err := s.database.Subscribe(mux.Vars(r)["code"], req.TgUserID, email, email == own)
	switch {
	case err == nil:
		respondJSON(w, http.StatusOK, map[string]any{"status": sub.Status, "email": sub.Email, "title": l.Title, "address": l.Address()})
	case errors.Is(err, db.ErrListNotFound):
		botError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, db.ErrListOff):
		botError(w, http.StatusConflict, "off")
	case errors.Is(err, db.ErrListFull):
		botError(w, http.StatusConflict, "full")
	case errors.Is(err, db.ErrAlreadySubscribed):
		botError(w, http.StatusConflict, "already")
	case errors.Is(err, db.ErrConfirmPending):
		botError(w, http.StatusConflict, "pending")
	case errors.Is(err, db.ErrTooManyConfirms):
		botError(w, http.StatusTooManyRequests, "too_many")
	default:
		log.Printf("bot api: subscribe: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
	}
}

func (s *Server) handleBotListSubscriptions(w http.ResponseWriter, r *http.Request) {
	subs, err := s.database.ListSubscriptionsOfTg(tgUserID(r))
	if err != nil {
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"lists": subs})
}

func (s *Server) handleBotUnsubscribe(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	err := s.database.UnsubscribeTg(tgUserID(r), id)
	switch {
	case err == nil:
		respondJSON(w, http.StatusOK, map[string]bool{"unsubscribed": true})
	case errors.Is(err, db.ErrSubscriberUnknown):
		botError(w, http.StatusNotFound, "not_found")
	default:
		log.Printf("bot api: unsubscribe: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
	}
}

// ---- cabinet ----

// ListTier is a subscriber ceiling on sale.
type ListTier struct {
	Size  int
	Price int
}

// MyList is a list on the page.
type MyList struct {
	*db.MailingList
	Link        string // the bot's subscribe link
	Paid        string // "" — free (not a billed user); else the subscription state
	Until       string
	Stopped     string // autostop until
	Subscribers []db.ListSubscriber
	Issues      []db.ListIssue
}

// MyListsData is the page data.
type MyListsData struct {
	PageData
	Lists   []MyList
	Domains []*models.Domain // where a list may be made
	Tiers   []ListTier
	Billed  bool
	Balance int
	BotURL  string
	Service string // the service domain
}

func (s *Server) listsEnabled() bool { return s.bot != nil && s.bot.publicURL != "" }

// listTiers: the tiers with their prices; a billed user sees only priced ones.
func (s *Server) listTiers(billed bool) []ListTier {
	var out []ListTier
	for _, t := range db.ListTiers {
		p := s.bot.prices[db.ListTierPriceKey(t)]
		if billed && p <= 0 {
			continue
		}
		if !billed {
			p = 0
		}
		out = append(out, ListTier{Size: t, Price: p})
	}
	return out
}

func fmtDate(ms int64) string {
	return time.UnixMilli(ms).In(time.FixedZone("MSK", 3*3600)).Format("02.01.2006")
}

func fmtDateTime(ms int64) string {
	return time.UnixMilli(ms).In(time.FixedZone("MSK", 3*3600)).Format("02.01.2006 15:04")
}

// HandleMyListsPage: the user's mailing lists.
func (s *Server) HandleMyListsPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil || user.IsAdmin() || user.IsMailbox() || !s.listsEnabled() {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	data := MyListsData{PageData: PageData{Title: "Рассылки", User: user}, BotURL: s.botURL(), Service: s.bot.domain}
	data.Billed = s.billed(user.ID)
	data.Balance, _ = s.database.Balance(user.ID)
	data.Tiers = s.listTiers(data.Billed)
	if sd, err := s.database.GetDomainByName(s.bot.domain); err == nil && sd != nil {
		data.Domains = append(data.Domains, sd)
	}
	if own, err := s.database.GetDomainsByUserID(user.ID); err == nil {
		for _, d := range own {
			if d.Verified && d.Domain != s.bot.domain {
				data.Domains = append(data.Domains, d)
			}
		}
	}
	lists, err := s.database.MailingListsOf(user.ID)
	if err != nil {
		log.Printf("my lists: %v", err)
	}
	subs, _ := s.database.Subscriptions(user.ID)
	now := time.Now().UnixMilli()
	for _, l := range lists {
		ml := MyList{MailingList: l, Link: s.botURL() + "?start=l_" + l.Code}
		if l.StoppedUntil > now {
			ml.Stopped = fmtDateTime(l.StoppedUntil)
		}
		for _, sub := range subs { // newest first
			if sub.Feature == db.FeatureMailingList && sub.RefID == l.ID {
				switch {
				case sub.Working(now) && sub.Status == "active":
					ml.Paid = "active"
				case sub.Working(now):
					ml.Paid = "cancelled"
				default:
					ml.Paid = "ended"
				}
				ml.Until = fmtDate(sub.PaidUntil)
				break
			}
		}
		ml.Subscribers, _ = s.database.ListSubscribers(l.ID)
		ml.Issues, _ = s.database.ListIssues(l.ID, 5)
		data.Lists = append(data.Lists, ml)
	}
	s.renderTemplate(w, "my_lists.html", data)
}

// HandleMyAddList makes a list and, for a billed user, subscribes to it.
func (s *Server) HandleMyAddList(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil || user.IsAdmin() || user.IsMailbox() || !s.listsEnabled() {
		respondError(w, http.StatusForbidden, "forbidden")
		return
	}
	domainID, _ := strconv.ParseInt(r.FormValue("domain_id"), 10, 64)
	d, err := s.database.GetDomainByID(domainID)
	if err != nil || d == nil {
		respondError(w, http.StatusNotFound, "Домен не найден")
		return
	}
	service := d.Domain == s.bot.domain
	if !service && (d.UserID != user.ID || !d.Verified) {
		respondError(w, http.StatusConflict, "Сначала подтвердите домен")
		return
	}
	local := strings.ToLower(strings.TrimSpace(r.FormValue("local_part")))
	if service {
		local = signup.NormalizeLogin(local)
		switch signup.CheckLogin(local) {
		case "":
		case signup.ReasonReserved:
			respondError(w, http.StatusBadRequest, "Этот адрес зарезервирован: служебные имена и названия организаций не выдаются")
			return
		default:
			respondError(w, http.StatusBadRequest, "Адрес: 3–32 символа, латиница, цифры, точка, дефис, подчёркивание; начинается с буквы")
			return
		}
	} else if !validLocalPart(local) {
		respondError(w, http.StatusBadRequest, "Так адрес не записывается: латиница, цифры, точка, дефис, подчёркивание")
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" || utf8.RuneCountInString(title) > 100 || strings.ContainsAny(title, "\r\n") {
		respondError(w, http.StatusBadRequest, "Название рассылки: от 1 до 100 символов")
		return
	}
	billed := s.billed(user.ID)
	tier, _ := strconv.Atoi(r.FormValue("tier"))
	price := -1
	for _, t := range s.listTiers(billed) {
		if t.Size == tier {
			price = t.Price
		}
	}
	if price < 0 {
		respondError(w, http.StatusBadRequest, "Выберите число подписчиков")
		return
	}
	if billed {
		if b, err := s.database.Balance(user.ID); err != nil || b < price {
			respondError(w, http.StatusPaymentRequired, fmt.Sprintf("На балансе не хватает звёзд: рассылка до %d подписчиков стоит %d ⭐ в месяц. Пополните баланс в боте.", tier, price))
			return
		}
	}
	l := &db.MailingList{UserID: user.ID, DomainID: d.ID, LocalPart: local, Title: title, Tier: tier}
	if err := s.database.CreateMailingList(l); err != nil {
		if errors.Is(err, db.ErrListAddressTaken) {
			respondError(w, http.StatusConflict, "Этот адрес занят")
			return
		}
		log.Printf("my lists: create: %v", err)
		respondError(w, http.StatusInternalServerError, "Не получилось создать рассылку")
		return
	}
	if billed {
		if _, _, err := s.database.EnableFeature(user.ID, db.FeatureMailingList, l.ID, price); err != nil {
			_ = s.database.DeleteMailingList(user.ID, l.ID)
			if errors.Is(err, db.ErrInsufficientFunds) {
				respondError(w, http.StatusPaymentRequired, "На балансе не хватает звёзд")
				return
			}
			log.Printf("my lists: subscribe %s: %v", l.Address(), err)
			respondError(w, http.StatusInternalServerError, "Не получилось оплатить рассылку")
			return
		}
	}
	log.Printf("my lists: %s made list %s (tier %d, price %d)", user.Username, l.Address(), tier, price)
	respondJSON(w, http.StatusCreated, map[string]string{"address": l.Address()})
}

func (s *Server) myList(w http.ResponseWriter, r *http.Request) (*models.User, *db.MailingList, bool) {
	user := s.GetUserFromContext(r.Context())
	id, _ := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	l, err := s.database.MailingListByID(id)
	if user == nil || err != nil || l.UserID != user.ID {
		respondError(w, http.StatusNotFound, "Рассылка не найдена")
		return nil, nil, false
	}
	return user, l, true
}

// HandleMyDeleteList stops the list's subscription and deletes it with its
// subscribers.
func (s *Server) HandleMyDeleteList(w http.ResponseWriter, r *http.Request) {
	user, l, ok := s.myList(w, r)
	if !ok {
		return
	}
	subs, _ := s.database.Subscriptions(user.ID)
	for _, sub := range subs {
		if sub.Feature == db.FeatureMailingList && sub.RefID == l.ID && sub.Status == "active" {
			_ = s.database.CancelSubscription(user.ID, sub.ID)
		}
	}
	if err := s.database.DeleteMailingList(user.ID, l.ID); err != nil {
		log.Printf("my lists: delete %d: %v", l.ID, err)
		respondError(w, http.StatusInternalServerError, "Не получилось удалить рассылку")
		return
	}
	respondJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// HandleMyRemoveSubscriber: the owner takes an address off the list.
func (s *Server) HandleMyRemoveSubscriber(w http.ResponseWriter, r *http.Request) {
	user, l, ok := s.myList(w, r)
	if !ok {
		return
	}
	sid, _ := strconv.ParseInt(mux.Vars(r)["sid"], 10, 64)
	if err := s.database.RemoveSubscriber(user.ID, l.ID, sid); err != nil {
		respondError(w, http.StatusNotFound, "Подписчик не найден")
		return
	}
	respondJSON(w, http.StatusOK, map[string]bool{"removed": true})
}

// ---- public confirm / unsubscribe pages ----

func (s *Server) registerListRoutes() {
	s.router.HandleFunc("/l/c/{token}", s.handleListConfirm).Methods("GET", "POST")
	s.router.HandleFunc("/l/u/{token}", s.handleListUnsubscribe).Methods("GET", "POST")
}

var listPage = template.Must(template.New("l").Parse(`<!doctype html>
<html lang="ru"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;background:#f5f7fb;color:#1d273b;margin:0;padding:48px 16px}
main{max-width:480px;margin:0 auto;background:#fff;border-radius:8px;padding:28px;box-shadow:0 1px 3px rgba(0,0,0,.08)}
h1{font-size:20px;margin:0 0 12px}p{line-height:1.5}
button{font:inherit;background:#206bc4;color:#fff;border:0;border-radius:6px;padding:10px 18px;cursor:pointer}
</style></head><body><main>
<h1>{{.Title}}</h1><p>{{.Text}}</p>
{{if .Button}}<form method="post"><button type="submit">{{.Button}}</button></form>{{end}}
</main></body></html>`))

type listPageData struct{ Title, Text, Button string }

func renderListPage(w http.ResponseWriter, status int, d listPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.WriteHeader(status)
	if err := listPage.Execute(w, d); err != nil {
		log.Printf("list page: %v", err)
	}
}

var badListLink = listPageData{Title: "Ссылка недействительна", Text: "Возможно, рассылку удалили или ссылку скопировали не целиком."}

// handleListConfirm: GET shows a button, POST confirms — link scanners of
// mail services open links, and must not confirm a subscription by that.
func (s *Server) handleListConfirm(w http.ResponseWriter, r *http.Request) {
	token := mux.Vars(r)["token"]
	sub, l, err := s.database.SubscriberByToken(token)
	if err != nil || l == nil {
		renderListPage(w, http.StatusNotFound, badListLink)
		return
	}
	name := "«" + l.Title + "» (" + l.Address() + ")"
	switch {
	case sub.Status == "active":
		renderListPage(w, http.StatusOK, listPageData{Title: "Подписка уже подтверждена", Text: "Адрес " + sub.Email + " получает рассылку " + name + "."})
	case sub.Status != "pending":
		renderListPage(w, http.StatusOK, listPageData{Title: "Подписки нет", Text: "Адрес " + sub.Email + " отписан от рассылки " + name + ". Подписаться снова можно через Telegram-бота."})
	case r.Method == http.MethodGet:
		renderListPage(w, http.StatusOK, listPageData{Title: "Подтвердите подписку", Text: "Подписать адрес " + sub.Email + " на рассылку " + name + "?", Button: "Подтвердить подписку"})
	default:
		if err := s.database.ConfirmSubscriber(token); err != nil {
			renderListPage(w, http.StatusNotFound, badListLink)
			return
		}
		renderListPage(w, http.StatusOK, listPageData{Title: "Подписка подтверждена", Text: "Письма рассылки " + name + " будут приходить на " + sub.Email + ". Отписаться можно по ссылке в любом письме или в Telegram-боте."})
	}
}

// handleListUnsubscribe: GET shows a button, POST unsubscribes; a POST with
// List-Unsubscribe=One-Click is the mail service's own button (RFC 8058).
func (s *Server) handleListUnsubscribe(w http.ResponseWriter, r *http.Request) {
	token := mux.Vars(r)["token"]
	sub, l, err := s.database.SubscriberByToken(token)
	if err != nil || l == nil {
		renderListPage(w, http.StatusNotFound, badListLink)
		return
	}
	name := "«" + l.Title + "» (" + l.Address() + ")"
	if r.Method == http.MethodGet {
		if sub.Status != "active" && sub.Status != "pending" {
			renderListPage(w, http.StatusOK, listPageData{Title: "Вы отписаны", Text: "Адрес " + sub.Email + " больше не получает рассылку " + name + "."})
			return
		}
		renderListPage(w, http.StatusOK, listPageData{Title: "Отписаться от рассылки", Text: "Больше не присылать рассылку " + name + " на " + sub.Email + "?", Button: "Отписаться"})
		return
	}
	if err := s.database.UnsubscribeToken(token); err != nil && !errors.Is(err, db.ErrSubscriberUnknown) {
		log.Printf("list unsubscribe: %v", err)
		renderListPage(w, http.StatusInternalServerError, listPageData{Title: "Не получилось", Text: "Попробуйте ещё раз через минуту."})
		return
	}
	renderListPage(w, http.StatusOK, listPageData{Title: "Вы отписаны", Text: "Адрес " + sub.Email + " больше не получает рассылку " + name + "."})
}
