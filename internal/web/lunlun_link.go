package web

import (
	"context"
	"crypto/subtle"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/maillink"
	"github.com/gorilla/mux"
)

// Связка с lunlun.io (internal/maillink, docs/lunlun-bot-plan.md).
//
//	GET /api/link/v1/space/{user}  — для lunlun: квота и место почты (напрямую на 127.0.0.1, с секретом)
//	GET /sso/lunlun                — из кабинета почты в файлы, без пароля
//	GET /sso?t=<пропуск>           — из lunlun.io в кабинет почты, без пароля

type lunlunLink struct {
	client   *maillink.Lunlun
	verifier *maillink.Verifier
}

// SetLunlun switches the link with lunlun.io on.
func (s *Server) SetLunlun(c *maillink.Lunlun) {
	if c == nil || len(c.Secret) == 0 {
		return
	}
	s.lunlun = &lunlunLink{client: c, verifier: maillink.NewVerifier(c.Secret)}
}

func (s *Server) registerLunlunRoutes() {
	s.router.HandleFunc("/api/link/v1/space/{user:[0-9]+}", s.handleLinkSpace).Methods("GET")
	s.router.HandleFunc("/sso", s.handleSSOFromLunlun).Methods("GET")
}

func (s *Server) handleLinkSpace(w http.ResponseWriter, r *http.Request) {
	l := s.lunlun
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if l == nil || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" ||
		!ok || subtle.ConstantTimeCompare([]byte(tok), l.client.Secret) != 1 {
		http.NotFound(w, r)
		return
	}
	id, _ := strconv.ParseInt(mux.Vars(r)["user"], 10, 64)
	sp, err := s.database.SpaceOf(id)
	if err != nil {
		log.Printf("link: space of %d: %v", id, err)
		respondError(w, http.StatusInternalServerError, "internal")
		return
	}
	respondJSON(w, http.StatusOK, sp)
}

// HandleSSOToLunlun: the cabinet's «Файлы» — into lunlun.io without a password.
func (s *Server) HandleSSOToLunlun(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if s.lunlun == nil || user == nil {
		http.Redirect(w, r, "https://lunlun.io", http.StatusSeeOther)
		return
	}
	if ok, _ := s.database.HasLunlunLink(user.ID); !ok {
		http.Redirect(w, r, "https://lunlun.io", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, s.lunlun.client.SSO+"?t="+maillink.Sign(s.lunlun.client.Secret, user.ID, time.Now()), http.StatusSeeOther)
}

// handleSSOFromLunlun: «Почта» in lunlun.io — into the mail cabinet.
func (s *Server) handleSSOFromLunlun(w http.ResponseWriter, r *http.Request) {
	if s.lunlun == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	id, err := s.lunlun.verifier.Verify(r.URL.Query().Get("t"), time.Now())
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	u, err := s.database.GetUserByID(id)
	if err != nil || u.Blocked() || u.IsAdmin() || u.IsMailbox() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	token, err := GenerateToken(u.ID, u.Username, s.jwtSecret)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.SetSessionCookie(w, r, token)
	http.Redirect(w, r, "/domains", http.StatusSeeOther)
}

// ProvisionLunlun makes the lunlun.io account of a service user; password ""
// — a random one (the user signs in through the cabinet, or resets it in the
// bot, which sets both).
func (s *Server) ProvisionLunlun(ctx context.Context, userID int64, address, password string) {
	if s.lunlun == nil {
		return
	}
	if password == "" {
		p, _, err := newBotPassword()
		if err != nil {
			return
		}
		password = p
	}
	id, err := s.lunlun.client.CreateUser(ctx, address, password, userID)
	if err != nil {
		log.Printf("link: lunlun account for %s: %v", address, err)
		return
	}
	if err := s.database.SetLunlunLink(userID, id); err != nil {
		log.Printf("link: %v", err)
		return
	}
	log.Printf("link: lunlun account %d for %s", id, address)
}

// lunlunPassword keeps the lunlun.io password the same as the mail one.
func (s *Server) lunlunPassword(ctx context.Context, userID int64, password string) {
	if s.lunlun == nil {
		return
	}
	if ok, _ := s.database.HasLunlunLink(userID); !ok {
		return
	}
	if err := s.lunlun.client.SetPassword(ctx, userID, password); err != nil {
		log.Printf("link: lunlun password of user %d: %v", userID, err)
	}
}

// RunLunlunReconcile gives every service user without a lunlun.io account one,
// hourly — for sign-ups made while lunlun was down and those before the link.
func (s *Server) RunLunlunReconcile(ctx context.Context) {
	if s.lunlun == nil {
		return
	}
	for {
		if s.bot != nil {
			users, err := s.database.ServiceUsersWithoutLunlun(50)
			if err != nil {
				log.Printf("link: reconcile: %v", err)
			}
			for _, u := range users {
				c, cancel := context.WithTimeout(ctx, 15*time.Second)
				s.ProvisionLunlun(c, u.UserID, u.Username+"@"+s.bot.domain, "")
				cancel()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}
