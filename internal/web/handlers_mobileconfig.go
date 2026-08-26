package web

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/mobileconfig"
)

// appPasswordProfileLabel is the label given to the credential minted for a
// device profile. Re-exporting revokes the previous one under the same label,
// so a user who exports twice ends up with one working profile rather than an
// unbounded set of live credentials nobody can account for.
const appPasswordProfileLabel = "Device profile"

// HandleExportMobileconfig emits an Apple configuration profile for the signed-in
// user, pointing at this server: IMAP for mail, CalDAV for calendars, CardDAV
// for contacts. One file, one account on the device, everything this server has
// already aggregated behind it.
//
// The credential in the profile is an application password, never the account
// password. .mobileconfig has no way to encrypt a secret, so whatever goes in
// is readable by anyone who gets the file; making it a revocable credential is
// what keeps that survivable.
func (s *Server) HandleExportMobileconfig(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	user, err := s.database.GetUserByID(userID)
	if err != nil {
		respondError(w, http.StatusNotFound, "user not found")
		return
	}

	public := s.publicEndpoints
	if public.Hostname == "" {
		// Nothing to point the device at. Better a clear error than a profile
		// that installs and then silently fails to connect to "".
		respondError(w, http.StatusInternalServerError, "public hostname is not configured")
		return
	}

	email := user.Email
	if email == "" {
		email = fmt.Sprintf("%s@%s", user.Username, public.Hostname)
	}

	// "1" (the default) embeds a freshly minted application password;
	// "0" emits a profile that prompts on the device instead.
	includeSecret := r.URL.Query().Get("password") != "0"

	var secret string
	if includeSecret {
		// Retire the previous profile's credential first. A phone that was set
		// up from an older export stops working, which is the intended
		// meaning of re-exporting.
		if revoked, err := s.database.RevokeAppPasswordsByLabel(userID, appPasswordProfileLabel); err != nil {
			log.Printf("mobileconfig export: failed to revoke previous profile credentials for user %d: %v", userID, err)
			respondError(w, http.StatusInternalServerError, "failed to rotate profile credential")
			return
		} else if revoked > 0 {
			log.Printf("mobileconfig export: revoked %d previous profile credential(s) for user %d", revoked, userID)
		}

		_, secret, err = s.database.CreateAppPassword(userID, appPasswordProfileLabel)
		if err != nil {
			log.Printf("mobileconfig export: failed to create app password for user %d: %v", userID, err)
			respondError(w, http.StatusInternalServerError, "failed to create application password")
			return
		}
	}

	profile := &mobileconfig.Profile{
		DisplayName:  email,
		Organization: public.Hostname,
		AccountName:  user.Username,
		EmailAddress: email,
		Username:     user.Username,
		Secret:       secret,
		Hostname:     public.Hostname,
		IMAPPort:     public.IMAPPort,
		SMTPPort:     public.SMTPPort,
		HTTPSPort:    public.HTTPSPort,
		CalDAVPath:   "/caldav/",
		CardDAVPath:  "/carddav/",
	}

	data, err := mobileconfig.Generate(profile)
	if err != nil {
		log.Printf("mobileconfig export: generation failed for user %d: %v", userID, err)
		respondError(w, http.StatusInternalServerError, "failed to generate profile")
		return
	}

	filename := sanitizeFilename(email) + ".mobileconfig"

	w.Header().Set("Content-Type", "application/x-apple-aspen-config")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	// The body carries a live credential; no cache, no store, anywhere.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	w.Write(data)

	log.Printf("mobileconfig export: profile issued for user %d (with credential: %t)", userID, includeSecret)
}

// sanitizeFilename reduces an address to something safe in a Content-Disposition
// header and on every filesystem the file might land on.
func sanitizeFilename(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == '@':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	out := sb.String()
	if out == "" {
		return "profile"
	}
	return out
}

// HandleListAppPasswords returns the user's application passwords. The secrets
// themselves are not stored and cannot appear here — only the label, the last
// four characters, and when each was created, last used and revoked.
func (s *Server) HandleListAppPasswords(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	passwords, err := s.database.ListAppPasswords(userID)
	if err != nil {
		log.Printf("app passwords: list failed for user %d: %v", userID, err)
		respondError(w, http.StatusInternalServerError, "failed to list application passwords")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"app_passwords": passwords})
}

// CreateAppPasswordRequest is the body of a create call.
type CreateAppPasswordRequest struct {
	Label string `json:"label"`
}

// HandleCreateAppPassword issues a new application password and returns it in
// the clear — the only time it is ever visible. The caller must show it to the
// user immediately; there is no way to retrieve it afterwards.
func (s *Server) HandleCreateAppPassword(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var req CreateAppPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	label := strings.TrimSpace(req.Label)
	if label == "" {
		respondError(w, http.StatusBadRequest, "label is required")
		return
	}
	if len(label) > 255 {
		respondError(w, http.StatusBadRequest, "label is too long")
		return
	}

	record, secret, err := s.database.CreateAppPassword(userID, label)
	if err != nil {
		log.Printf("app passwords: create failed for user %d: %v", userID, err)
		respondError(w, http.StatusInternalServerError, "failed to create application password")
		return
	}

	log.Printf("app passwords: issued %q for user %d", label, userID)

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"app_password": record,
		// Shown once. Deliberately absent from every other response.
		"secret": secret,
	})
}

// HandleRevokeAppPassword withdraws one application password.
func (s *Server) HandleRevokeAppPassword(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid id")
		return
	}

	// Scoped by user inside the query, so an id belonging to someone else
	// reports "not found" rather than revoking their credential.
	if err := s.database.RevokeAppPassword(userID, id); err != nil {
		log.Printf("app passwords: revoke %d failed for user %d: %v", id, userID, err)
		respondError(w, http.StatusNotFound, "application password not found")
		return
	}

	log.Printf("app passwords: revoked %d for user %d", id, userID)

	respondJSON(w, http.StatusOK, map[string]interface{}{"revoked": true})
}
