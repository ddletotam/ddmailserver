package web

import (
	"log"
	"net/http"
	"strconv"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
	"github.com/gorilla/mux"
)

// adminUserRow is the projection rendered in the admin user table.
type adminUserRow struct {
	ID        int64
	Username  string
	Email     string
	IsBanned  bool
	IsCurrent bool // self-modification of admin/ban/delete is disabled in UI
	CreatedAt string
}

func toAdminUserRows(users []*models.User, currentID int64) []adminUserRow {
	rows := make([]adminUserRow, 0, len(users))
	for _, u := range users {
		if u.IsAdmin() {
			continue // administrator accounts are not users (migration 061)
		}
		rows = append(rows, adminUserRow{
			ID:        u.ID,
			Username:  u.Username,
			Email:     u.Email,
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

	// Forwarding headers count only from a trusted proxy, and the host
	// must be one this server is known by — see publicBaseURL.
	base := s.publicBaseURL(r)
	googleRedirectURI := base + "/oauth/google/callback"
	microsoftRedirectURI := base + "/oauth/microsoft/callback"

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
		Audit                  []adminAuditRow
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
		Audit:                  s.adminAuditRows(),
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

// HandleAdminToggleBan flips is_banned on a target user.
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
	if target.IsAdmin() {
		respondError(w, http.StatusBadRequest, "administrator accounts are not managed here")
		return
	}
	newVal := !target.IsBanned()
	if err := s.database.SetUserBanned(id, newVal); err != nil {
		log.Printf("SetUserBanned failed: %v", err)
		respondError(w, http.StatusInternalServerError, "update failed")
		return
	}
	action := "user.ban"
	if !newVal {
		action = "user.unban"
	}
	s.auditAdmin(r, s.GetUserFromContext(r.Context()), action, "user", id, target.Username)
	respondJSON(w, http.StatusOK, map[string]bool{"is_banned": newVal})
}

// HandleAdminDeleteUser removes a user (CASCADE wipes their data).
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
		respondError(w, http.StatusBadRequest, "administrator accounts are not managed here")
		return
	}
	if err := s.database.DeleteUser(id); err != nil {
		log.Printf("DeleteUser failed: %v", err)
		respondError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	s.auditAdmin(r, s.GetUserFromContext(r.Context()), "user.delete", "user", id, target.Username)
	respondJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// adminAuditRow is one line of the audit table on the admin page.
type adminAuditRow struct {
	When, Who, Action, Target, IP string
}

func (s *Server) adminAuditRows() []adminAuditRow {
	actions, err := s.database.ListAdminActions(100)
	if err != nil {
		log.Printf("admin audit list: %v", err)
		return nil
	}
	rows := make([]adminAuditRow, 0, len(actions))
	for _, a := range actions {
		target := a.TargetType
		if a.TargetID != 0 {
			target += " #" + strconv.FormatInt(a.TargetID, 10)
		}
		if a.Details != "" {
			target += " " + a.Details
		}
		rows = append(rows, adminAuditRow{
			When:   timeutil.FromMs(a.CreatedAt).Format("2006-01-02 15:04"),
			Who:    a.AdminName,
			Action: a.Action,
			Target: target,
			IP:     a.IP,
		})
	}
	return rows
}
