package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/signup"
	"github.com/gorilla/mux"
)

// The user's own domains and mailboxes in the cabinet (docs/lunlun-bot-plan.md,
// решения 2026-11-28): a domain is free — ownership proven by a TXT record;
// every mailbox beyond the user's own address is paid, one subscription each,
// on the user's domains or on the service domain. A mailbox is either the
// user's own (mail comes to the account) or has a separate sign-in.

const domainVerifyPrefix = "ddmail-verify="

// DNSRecord is a record the domain owner publishes.
type DNSRecord struct {
	Type  string
	Name  string
	Value string
	Why   string
}

// MyDomain is a domain on the page.
type MyDomain struct {
	*models.Domain
	Records   []DNSRecord
	Mailboxes []MyMailbox
}

// MyMailbox is a mailbox on the page.
type MyMailbox struct {
	ID       int64
	Address  string
	Separate bool // has its own sign-in
	Enabled  bool
	Paid     string // "" — free (the user's own address); else the state of its subscription
	Until    string
}

// MyDomainsData is the page data.
type MyDomainsData struct {
	PageData
	Domains      []MyDomain
	Service      *MyDomain // the service domain: the user's extra addresses on it
	Balance      int
	MailboxPrice int
	Billed       bool // the user pays for mailboxes (signed up through the bot)
	MailHost     string
	BotURL       string
}

func (s *Server) mailHost() string {
	if h := s.publicEndpoints.Hostname; h != "" {
		return h
	}
	return "mail.lunlun.email"
}

// domainRecords are the DNS records a user's domain needs.
func (s *Server) domainRecords(d *models.Domain) []DNSRecord {
	host := s.mailHost()
	recs := []DNSRecord{
		{"TXT", d.Domain, domainVerifyPrefix + d.VerifyToken, "подтверждение владения"},
		{"MX", d.Domain, "10 " + host + ".", "куда доставлять почту"},
		{"TXT", d.Domain, "v=spf1 mx a:" + host + " ~all", "SPF: кто может отправлять от имени домена"},
	}
	if pub, err := s.database.EnsureDKIMKey(d.Domain); err == nil {
		recs = append(recs, DNSRecord{"TXT", "mf._domainkey." + d.Domain, "v=DKIM1; k=rsa; p=" + pub, "DKIM: подпись писем"})
	} else {
		log.Printf("my domains: dkim key of %s: %v", d.Domain, err)
	}
	recs = append(recs, DNSRecord{"TXT", "_dmarc." + d.Domain, "v=DMARC1; p=quarantine", "DMARC: что делать с подделками"})
	return recs
}

func (s *Server) myMailboxes(user *models.User, domainID int64, subs []db.Subscription) []MyMailbox {
	mbs, err := s.database.GetMailboxesByDomainID(domainID)
	if err != nil {
		log.Printf("my domains: %v", err)
		return nil
	}
	d, _ := s.database.GetDomainByID(domainID)
	now := time.Now().UnixMilli()
	var out []MyMailbox
	for _, m := range mbs {
		if m.UserID != user.ID {
			continue
		}
		mm := MyMailbox{ID: m.ID, Address: m.LocalPart + "@" + d.Domain, Separate: m.PrincipalID != 0, Enabled: m.Enabled}
		for _, sub := range subs {
			if sub.Feature == db.FeatureMailbox && sub.RefID == m.ID {
				switch {
				case sub.Working(now) && sub.Status == "active":
					mm.Paid = "active"
				case sub.Working(now):
					mm.Paid = "cancelled"
				default:
					mm.Paid = "ended"
				}
				mm.Until = time.UnixMilli(sub.PaidUntil).Format("02.01.2006")
				break
			}
		}
		out = append(out, mm)
	}
	return out
}

// HandleMyDomainsPage: the user's domains and mailboxes.
func (s *Server) HandleMyDomainsPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil || user.IsAdmin() || user.IsMailbox() {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	data := MyDomainsData{PageData: PageData{Title: "Домены и ящики", User: user}, MailHost: s.mailHost(), BotURL: s.botURL()}
	subs, _ := s.database.Subscriptions(user.ID)
	data.Balance, _ = s.database.Balance(user.ID)
	data.Billed = s.billed(user.ID)
	if s.bot != nil {
		data.MailboxPrice = s.bot.prices[db.FeatureMailbox]
		if sd, err := s.database.GetDomainByName(s.bot.domain); err == nil && sd != nil {
			data.Service = &MyDomain{Domain: sd, Mailboxes: s.myMailboxes(user, sd.ID, subs)}
		}
	}
	domains, err := s.database.GetDomainsByUserID(user.ID)
	if err != nil {
		log.Printf("my domains: %v", err)
	}
	for _, d := range domains {
		data.Domains = append(data.Domains, MyDomain{Domain: d, Records: s.domainRecords(d), Mailboxes: s.myMailboxes(user, d.ID, subs)})
	}
	s.renderTemplate(w, "my_domains.html", data)
}

// billed: the user pays for mailboxes — a service user (signed up through the bot).
func (s *Server) billed(userID int64) bool {
	_, err := s.database.GetTelegramAccountByUser(userID)
	return err == nil
}

// HandleMyAddDomain adds a domain, unverified until its TXT record is seen.
func (s *Server) HandleMyAddDomain(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil || user.IsAdmin() || user.IsMailbox() {
		respondError(w, http.StatusForbidden, "forbidden")
		return
	}
	name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(r.FormValue("domain"))), ".")
	if !validDomainName(name) {
		respondError(w, http.StatusBadRequest, "Так домен не записывается — например, example.com")
		return
	}
	if s.bot != nil && (name == s.bot.domain || strings.HasSuffix(name, "."+s.bot.domain)) {
		respondError(w, http.StatusBadRequest, "Этот домен принадлежит сервису")
		return
	}
	d := &models.Domain{Domain: name, UserID: user.ID, Enabled: true}
	if err := s.database.CreateDomain(d); err != nil {
		if errors.Is(err, db.ErrDomainTaken) {
			respondError(w, http.StatusConflict, "Этот домен уже подключён")
			return
		}
		log.Printf("my domains: add %s: %v", name, err)
		respondError(w, http.StatusInternalServerError, "Не получилось добавить домен")
		return
	}
	if _, err := s.database.EnsureDKIMKey(name); err != nil {
		log.Printf("my domains: dkim %s: %v", name, err)
	}
	log.Printf("my domains: %s added %s", user.Username, name)
	respondJSON(w, http.StatusCreated, map[string]int64{"id": d.ID})
}

func (s *Server) lookupDomainTXT(ctx context.Context, name string) ([]string, error) {
	if s.lookupTXT != nil {
		return s.lookupTXT(ctx, name)
	}
	return net.DefaultResolver.LookupTXT(ctx, name)
}

func hasDomainVerifyRecord(txts []string, token string) bool {
	if token == "" {
		return false
	}
	for _, t := range txts {
		t = strings.Trim(strings.TrimSpace(t), `"`)
		if len(t) >= len(domainVerifyPrefix) && strings.EqualFold(t[:len(domainVerifyPrefix)], domainVerifyPrefix) &&
			strings.TrimSpace(t[len(domainVerifyPrefix):]) == token {
			return true
		}
	}
	return false
}

func (s *Server) myDomain(w http.ResponseWriter, r *http.Request, user *models.User) (*models.Domain, bool) {
	id, _ := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	d, err := s.database.GetDomainByID(id)
	if err != nil || d == nil || d.UserID != user.ID {
		respondError(w, http.StatusNotFound, "Домен не найден")
		return nil, false
	}
	return d, true
}

// HandleMyVerifyDomain checks the TXT record and marks the domain verified.
func (s *Server) HandleMyVerifyDomain(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	d, ok := s.myDomain(w, r, user)
	if !ok {
		return
	}
	if d.Verified {
		respondJSON(w, http.StatusOK, map[string]bool{"verified": true})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	txts, err := s.lookupDomainTXT(ctx, d.Domain)
	if err != nil || !hasDomainVerifyRecord(txts, d.VerifyToken) {
		respondError(w, http.StatusConflict, "Запись подтверждения не найдена. Если только что добавили её — подождите несколько минут, пока DNS обновится.")
		return
	}
	if err := s.database.VerifyDomain(d.ID, user.ID, models.DomainVerifiedDNSTXT); err != nil {
		if errors.Is(err, db.ErrDomainTaken) {
			respondError(w, http.StatusConflict, "Этот домен уже подтвердил другой пользователь")
			return
		}
		log.Printf("my domains: verify %s: %v", d.Domain, err)
		respondError(w, http.StatusInternalServerError, "Не получилось подтвердить домен")
		return
	}
	log.Printf("my domains: %s verified %s", user.Username, d.Domain)
	respondJSON(w, http.StatusOK, map[string]bool{"verified": true})
}

// HandleMyDeleteDomain removes a domain that has no mailboxes left.
func (s *Server) HandleMyDeleteDomain(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	d, ok := s.myDomain(w, r, user)
	if !ok {
		return
	}
	if mbs, _ := s.database.GetMailboxesByDomainID(d.ID); len(mbs) > 0 {
		respondError(w, http.StatusConflict, "Сначала удалите ящики на этом домене")
		return
	}
	if err := s.database.DeleteDomain(d.ID, user.ID); err != nil {
		log.Printf("my domains: delete %s: %v", d.Domain, err)
		respondError(w, http.StatusInternalServerError, "Не получилось удалить домен")
		return
	}
	respondJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func validLocalPart(l string) bool {
	if l == "" || len(l) > 64 || l[0] == '.' || l[len(l)-1] == '.' || strings.Contains(l, "..") {
		return false
	}
	for _, c := range l {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_' || c == '+') {
			return false
		}
	}
	return true
}

// HandleMyAddMailbox makes a mailbox on the user's verified domain or on the
// service domain and, for a service user, subscribes to it from the balance.
func (s *Server) HandleMyAddMailbox(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil || user.IsAdmin() || user.IsMailbox() {
		respondError(w, http.StatusForbidden, "forbidden")
		return
	}
	domainID, _ := strconv.ParseInt(r.FormValue("domain_id"), 10, 64)
	d, err := s.database.GetDomainByID(domainID)
	if err != nil || d == nil {
		respondError(w, http.StatusNotFound, "Домен не найден")
		return
	}
	service := s.bot != nil && d.Domain == s.bot.domain
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
		if taken, err := s.database.LoginTaken(local, d.Domain, d.ID); err != nil || taken {
			respondError(w, http.StatusConflict, "Этот адрес занят")
			return
		}
	} else if !validLocalPart(local) {
		respondError(w, http.StatusBadRequest, "Так адрес не записывается: латиница, цифры, точка, дефис, подчёркивание")
		return
	} else if _, err := s.database.MailingListByAddress(local + "@" + d.Domain); err == nil {
		respondError(w, http.StatusConflict, "Этот адрес занят рассылкой")
		return
	}
	separate := r.FormValue("kind") == "separate"
	price := 0
	billed := s.billed(user.ID)
	if billed {
		if s.bot == nil || s.bot.prices[db.FeatureMailbox] <= 0 {
			respondError(w, http.StatusServiceUnavailable, "Ящики сейчас не продаются")
			return
		}
		price = s.bot.prices[db.FeatureMailbox]
		if b, err := s.database.Balance(user.ID); err != nil || b < price {
			respondError(w, http.StatusPaymentRequired, fmt.Sprintf("На балансе не хватает звёзд: ящик стоит %d ⭐ в месяц. Пополните баланс в боте.", price))
			return
		}
	}
	mb := &models.Mailbox{UserID: user.ID, DomainID: d.ID, LocalPart: local, Enabled: true}
	if err := s.database.CreateMailbox(mb); err != nil {
		respondError(w, http.StatusConflict, "Этот адрес занят")
		return
	}
	address := local + "@" + d.Domain
	password := ""
	if separate {
		var hash string
		password, hash, err = newBotPassword()
		if err == nil {
			_, err = s.database.CreateMailboxPrincipal(mb.ID, address, hash)
		}
		if err != nil {
			_, _ = s.database.DeleteMailboxAndAccount(mb.ID)
			log.Printf("my domains: separate sign-in for %s: %v", address, err)
			respondError(w, http.StatusInternalServerError, "Не получилось создать ящик")
			return
		}
	}
	if billed {
		if _, _, err := s.database.EnableFeature(user.ID, db.FeatureMailbox, mb.ID, price); err != nil {
			_, _ = s.database.DeleteMailboxAndAccount(mb.ID)
			if errors.Is(err, db.ErrInsufficientFunds) {
				respondError(w, http.StatusPaymentRequired, "На балансе не хватает звёзд")
				return
			}
			log.Printf("my domains: subscribe %s: %v", address, err)
			respondError(w, http.StatusInternalServerError, "Не получилось оплатить ящик")
			return
		}
	}
	log.Printf("my domains: %s made %s (separate=%v, price %d)", user.Username, address, separate, price)
	respondJSON(w, http.StatusCreated, map[string]string{"address": address, "password": password})
}

func (s *Server) myMailbox(w http.ResponseWriter, r *http.Request) (*models.User, *models.Mailbox, bool) {
	user := s.GetUserFromContext(r.Context())
	id, _ := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	mb, err := s.database.GetMailboxByID(id)
	if user == nil || err != nil || mb == nil || mb.UserID != user.ID {
		respondError(w, http.StatusNotFound, "Ящик не найден")
		return nil, nil, false
	}
	return user, mb, true
}

// HandleMyMailboxPassword gives a separate-sign-in mailbox a new password.
func (s *Server) HandleMyMailboxPassword(w http.ResponseWriter, r *http.Request) {
	_, mb, ok := s.myMailbox(w, r)
	if !ok {
		return
	}
	if mb.PrincipalID == 0 {
		respondError(w, http.StatusBadRequest, "У этого ящика нет отдельного входа — он открывается вашим логином")
		return
	}
	password, hash, err := newBotPassword()
	if err == nil {
		err = s.database.UpdatePassword(mb.PrincipalID, hash)
	}
	if err != nil {
		log.Printf("my domains: password of mailbox %d: %v", mb.ID, err)
		respondError(w, http.StatusInternalServerError, "Не получилось сменить пароль")
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"password": password})
}

// HandleMyDeleteMailbox stops the mailbox's subscription and deletes it with
// its mail.
func (s *Server) HandleMyDeleteMailbox(w http.ResponseWriter, r *http.Request) {
	user, mb, ok := s.myMailbox(w, r)
	if !ok {
		return
	}
	if mb.LocalPart == user.Username && s.bot != nil {
		if d, _ := s.database.GetDomainByID(mb.DomainID); d != nil && d.Domain == s.bot.domain {
			respondError(w, http.StatusBadRequest, "Основной адрес удалить нельзя")
			return
		}
	}
	subs, _ := s.database.Subscriptions(user.ID)
	for _, sub := range subs {
		if sub.Feature == db.FeatureMailbox && sub.RefID == mb.ID && sub.Status == "active" {
			_ = s.database.CancelSubscription(user.ID, sub.ID)
		}
	}
	if _, err := s.database.DeleteMailboxAndAccount(mb.ID); err != nil {
		log.Printf("my domains: delete mailbox %d: %v", mb.ID, err)
		respondError(w, http.StatusInternalServerError, "Не получилось удалить ящик")
		return
	}
	respondJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
