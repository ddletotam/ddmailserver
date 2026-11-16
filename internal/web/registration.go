package web

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
	"github.com/gorilla/mux"
)

// Registration as a server feature (docs/server-admin-split-plan.md, stages
// 5–6): three modes set by the administrator, invitations, approval.

const inviteTTL = 7 * 24 * time.Hour

// registrationMode: the administrator's setting; without one — the config
// (security.disable_registration: closed, otherwise open).
func (s *Server) registrationMode() string {
	if s.database != nil {
		if m, err := s.database.GetSetting(db.SettingRegistrationMode); err == nil && db.ValidRegistrationMode(m) {
			return m
		}
	}
	if s.registrationDisabled {
		return db.RegistrationClosed
	}
	return db.RegistrationOpen
}

// HandleInvitePage: the sign-up form behind an invitation link.
func (s *Server) HandleInvitePage(w http.ResponseWriter, r *http.Request) {
	token := mux.Vars(r)["token"]
	_, err := s.database.InviteByToken(token)
	s.renderTemplate(w, "invite.html", struct {
		PageData
		Token string
		Valid bool
	}{PageData: PageData{Title: "Invitation"}, Token: token, Valid: err == nil})
}

// HandleInviteRegister creates an approved account from an invitation,
// whatever the registration mode.
func (s *Server) HandleInviteRegister(w http.ResponseWriter, r *http.Request) {
	inv, err := s.database.InviteByToken(mux.Vars(r)["token"])
	if err != nil {
		respondHTMXError(w, r, http.StatusGone, s.i18n.T("auth.invite_invalid"))
		return
	}
	s.registerAccount(w, r, false, inv)
}

// HandleAdminSetRegistrationMode: POST mode=open|closed|moderated.
func (s *Server) HandleAdminSetRegistrationMode(w http.ResponseWriter, r *http.Request) {
	mode := strings.TrimSpace(r.FormValue("mode"))
	if !db.ValidRegistrationMode(mode) {
		respondError(w, http.StatusBadRequest, "mode must be open, closed or moderated")
		return
	}
	if err := s.database.SetSetting(db.SettingRegistrationMode, mode); err != nil {
		log.Printf("set registration mode: %v", err)
		respondError(w, http.StatusInternalServerError, "save failed")
		return
	}
	s.auditAdmin(r, s.GetUserFromContext(r.Context()), "registration.mode", "settings", 0, mode)
	respondJSON(w, http.StatusOK, map[string]string{"mode": mode})
}

// HandleAdminApproveUser lets a moderated sign-up in.
func (s *Server) HandleAdminApproveUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.adminParseUserID(w, r, false)
	if !ok {
		return
	}
	target, err := s.database.GetUserByID(id)
	if err != nil {
		respondError(w, http.StatusNotFound, "user not found")
		return
	}
	if err := s.database.ApproveUser(id); err != nil {
		log.Printf("approve user: %v", err)
		respondError(w, http.StatusInternalServerError, "approve failed")
		return
	}
	s.auditAdmin(r, s.GetUserFromContext(r.Context()), "user.approve", "user", id, target.Username)
	respondJSON(w, http.StatusOK, map[string]bool{"approved": true})
}

// HandleAdminCreateInvite returns the link once; only its hash is stored.
func (s *Server) HandleAdminCreateInvite(w http.ResponseWriter, r *http.Request) {
	admin := s.GetUserFromContext(r.Context())
	note := strings.TrimSpace(r.FormValue("note"))
	if len(note) > 200 {
		note = note[:200]
	}
	token, inv, err := s.database.CreateInvite(admin.ID, note, inviteTTL.Milliseconds())
	if err != nil {
		log.Printf("create invite: %v", err)
		respondError(w, http.StatusInternalServerError, "create failed")
		return
	}
	s.auditAdmin(r, admin, "invite.create", "invite", inv.ID, note)
	respondJSON(w, http.StatusCreated, map[string]string{
		"url":     s.publicBaseURL(r) + "/invite/" + token,
		"expires": timeutil.FromMs(inv.ExpiresAt).Format("2006-01-02 15:04"),
	})
}

// HandleAdminRevokeInvite deletes an unused invitation.
func (s *Server) HandleAdminRevokeInvite(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil || id <= 0 {
		respondError(w, http.StatusBadRequest, "invalid invite id")
		return
	}
	if err := s.database.RevokeInvite(id); err != nil {
		log.Printf("revoke invite: %v", err)
		respondError(w, http.StatusInternalServerError, "revoke failed")
		return
	}
	s.auditAdmin(r, s.GetUserFromContext(r.Context()), "invite.revoke", "invite", id, "")
	respondJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

// adminInviteRow is an active invitation on the admin page.
type adminInviteRow struct {
	ID      int64
	Note    string
	Created string
	Expires string
}

func (s *Server) adminInviteRows() []adminInviteRow {
	invs, err := s.database.ListActiveInvites()
	if err != nil {
		log.Printf("list invites: %v", err)
		return nil
	}
	rows := make([]adminInviteRow, 0, len(invs))
	for _, inv := range invs {
		rows = append(rows, adminInviteRow{
			ID: inv.ID, Note: inv.Note,
			Created: timeutil.FromMs(inv.CreatedAt).Format("2006-01-02 15:04"),
			Expires: timeutil.FromMs(inv.ExpiresAt).Format("2006-01-02 15:04"),
		})
	}
	return rows
}
