package web

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// adminUserRow is the projection rendered in the admin user table.
type adminUserRow struct {
	ID        int64
	Username  string
	Email     string
	IsAdmin   bool
	IsBanned  bool
	IsCurrent bool // self-modification of admin/ban/delete is disabled in UI
	CreatedAt string
}

func toAdminUserRows(users []*models.User, currentID int64) []adminUserRow {
	rows := make([]adminUserRow, 0, len(users))
	for _, u := range users {
		rows = append(rows, adminUserRow{
			ID:        u.ID,
			Username:  u.Username,
			Email:     u.Email,
			IsAdmin:   u.IsAdmin(),
			IsBanned:  u.IsBanned(),
			IsCurrent: u.ID == currentID,
			CreatedAt: timeutil.FromMs(u.CreatedAt).Format("2006-01-02"),
		})
	}
	return rows
}

// HandleAdminPage renders the admin/installation settings page (OAuth + user
// management). Already gated by WebAdminMiddleware, so we don't re-check here.
func (s *Server) HandleAdminPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())

	googleSettings, _ := s.database.GetGoogleOAuthSettings()
	if googleSettings == nil {
		googleSettings = &db.GoogleOAuthSettings{}
	}
	microsoftSettings, _ := s.database.GetMicrosoftOAuthSettings()
	if microsoftSettings == nil {
		microsoftSettings = &db.MicrosoftOAuthSettings{}
	}

	scheme := "https"
	host := r.Host
	if fwdHost := r.Header.Get("X-Forwarded-Host"); fwdHost != "" {
		host = fwdHost
	}
	if fwdProto := r.Header.Get("X-Forwarded-Proto"); fwdProto != "" {
		scheme = fwdProto
	} else if r.TLS == nil && (strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.0.0.1")) {
		scheme = "http"
	}
	googleRedirectURI := fmt.Sprintf("%s://%s/oauth/google/callback", scheme, host)
	microsoftRedirectURI := fmt.Sprintf("%s://%s/oauth/microsoft/callback", scheme, host)

	users, err := s.database.ListUsers()
	if err != nil {
		log.Printf("HandleAdminPage: failed to list users: %v", err)
	}

	data := struct {
		PageData
		GoogleOAuthSettings    *db.GoogleOAuthSettings
		MicrosoftOAuthSettings *db.MicrosoftOAuthSettings
		GoogleRedirectURI      string
		MicrosoftRedirectURI   string
		GoogleOAuthEnabled     bool
		MicrosoftOAuthEnabled  bool
		Users                  []adminUserRow
	}{
		PageData: PageData{
			Title: "Server Settings",
			User:  user,
		},
		GoogleOAuthSettings:    googleSettings,
		MicrosoftOAuthSettings: microsoftSettings,
		GoogleRedirectURI:      googleRedirectURI,
		MicrosoftRedirectURI:   microsoftRedirectURI,
		GoogleOAuthEnabled:     s.googleOAuth != nil,
		MicrosoftOAuthEnabled:  s.microsoftOAuth != nil,
		Users:                  toAdminUserRows(users, user.ID),
	}

	s.renderTemplate(w, "admin.html", data)
}

// HandleAdminListUsers returns the user table fragment as HTMX HTML.
func (s *Server) HandleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	current := s.GetUserFromContext(r.Context())
	users, err := s.database.ListUsers()
	if err != nil {
		log.Printf("HandleAdminListUsers: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	rows := toAdminUserRows(users, current.ID)
	if r.Header.Get("Accept") == "application/json" {
		respondJSON(w, http.StatusOK, rows)
		return
	}
	s.renderTemplatePartial(w, "admin.html", "admin-users-table", rows)
}

// adminParseUserID extracts {id} from the route, refusing self-modification.
// Returns (id, ok). When ok is false a response has already been written.
func (s *Server) adminParseUserID(w http.ResponseWriter, r *http.Request, allowSelf bool) (int64, bool) {
	idStr := mux.Vars(r)["id"]
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		respondError(w, http.StatusBadRequest, "invalid user id")
		return 0, false
	}
	if !allowSelf {
		current := s.GetUserFromContext(r.Context())
		if current != nil && current.ID == id {
			respondError(w, http.StatusBadRequest, "cannot target your own account")
			return 0, false
		}
	}
	return id, true
}

// HandleAdminToggleAdmin flips is_admin on a target user. Refuses to demote the
// last admin so the install never ends up with zero admins.
func (s *Server) HandleAdminToggleAdmin(w http.ResponseWriter, r *http.Request) {
	id, ok := s.adminParseUserID(w, r, false)
	if !ok {
		return
	}
	target, err := s.database.GetUserByID(id)
	if err != nil {
		respondError(w, http.StatusNotFound, "user not found")
		return
	}
	newVal := !target.IsAdmin()
	if !newVal {
		// Demoting — make sure another admin remains.
		count, err := s.database.CountAdmins()
		if err != nil {
			respondError(w, http.StatusInternalServerError, "admin count failed")
			return
		}
		if count <= 1 {
			respondError(w, http.StatusBadRequest, "cannot demote the last admin")
			return
		}
	}
	if err := s.database.SetUserAdmin(id, newVal); err != nil {
		log.Printf("SetUserAdmin failed: %v", err)
		respondError(w, http.StatusInternalServerError, "update failed")
		return
	}
	log.Printf("admin: user %d is_admin=%v (by user %d)", id, newVal, s.GetUserFromContext(r.Context()).ID)
	respondJSON(w, http.StatusOK, map[string]bool{"is_admin": newVal})
}

// HandleAdminToggleBan flips is_banned on a target user. Refuses to ban the
// last admin (would lock the install out).
func (s *Server) HandleAdminToggleBan(w http.ResponseWriter, r *http.Request) {
	id, ok := s.adminParseUserID(w, r, false)
	if !ok {
		return
	}
	target, err := s.database.GetUserByID(id)
	if err != nil {
		respondError(w, http.StatusNotFound, "user not found")
		return
	}
	newVal := !target.IsBanned()
	if newVal && target.IsAdmin() {
		count, err := s.database.CountAdmins()
		if err != nil {
			respondError(w, http.StatusInternalServerError, "admin count failed")
			return
		}
		if count <= 1 {
			respondError(w, http.StatusBadRequest, "cannot ban the last admin")
			return
		}
	}
	if err := s.database.SetUserBanned(id, newVal); err != nil {
		log.Printf("SetUserBanned failed: %v", err)
		respondError(w, http.StatusInternalServerError, "update failed")
		return
	}
	log.Printf("admin: user %d is_banned=%v (by user %d)", id, newVal, s.GetUserFromContext(r.Context()).ID)
	respondJSON(w, http.StatusOK, map[string]bool{"is_banned": newVal})
}

// HandleAdminDeleteUser removes a user (CASCADE wipes their data). Refuses to
// delete the last admin or self.
func (s *Server) HandleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.adminParseUserID(w, r, false)
	if !ok {
		return
	}
	target, err := s.database.GetUserByID(id)
	if err != nil {
		respondError(w, http.StatusNotFound, "user not found")
		return
	}
	if target.IsAdmin() {
		count, err := s.database.CountAdmins()
		if err != nil {
			respondError(w, http.StatusInternalServerError, "admin count failed")
			return
		}
		if count <= 1 {
			respondError(w, http.StatusBadRequest, "cannot delete the last admin")
			return
		}
	}
	if err := s.database.DeleteUser(id); err != nil {
		log.Printf("DeleteUser failed: %v", err)
		respondError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	log.Printf("admin: user %d deleted (by user %d)", id, s.GetUserFromContext(r.Context()).ID)
	respondJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
