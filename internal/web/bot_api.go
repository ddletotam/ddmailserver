package web

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

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
//	GET  /api/bot/v1/accounts/{tg}/features  → 200 {features:[{code,title,description,price,until}]}
//	POST /api/bot/v1/payments                {tg_user_id,feature,amount,charge_id,recurring,expires_at}
//	                                         → 200 {feature,until} | 404 not_found | 400 unknown_feature|wrong_amount
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
}

// botFeature is a paid feature as the bot sells it.
type botFeature struct {
	Code        string `json:"code"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Price       int    `json:"price"`
	Until       int64  `json:"until,omitempty"` // ms; past — expired
}

// botFeatureCatalog: order and wording of the paid features. No brands of
// mail services anywhere — only IMAP/SMTP terms (owner's rule, 2026-11-27).
var botFeatureCatalog = []botFeature{
	{Code: db.FeatureExternalAccounts, Title: "Другие ящики",
		Description: "Сервер забирает почту из других ящиков по IMAP и отправляет через их SMTP — почта всех адресов видна на всех устройствах."},
	{Code: db.FeatureExtraStorage, Title: "+10 ГБ",
		Description: "Ящик 11 ГБ вместо 1 ГБ."},
	{Code: db.FeatureSendLimits, Title: "Отправка без лимитов",
		Description: "Без суточных ограничений бесплатного ящика на число писем и получателей."},
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
	r.HandleFunc("/accounts/{tg:[0-9]+}/features", s.handleBotFeatures).Methods("GET")
	r.HandleFunc("/payments", s.handleBotPayment).Methods("POST")
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

// handleBotFeatures: the paid features on sale and how long each is paid for.
func (s *Server) handleBotFeatures(w http.ResponseWriter, r *http.Request) {
	a, ok := s.botLinkedAccount(w, tgUserID(r))
	if !ok {
		return
	}
	paid, err := s.database.Entitlements(a.UserID)
	if err != nil {
		log.Printf("bot api: %v", err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	out := []botFeature{}
	for _, f := range botFeatureCatalog {
		price := s.bot.prices[f.Code]
		if price <= 0 {
			continue
		}
		f.Price, f.Until = price, paid[f.Code]
		out = append(out, f)
	}
	respondJSON(w, http.StatusOK, map[string]any{"features": out})
}

type botPaymentRequest struct {
	TgUserID  int64  `json:"tg_user_id"`
	Feature   string `json:"feature"`
	Amount    int    `json:"amount"`
	ChargeID  string `json:"charge_id"`
	Recurring bool   `json:"recurring"`
	ExpiresAt int64  `json:"expires_at"` // unix seconds, Telegram's subscription_expiration_date; 0 — none
}

// handleBotPayment records a successful Stars payment and extends the
// feature. The amount is checked against the price only on the first charge:
// a renewal is billed at the price the subscription was made at.
func (s *Server) handleBotPayment(w http.ResponseWriter, r *http.Request) {
	var req botPaymentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.TgUserID <= 0 || req.ChargeID == "" {
		botError(w, http.StatusBadRequest, "bad_request")
		return
	}
	price, known := s.bot.prices[req.Feature]
	if !known {
		log.Printf("bot api: payment %s for unknown feature %q, %d stars — refund by hand", req.ChargeID, req.Feature, req.Amount)
		botError(w, http.StatusBadRequest, "unknown_feature")
		return
	}
	if !req.Recurring && req.Amount < price {
		log.Printf("bot api: payment %s for %s: %d stars, price %d — refund by hand", req.ChargeID, req.Feature, req.Amount, price)
		botError(w, http.StatusBadRequest, "wrong_amount")
		return
	}
	a, ok := s.botLinkedAccount(w, req.TgUserID)
	if !ok {
		log.Printf("bot api: payment %s from Telegram %d without an account — refund by hand", req.ChargeID, req.TgUserID)
		return
	}
	until, err := s.database.RecordPayment(db.Payment{
		UserID: a.UserID, TgUserID: req.TgUserID, Feature: req.Feature, Amount: req.Amount,
		ChargeID: req.ChargeID, Recurring: req.Recurring, ExpiresAt: req.ExpiresAt * 1000,
	})
	if err != nil {
		log.Printf("bot api: payment %s: %v", req.ChargeID, err)
		botError(w, http.StatusInternalServerError, "internal")
		return
	}
	log.Printf("bot api: %s paid %s (%d stars, recurring=%v) until %d", a.Username, req.Feature, req.Amount, req.Recurring, until)
	respondJSON(w, http.StatusOK, map[string]any{"feature": req.Feature, "until": until})
}
