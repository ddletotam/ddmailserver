package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/signup"
	"github.com/gorilla/mux"
)

// Service API of the Telegram sign-up bot (docs/lunlun-bot-plan.md). The bot
// runs outside Russia, where api.telegram.org is reachable, and calls in here
// over HTTPS with a shared secret; nothing on this side talks to Telegram.
//
//	GET  /api/bot/v1/accounts/{tg}           → 200 {login,address,created_at} | 404 not_found | 403 banned
//	GET  /api/bot/v1/logins/{login}          → 200 {login,address,available,reason}
//	POST /api/bot/v1/accounts                {tg_user_id,login,language}
//	                                         → 201 {login,address,password} | 400 invalid|reserved
//	                                           | 409 taken|registered | 403 banned | 429 busy
//	POST /api/bot/v1/accounts/{tg}/password  → 200 {login,address,password}
//	GET    /api/bot/v1/accounts/{tg}/billing           → 200 {balance,features:[{code,title,description,price,status,paid_until}]}
//	POST   /api/bot/v1/accounts/{tg}/features/{code}   → 200 {balance,status,paid_until} | 402 insufficient {balance,price}
//	DELETE /api/bot/v1/accounts/{tg}/features/{code}   → 200 {status,paid_until}  (stops renewal)
//	POST   /api/bot/v1/topups {tg_user_id,amount,charge_id} → 200 {balance}  (the same charge twice — once)
//	GET    /api/bot/v1/accounts/{tg}/refunds           → 200 {topups:[{amount,charge_id}]}
//	POST   /api/bot/v1/accounts/{tg}/refunds/{charge}  → 200 {amount}  (taken off the balance; bot refunds in Telegram)
//	DELETE /api/bot/v1/accounts/{tg}/refunds/{charge}  → 204           (Telegram refused: put it back)
//	GET  /api/bot/v1/notices                 → 200 {notices:[{id,tg_user_id,kind,text}]}
//	POST /api/bot/v1/notices/{id}/sent       → 204
//
// Errors are {"error": "<code>"}; the bot turns the code into words.

const botPasswordLen = 16

// botAPI is the configuration of the bot API; nil — switched off.
type botAPI struct {
	secret string
	domain string
	nets   []*net.IPNet
	prices map[string]int
	// botName (without @) and publicURL (the cabinet) — for subscribe,
	// confirm and unsubscribe links of mailing lists.
	botName   string
	publicURL string
}

// SetBotLinks sets the bot's name and the cabinet's public URL.
func (s *Server) SetBotLinks(botName, publicURL string) {
	if s.bot != nil {
		s.bot.botName, s.bot.publicURL = botName, publicURL
	}
}

func (s *Server) botURL() string {
	if s.bot != nil && s.bot.botName != "" {
		return "https://t.me/" + s.bot.botName
	}
	return "https://t.me/robotohobot"
}

// botFeature is a paid feature as the bot sells it.
type botFeature struct {
	Code        string `json:"code"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Price       int    `json:"price"`
	Status      string `json:"status,omitempty"`     // active | cancelled | suspended; "" — never had it
	PaidUntil   int64  `json:"paid_until,omitempty"` // ms
	Working     bool   `json:"working"`
}

// botFeatureCatalog: order and wording of the paid features. No brands of
// mail services anywhere — only IMAP/SMTP terms (owner's rule, 2026-11-27).
var botFeatureCatalog = []botFeature{
	{Code: db.FeatureExternalAccounts, Title: "Другие ящики",
		Description: "Сервер забирает почту из других ящиков по IMAP и отправляет через их SMTP — почта всех адресов видна на всех устройствах."},
	{Code: db.FeatureExtraStorage, Title: "+10 ГБ",
		Description: "Ящик 11 ГБ вместо 1 ГБ."},
	{Code: db.FeatureMailbox, Title: "Ещё ящик",
		Description: "Дополнительный ящик на lunlun.email или на своём домене — для себя или с отдельным входом. Создаётся в кабинете, оплачивается за каждый."},
	{Code: db.FeatureMailingList, Title: "Рассылка",
		Description: "Свой список рассылки: пишете письмо на адрес рассылки — оно уходит всем подписчикам. Подписываются люди сами, через этого бота. Создаётся в кабинете, цена — по числу подписчиков."},
	{Code: db.FeatureSendLimits, Title: "Больше отправки",
		Description: "До 300 писем и 100 разных получателей в сутки вместо 50 и 20."},
}

// SetBotPrices sets the monthly price in Stars of each paid feature; a
// feature without a price is not sold.
func (s *Server) SetBotPrices(prices map[string]int) {
	if s.bot != nil {
		s.bot.prices = prices
	}
}

// SetBotAPI switches the bot API on. An empty secret or domain leaves it off.
func (s *Server) SetBotAPI(secret, domain string, networks []string) error {
	secret, domain = strings.TrimSpace(secret), strings.ToLower(strings.TrimSpace(domain))
	if secret == "" || domain == "" {
		s.bot = nil
		return nil
	}
	if len(secret) < 32 {
		s.bot = nil
		return errors.New("bot_api: secret is shorter than 32 characters, API stays off")
	}
	nets, err := parseNetworks(networks)
	s.bot = &botAPI{secret: secret, domain: domain, nets: nets}
	if err != nil {
		return fmt.Errorf("bot_api: %w", err)
	}
	return nil
}

func (s *Server) registerBotRoutes() {
	r := s.router.PathPrefix("/api/bot/v1").Subrouter()
	r.Use(s.botAuthMiddleware)
	r.HandleFunc("/accounts", s.handleBotCreateAccount).Methods("POST")
	r.HandleFunc("/accounts/{tg:[0-9]+}", s.handleBotGetAccount).Methods("GET")
	r.HandleFunc("/accounts/{tg:[0-9]+}/password", s.handleBotResetPassword).Methods("POST")
	r.HandleFunc("/logins/{login}", s.handleBotCheckLogin).Methods("GET")
	r.HandleFunc("/accounts/{tg:[0-9]+}/billing", s.handleBotBilling).Methods("GET")
	r.HandleFunc("/accounts/{tg:[0-9]+}/features/{code}", s.handleBotEnableFeature).Methods("POST")
	r.HandleFunc("/accounts/{tg:[0-9]+}/features/{code}", s.handleBotCancelFeature).Methods("DELETE")
	r.HandleFunc("/topups", s.handleBotTopUp).Methods("POST")
	r.HandleFunc("/accounts/{tg:[0-9]+}/refunds", s.handleBotRefundable).Methods("GET")
	r.HandleFunc("/accounts/{tg:[0-9]+}/refunds/{charge}", s.handleBotReserveRefund).Methods("POST")
	r.HandleFunc("/accounts/{tg:[0-9]+}/refunds/{charge}", s.handleBotCancelRefund).Methods("DELETE")
	r.HandleFunc("/lists/{code}", s.handleBotList).Methods("GET")
	r.HandleFunc("/lists/{code}/subscribers", s.handleBotSubscribe).Methods("POST")
	r.HandleFunc("/accounts/{tg:[0-9]+}/lists", s.handleBotListSubscriptions).Methods("GET")
	r.HandleFunc("/accounts/{tg:[0-9]+}/lists/{id:[0-9]+}", s.handleBotUnsubscribe).Methods("DELETE")
	r.HandleFunc("/notices", s.handleBotNotices).Methods("GET")
	r.HandleFunc("/notices/{id:[0-9]+}/sent", s.handleBotNoticeSent).Methods("POST")
}

// handleBotNotices: messages waiting for the bot to deliver.
func (s *Server) handleBotNotices(w http.ResponseWriter, r *http.Request) {
	notices, err := s.database.PendingBotNotices(50)
	if err != nil {
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"notices": notices})
}

func (s *Server) handleBotNoticeSent(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err := s.database.MarkBotNoticeSent(id); err != nil {
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// botAuthMiddleware: API off → 404 (as if it did not exist); wrong network
// or secret → 401 without saying which.
func (s *Server) botAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := s.bot
		if b == nil {
			http.NotFound(w, r)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(b.secret)) != 1 || !s.botNetworkAllowed(b, r) {
			respondJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) botNetworkAllowed(b *botAPI, r *http.Request) bool {
	if len(b.nets) == 0 {
		return true
	}
	ip := net.ParseIP(s.clientIP.FromRequest(r))
	if ip == nil {
		return false
	}
	for _, n := range b.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func botError(w http.ResponseWriter, status int, code string) {
	respondJSON(w, status, map[string]string{"error": code})
}

func tgUserID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(mux.Vars(r)["tg"], 10, 64)
	return id
}

// botDomainID resolves the sign-up domain; a domain missing from the admin
// pages is a configuration error, answered 503.
func (s *Server) botDomainID(w http.ResponseWriter, b *botAPI) (int64, bool) {
	d, err := s.database.GetDomainByName(b.domain)
	if err != nil || d == nil || !d.Enabled {
		log.Printf("bot api: sign-up domain %q is not usable: %v", b.domain, err)
		botError(w, http.StatusServiceUnavailable, "domain_unavailable")
		return 0, false
	}
	return d.ID, true
}

func (s *Server) handleBotGetAccount(w http.ResponseWriter, r *http.Request) {
	b := s.bot
	a, err := s.database.GetTelegramAccount(tgUserID(r))
	switch {
	case errors.Is(err, db.ErrTelegramNotLinked):
		botError(w, http.StatusNotFound, "not_found")
		return
	case err != nil:
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	case a.Banned:
		botError(w, http.StatusForbidden, "banned")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"login": a.Username, "address": a.Username + "@" + b.domain, "created_at": a.CreatedAt,
	})
}

func (s *Server) handleBotCheckLogin(w http.ResponseWriter, r *http.Request) {
	b := s.bot
	login := signup.NormalizeLogin(mux.Vars(r)["login"])
	resp := map[string]any{"login": login, "address": login + "@" + b.domain, "available": false}
	if reason := signup.CheckLogin(login); reason != "" {
		resp["reason"] = reason
		respondJSON(w, http.StatusOK, resp)
		return
	}
	domainID, ok := s.botDomainID(w, b)
	if !ok {
		return
	}
	taken, err := s.database.LoginTaken(login, b.domain, domainID)
	if err != nil {
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	if taken {
		resp["reason"] = signup.ReasonTaken
	} else {
		resp["available"] = true
	}
	respondJSON(w, http.StatusOK, resp)
}

type botCreateRequest struct {
	TgUserID int64  `json:"tg_user_id"`
	Login    string `json:"login"`
	Language string `json:"language"`
}

func (s *Server) handleBotCreateAccount(w http.ResponseWriter, r *http.Request) {
	b := s.bot
	var req botCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.TgUserID <= 0 {
		botError(w, http.StatusBadRequest, "bad_request")
		return
	}
	login := signup.NormalizeLogin(req.Login)
	if reason := signup.CheckLogin(login); reason != "" {
		botError(w, http.StatusBadRequest, reason)
		return
	}
	domainID, ok := s.botDomainID(w, b)
	if !ok {
		return
	}
	password, hash, err := newBotPassword()
	if err != nil {
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	lang := "ru"
	if strings.HasPrefix(strings.ToLower(req.Language), "en") {
		lang = "en"
	}
	u, err := s.database.CreateTelegramAccount(req.TgUserID, login, b.domain, domainID, hash, lang)
	switch {
	case errors.Is(err, db.ErrTelegramBanned):
		botError(w, http.StatusForbidden, "banned")
		return
	case errors.Is(err, db.ErrTelegramRegistered):
		botError(w, http.StatusConflict, "registered")
		return
	case errors.Is(err, db.ErrLoginTaken):
		botError(w, http.StatusConflict, signup.ReasonTaken)
		return
	case errors.Is(err, db.ErrSignupBusy):
		log.Printf("bot api: sign-up ceiling of %d per hour reached", db.SignupsPerHour)
		botError(w, http.StatusTooManyRequests, "busy")
		return
	case err != nil:
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	log.Printf("bot api: signed up %s@%s (user %d)", login, b.domain, u.ID)
	// The lunlun.io account with the same password (internal/maillink).
	lctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	s.ProvisionLunlun(lctx, u.ID, login+"@"+b.domain, password)
	cancel()
	respondJSON(w, http.StatusCreated, map[string]string{
		"login": login, "address": login + "@" + b.domain, "password": password,
	})
}

func (s *Server) handleBotResetPassword(w http.ResponseWriter, r *http.Request) {
	b := s.bot
	a, err := s.database.GetTelegramAccount(tgUserID(r))
	switch {
	case errors.Is(err, db.ErrTelegramNotLinked):
		botError(w, http.StatusNotFound, "not_found")
		return
	case err != nil:
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	case a.Banned:
		botError(w, http.StatusForbidden, "banned")
		return
	}
	password, hash, err := newBotPassword()
	if err == nil {
		err = s.database.UpdatePassword(a.UserID, hash)
	}
	if err != nil {
		log.Printf("bot api: reset password of user %d: %v", a.UserID, err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	log.Printf("bot api: password reset for %s@%s (user %d)", a.Username, b.domain, a.UserID)
	lctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	s.lunlunPassword(lctx, a.UserID, password)
	cancel()
	respondJSON(w, http.StatusOK, map[string]string{
		"login": a.Username, "address": a.Username + "@" + b.domain, "password": password,
	})
}

func newBotPassword() (password, hash string, err error) {
	password, err = signup.NewPassword(botPasswordLen)
	if err != nil {
		return "", "", fmt.Errorf("generate password: %w", err)
	}
	hash, err = HashPassword(password)
	if err != nil {
		return "", "", fmt.Errorf("hash password: %w", err)
	}
	return password, hash, nil
}

// botLinkedAccount resolves the Telegram user of the request to its account,
// answering 404/403 itself when there is none.
func (s *Server) botLinkedAccount(w http.ResponseWriter, tg int64) (*db.TelegramAccount, bool) {
	a, err := s.database.GetTelegramAccount(tg)
	switch {
	case errors.Is(err, db.ErrTelegramNotLinked):
		botError(w, http.StatusNotFound, "not_found")
	case err != nil:
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
	case a.Banned:
		botError(w, http.StatusForbidden, "banned")
	default:
		return a, true
	}
	return nil, false
}

// handleBotBilling: balance and the features on sale with their state.
func (s *Server) handleBotBilling(w http.ResponseWriter, r *http.Request) {
	a, ok := s.botLinkedAccount(w, tgUserID(r))
	if !ok {
		return
	}
	balance, err := s.database.Balance(a.UserID)
	var subs []db.Subscription
	if err == nil {
		subs, err = s.database.Subscriptions(a.UserID)
	}
	if err != nil {
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	now := time.Now().UnixMilli()
	out := []botFeature{}
	for _, f := range botFeatureCatalog {
		price := s.bot.prices[f.Code]
		if f.Code == db.FeatureMailingList { // the smallest tier: «от N ⭐»
			price = s.bot.prices[db.ListTierPriceKey(db.ListTiers[0])]
		}
		if price <= 0 {
			continue
		}
		f.Price = price
		for _, sub := range subs { // newest first
			if sub.Feature == f.Code {
				f.Status, f.PaidUntil, f.Working = sub.Status, sub.PaidUntil, sub.Working(now)
				break
			}
		}
		out = append(out, f)
	}
	respondJSON(w, http.StatusOK, map[string]any{"balance": balance, "features": out})
}

func (s *Server) handleBotEnableFeature(w http.ResponseWriter, r *http.Request) {
	a, ok := s.botLinkedAccount(w, tgUserID(r))
	if !ok {
		return
	}
	code := mux.Vars(r)["code"]
	price := s.bot.prices[code]
	if price <= 0 || code == db.FeatureMailbox {
		// Mailboxes are subscribed to one by one, when made in the cabinet.
		botError(w, http.StatusBadRequest, "unknown_feature")
		return
	}
	sub, balance, err := s.database.EnableFeature(a.UserID, code, 0, price)
	if errors.Is(err, db.ErrInsufficientFunds) {
		respondJSON(w, http.StatusPaymentRequired, map[string]any{"error": "insufficient", "balance": balance, "price": price})
		return
	}
	if err != nil {
		log.Printf("bot api: enable %s for %s: %v", code, a.Username, err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	log.Printf("bot api: %s enabled %s, balance %d", a.Username, code, balance)
	respondJSON(w, http.StatusOK, map[string]any{"balance": balance, "status": sub.Status, "paid_until": sub.PaidUntil})
}

func (s *Server) handleBotCancelFeature(w http.ResponseWriter, r *http.Request) {
	a, ok := s.botLinkedAccount(w, tgUserID(r))
	if !ok {
		return
	}
	code := mux.Vars(r)["code"]
	subs, err := s.database.Subscriptions(a.UserID)
	if err != nil {
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	for _, sub := range subs {
		if sub.Feature == code && sub.Status == "active" {
			if err := s.database.CancelSubscription(a.UserID, sub.ID); err != nil {
				log.Printf("bot api: %v", err)
				botError(w, http.StatusInternalServerError, "internal")
				return
			}
			respondJSON(w, http.StatusOK, map[string]any{"status": "cancelled", "paid_until": sub.PaidUntil})
			return
		}
	}
	botError(w, http.StatusNotFound, "not_subscribed")
}

type botTopUpRequest struct {
	TgUserID int64  `json:"tg_user_id"`
	Amount   int    `json:"amount"`
	ChargeID string `json:"charge_id"`
}

// handleBotTopUp records a Stars payment onto the balance.
func (s *Server) handleBotTopUp(w http.ResponseWriter, r *http.Request) {
	var req botTopUpRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.TgUserID <= 0 || req.Amount <= 0 || req.ChargeID == "" {
		botError(w, http.StatusBadRequest, "bad_request")
		return
	}
	a, ok := s.botLinkedAccount(w, req.TgUserID)
	if !ok {
		log.Printf("bot api: top-up %s from Telegram %d without an account — refund", req.ChargeID, req.TgUserID)
		return
	}
	balance, err := s.database.AddTopUp(a.UserID, req.Amount, req.ChargeID)
	if err != nil {
		log.Printf("bot api: top-up %s: %v", req.ChargeID, err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	log.Printf("bot api: %s topped up %d stars, balance %d", a.Username, req.Amount, balance)
	respondJSON(w, http.StatusOK, map[string]any{"balance": balance})
}

func (s *Server) handleBotRefundable(w http.ResponseWriter, r *http.Request) {
	a, ok := s.botLinkedAccount(w, tgUserID(r))
	if !ok {
		return
	}
	list, err := s.database.RefundableTopUps(a.UserID)
	if err != nil {
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"topups": list})
}

func (s *Server) handleBotReserveRefund(w http.ResponseWriter, r *http.Request) {
	a, ok := s.botLinkedAccount(w, tgUserID(r))
	if !ok {
		return
	}
	t, err := s.database.ReserveRefund(a.UserID, mux.Vars(r)["charge"])
	if errors.Is(err, db.ErrNotRefundable) {
		botError(w, http.StatusConflict, "not_refundable")
		return
	}
	if err != nil {
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	log.Printf("bot api: %s refund of %d stars reserved", a.Username, t.Amount)
	respondJSON(w, http.StatusOK, map[string]any{"amount": t.Amount})
}

func (s *Server) handleBotCancelRefund(w http.ResponseWriter, r *http.Request) {
	a, ok := s.botLinkedAccount(w, tgUserID(r))
	if !ok {
		return
	}
	if err := s.database.CancelRefund(a.UserID, mux.Vars(r)["charge"]); err != nil {
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
