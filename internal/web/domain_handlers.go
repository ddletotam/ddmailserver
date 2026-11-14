package web

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
	"github.com/gorilla/mux"
)

// domainVerifyPrefix starts the TXT record that proves ownership of a domain:
// "ddmail-verify=<token>" at the domain itself.
const domainVerifyPrefix = "ddmail-verify="

// domainVerifyTimeout bounds the DNS lookup behind the "Verify" button.
const domainVerifyTimeout = 10 * time.Second

// DomainVerificationRecord is the DNS record the owner publishes.
type DomainVerificationRecord struct {
	Type  string `json:"type"`  // always "TXT"
	Name  string `json:"name"`  // the domain itself
	Value string `json:"value"` // ddmail-verify=<token>
}

// DomainView is a domain as the API shows it to its owner.
type DomainView struct {
	ID             int64  `json:"id"`
	Domain         string `json:"domain"`
	Enabled        bool   `json:"enabled"`
	Verified       bool   `json:"verified"`
	VerifiedMethod string `json:"verified_method,omitempty"`
	// Status: "pending_verification" until ownership is proven, then
	// "active" or "disabled".
	Status       string                    `json:"status"`
	CreatedAt    int64                     `json:"created_at"`
	Verification *DomainVerificationRecord `json:"verification,omitempty"`
}

// Domain statuses reported by DomainView.Status.
const (
	DomainStatusPending  = "pending_verification"
	DomainStatusActive   = "active"
	DomainStatusDisabled = "disabled"
)

func domainVerificationRecord(d *models.Domain) DomainVerificationRecord {
	return DomainVerificationRecord{Type: "TXT", Name: d.Domain, Value: domainVerifyPrefix + d.VerifyToken}
}

func newDomainView(d *models.Domain) DomainView {
	v := DomainView{
		ID:             d.ID,
		Domain:         d.Domain,
		Enabled:        d.Enabled,
		Verified:       d.Verified,
		VerifiedMethod: d.VerifiedMethod,
		CreatedAt:      d.CreatedAt,
	}
	switch {
	case !d.Verified:
		v.Status = DomainStatusPending
		rec := domainVerificationRecord(d)
		v.Verification = &rec
	case d.Enabled:
		v.Status = DomainStatusActive
	default:
		v.Status = DomainStatusDisabled
	}
	return v
}

// validDomainName accepts a lower-cased DNS name of at least two labels:
// letters, digits and hyphens, no leading/trailing hyphen, no IP literals.
func validDomainName(name string) bool {
	if len(name) < 4 || len(name) > 253 || net.ParseIP(name) != nil {
		return false
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	// The top-level label is never all digits.
	tld := labels[len(labels)-1]
	return strings.Trim(tld, "0123456789") != ""
}

// lookupDomainTXT resolves the TXT records of name (overridable in tests).
func (s *Server) lookupDomainTXT(ctx context.Context, name string) ([]string, error) {
	if s.lookupTXT != nil {
		return s.lookupTXT(ctx, name)
	}
	return net.DefaultResolver.LookupTXT(ctx, name)
}

// hasDomainVerifyRecord reports whether one of txts is the verification
// record for token (surrounding spaces/quotes and the prefix's case ignored).
func hasDomainVerifyRecord(txts []string, token string) bool {
	if token == "" {
		return false
	}
	for _, t := range txts {
		t = strings.Trim(strings.TrimSpace(t), `"`)
		if len(t) < len(domainVerifyPrefix) || !strings.EqualFold(t[:len(domainVerifyPrefix)], domainVerifyPrefix) {
			continue
		}
		if strings.TrimSpace(t[len(domainVerifyPrefix):]) == token {
			return true
		}
	}
	return false
}

func (s *Server) userI18n(user *models.User) *I18n {
	lang := "en"
	if user != nil && user.Language != "" {
		lang = user.Language
	}
	return s.i18nManager.Get(lang)
}

// HandleDomainsPage renders the domains management page
func (s *Server) HandleDomainsPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	domains, err := s.database.GetDomainsByUserID(user.ID)
	if err != nil {
		log.Printf("Failed to get domains: %v", err)
		domains = []*models.Domain{}
	}
	var verified []*models.Domain
	for _, d := range domains {
		if d.Verified {
			verified = append(verified, d)
		}
	}

	// Get mailboxes with domain info (on pending domains too: they are
	// shown, but take and send no mail until the domain is verified).
	mailboxes, err := s.database.GetAllMailboxesWithDomainByUserID(user.ID)
	if err != nil {
		log.Printf("Failed to get mailboxes: %v", err)
	}

	s.renderTemplate(w, "domains.html", map[string]interface{}{
		"Title":           s.userI18n(user).T("domains.title"),
		"User":            user,
		"Domains":         domains,
		"VerifiedDomains": verified,
		"Mailboxes":       mailboxes,
		"VerifyPrefix":    domainVerifyPrefix,
	})
}

// HandleListDomains returns the user's domains with their verification state.
func (s *Server) HandleListDomains(w http.ResponseWriter, r *http.Request) {
	domains, err := s.database.GetDomainsByUserID(getUserID(r))
	if err != nil {
		log.Printf("Failed to get domains: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to get domains")
		return
	}
	views := make([]DomainView, 0, len(domains))
	for _, d := range domains {
		views = append(views, newDomainView(d))
	}
	respondJSON(w, http.StatusOK, views)
}

// HandleCreateDomain adds a domain. A user's domain starts unverified — it
// does not affect delivery until the TXT record is checked by
// HandleVerifyDomain. One added by an administrator is verified at once.
func (s *Server) HandleCreateDomain(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	user := s.GetUserFromContext(r.Context())
	tr := s.userI18n(user)

	var domainName string

	contentType := r.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") {
		var req struct {
			Domain string `json:"domain"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondHTMXError(w, r, http.StatusBadRequest, "Invalid request")
			return
		}
		domainName = req.Domain
	} else {
		if err := r.ParseForm(); err != nil {
			respondHTMXError(w, r, http.StatusBadRequest, "Invalid form data")
			return
		}
		domainName = r.FormValue("domain")
	}

	// Validate domain
	domainName = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domainName)), ".")
	if domainName == "" {
		respondHTMXError(w, r, http.StatusBadRequest, tr.T("domains.error_required"))
		return
	}
	if !validDomainName(domainName) {
		respondHTMXError(w, r, http.StatusBadRequest, tr.T("domains.error_invalid"))
		return
	}

	domain := &models.Domain{
		Domain:  domainName,
		UserID:  userID,
		Enabled: true,
	}
	if user != nil && user.IsAdmin() {
		domain.Verified = true
		domain.VerifiedMethod = models.DomainVerifiedAdmin
	}

	if err := s.database.CreateDomain(domain); err != nil {
		if errors.Is(err, db.ErrDomainTaken) {
			respondHTMXError(w, r, http.StatusConflict, tr.T("domains.error_exists"))
			return
		}
		log.Printf("Failed to create domain: %v", err)
		// Surface the underlying error: this is a self-hosted admin action,
		// and a bare "failed" forces a trip to journalctl every time.
		respondHTMXError(w, r, http.StatusInternalServerError, "Failed to create domain: "+err.Error())
		return
	}

	log.Printf("Domain %s created by user %d (verified=%v)", domainName, userID, domain.Verified)

	// For HTMX, return empty response (page will reload)
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/domains")
		w.WriteHeader(http.StatusOK)
		return
	}

	respondJSON(w, http.StatusCreated, newDomainView(domain))
}

// HandleVerifyDomain checks the domain's TXT record and, if it carries the
// domain's token, marks the domain verified — from then on it is local.
func (s *Server) HandleVerifyDomain(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	tr := s.userI18n(s.GetUserFromContext(r.Context()))

	domainID, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		respondHTMXError(w, r, http.StatusBadRequest, "Invalid domain ID")
		return
	}
	domain, err := s.database.GetDomainByID(domainID)
	if err != nil || domain.UserID != userID {
		respondHTMXError(w, r, http.StatusNotFound, "Domain not found")
		return
	}

	if !domain.Verified {
		ctx, cancel := context.WithTimeout(r.Context(), domainVerifyTimeout)
		txts, lookupErr := s.lookupDomainTXT(ctx, domain.Domain)
		cancel()
		if !hasDomainVerifyRecord(txts, domain.VerifyToken) {
			rec := domainVerificationRecord(domain)
			log.Printf("Domain %s verification failed for user %d (lookup error: %v)", domain.Domain, userID, lookupErr)
			respondHTMXError(w, r, http.StatusUnprocessableEntity,
				tr.T("domains.error_verify_failed")+" "+rec.Name+" TXT \""+rec.Value+"\"")
			return
		}
		switch err := s.database.VerifyDomain(domain.ID, userID, models.DomainVerifiedDNSTXT); {
		case errors.Is(err, db.ErrDomainTaken):
			respondHTMXError(w, r, http.StatusConflict, tr.T("domains.error_exists"))
			return
		case err != nil:
			log.Printf("Failed to verify domain %s: %v", domain.Domain, err)
			respondHTMXError(w, r, http.StatusInternalServerError, "Failed to verify domain")
			return
		}
		log.Printf("Domain %s verified by DNS TXT for user %d", domain.Domain, userID)
		domain.Verified = true
		domain.VerifiedMethod = models.DomainVerifiedDNSTXT
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/domains")
		w.WriteHeader(http.StatusOK)
		return
	}
	respondJSON(w, http.StatusOK, newDomainView(domain))
}

// HandleToggleDomain enables/disables a domain.
func (s *Server) HandleToggleDomain(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	vars := mux.Vars(r)

	domainID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		respondHTMXError(w, r, http.StatusBadRequest, "Invalid domain ID")
		return
	}

	domain, err := s.database.GetDomainByID(domainID)
	if err != nil || domain.UserID != userID {
		respondHTMXError(w, r, http.StatusNotFound, "Domain not found")
		return
	}

	domain.Enabled = !domain.Enabled
	if err := s.database.UpdateDomain(domain); err != nil {
		respondHTMXError(w, r, http.StatusInternalServerError, "Failed to update domain")
		return
	}

	// The row depends on the verification state and the user's language:
	// reload the page rather than rebuild it here.
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/domains")
		w.WriteHeader(http.StatusOK)
		return
	}
	respondJSON(w, http.StatusOK, newDomainView(domain))
}

// HandleDeleteDomain deletes a domain
func (s *Server) HandleDeleteDomain(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	vars := mux.Vars(r)

	domainID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		respondHTMXError(w, r, http.StatusBadRequest, "Invalid domain ID")
		return
	}

	if err := s.database.DeleteDomain(domainID, userID); err != nil {
		log.Printf("Failed to delete domain: %v", err)
		respondHTMXError(w, r, http.StatusInternalServerError, "Failed to delete domain")
		return
	}

	log.Printf("Domain %d deleted by user %d", domainID, userID)

	// For HTMX, return empty response
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/domains")
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleCreateMailbox creates a new mailbox
func (s *Server) HandleCreateMailbox(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var domainID int64
	var localPart string

	contentType := r.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") {
		var req struct {
			DomainID  int64  `json:"domain_id"`
			LocalPart string `json:"local_part"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondHTMXError(w, r, http.StatusBadRequest, "Invalid request")
			return
		}
		domainID = req.DomainID
		localPart = req.LocalPart
	} else {
		if err := r.ParseForm(); err != nil {
			respondHTMXError(w, r, http.StatusBadRequest, "Invalid form data")
			return
		}
		domainID, _ = strconv.ParseInt(r.FormValue("domain_id"), 10, 64)
		localPart = r.FormValue("local_part")
	}

	// Validate
	localPart = strings.ToLower(strings.TrimSpace(localPart))
	if localPart == "" {
		respondHTMXError(w, r, http.StatusBadRequest, "Local part is required")
		return
	}

	if domainID <= 0 {
		respondHTMXError(w, r, http.StatusBadRequest, "Domain is required")
		return
	}

	// Verify domain belongs to user
	domain, err := s.database.GetDomainByID(domainID)
	if err != nil || domain.UserID != userID {
		respondHTMXError(w, r, http.StatusForbidden, "Domain not found or access denied")
		return
	}
	// A mailbox on a domain nobody proved is ours would be an address the
	// user could send as (DKIM-signed by us) — refuse until it is verified.
	if !domain.Verified {
		respondHTMXError(w, r, http.StatusConflict, s.userI18n(s.GetUserFromContext(r.Context())).T("domains.error_not_verified"))
		return
	}

	mailbox := &models.Mailbox{
		UserID:    userID,
		DomainID:  domainID,
		LocalPart: localPart,
		Enabled:   true,
	}

	if err := s.database.CreateMailbox(mailbox); err != nil {
		log.Printf("Failed to create mailbox: %v", err)
		if strings.Contains(err.Error(), "duplicate") {
			respondHTMXError(w, r, http.StatusConflict, "Mailbox already exists")
		} else {
			// Surface the underlying error: this is a self-hosted admin action,
			// and a bare "failed" forces a trip to journalctl every time.
			respondHTMXError(w, r, http.StatusInternalServerError, "Failed to create mailbox: "+err.Error())
		}
		return
	}

	log.Printf("Mailbox %s@%s created by user %d", localPart, domain.Domain, userID)

	// For HTMX, return empty response (page will reload)
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/domains")
		w.WriteHeader(http.StatusOK)
		return
	}

	respondJSON(w, http.StatusCreated, mailbox)
}

// HandleToggleMailbox enables/disables a mailbox and returns the updated table row
func (s *Server) HandleToggleMailbox(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	vars := mux.Vars(r)

	mailboxID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid mailbox ID", http.StatusBadRequest)
		return
	}

	mailbox, err := s.database.GetMailboxByID(mailboxID)
	if err != nil || mailbox.UserID != userID {
		http.Error(w, "Mailbox not found", http.StatusNotFound)
		return
	}

	mailbox.Enabled = !mailbox.Enabled
	if err := s.database.UpdateMailbox(mailbox); err != nil {
		http.Error(w, "Failed to update mailbox", http.StatusInternalServerError)
		return
	}

	// Need domain name for display — get from GetMailboxesWithDomain
	domainName := ""
	if mboxes, err := s.database.GetAllMailboxesWithDomainByUserID(userID); err == nil {
		for _, m := range mboxes {
			if m.ID == mailboxID {
				domainName = m.DomainName
				break
			}
		}
	}

	statusBadge := `<span class="badge bg-secondary-lt">Disabled</span>`
	toggleLabel := "Enable"
	if mailbox.Enabled {
		statusBadge = `<span class="badge bg-success-lt">Active</span>`
		toggleLabel = "Disable"
	}

	idStr := strconv.FormatInt(mailbox.ID, 10)
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<tr>
		<td><div class="d-flex align-items-center"><i class="ti ti-mail text-muted me-2"></i><span class="fw-medium">` + mailbox.LocalPart + `@` + domainName + `</span></div></td>
		<td>` + statusBadge + `</td>
		<td class="text-muted">` + timeutil.FromMs(mailbox.CreatedAt).Format("2006-01-02") + `</td>
		<td><div class="btn-list flex-nowrap">
			<button hx-post="/api/mailboxes/` + idStr + `/toggle" hx-target="closest tr" hx-swap="outerHTML" class="btn btn-sm btn-ghost-secondary">` + toggleLabel + `</button>
			<button hx-delete="/api/mailboxes/` + idStr + `" hx-confirm="Delete mailbox ` + mailbox.LocalPart + `@` + domainName + `?" hx-target="closest tr" hx-swap="outerHTML swap:0.3s" class="btn btn-sm btn-ghost-danger"><i class="ti ti-trash"></i></button>
		</div></td>
	</tr>`))
}

// HandleDeleteMailbox deletes a mailbox
func (s *Server) HandleDeleteMailbox(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	vars := mux.Vars(r)

	mailboxID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		respondHTMXError(w, r, http.StatusBadRequest, "Invalid mailbox ID")
		return
	}

	if err := s.database.DeleteMailbox(mailboxID, userID); err != nil {
		log.Printf("Failed to delete mailbox: %v", err)
		respondHTMXError(w, r, http.StatusInternalServerError, "Failed to delete mailbox")
		return
	}

	log.Printf("Mailbox %d deleted by user %d", mailboxID, userID)

	// For HTMX, return empty response
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/domains")
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
