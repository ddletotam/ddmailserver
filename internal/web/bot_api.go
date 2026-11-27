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
