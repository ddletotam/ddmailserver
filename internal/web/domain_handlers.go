package web

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/models"
)

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

	// Get mailboxes with domain info
	mailboxes, err := s.database.GetMailboxesWithDomainByUserID(user.ID)
	if err != nil {
		log.Printf("Failed to get mailboxes: %v", err)
	}

	// Get user's language for title translation
	userLang := user.Language
	if userLang == "" {
		userLang = "en"
	}
	i18n := s.i18nManager.Get(userLang)

	s.renderTemplate(w, "domains.html", map[string]interface{}{
		"Title":     i18n.T("domains.title"),
		"User":      user,
		"Domains":   domains,
		"Mailboxes": mailboxes,
	})
}

// HandleCreateDomain creates a new domain
func (s *Server) HandleCreateDomain(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

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
	domainName = strings.ToLower(strings.TrimSpace(domainName))
	if domainName == "" {
		respondHTMXError(w, r, http.StatusBadRequest, "Domain name is required")
		return
	}

	// Check valid domain format (basic check)
	if !strings.Contains(domainName, ".") || len(domainName) < 4 {
		respondHTMXError(w, r, http.StatusBadRequest, "Invalid domain format")
		return
	}

	domain := &models.Domain{
		Domain:  domainName,
		UserID:  userID,
		Enabled: true,
	}

	if err := s.database.CreateDomain(domain); err != nil {
		log.Printf("Failed to create domain: %v", err)
		if strings.Contains(err.Error(), "duplicate") {
			respondHTMXError(w, r, http.StatusConflict, "Domain already exists")
		} else {
			respondHTMXError(w, r, http.StatusInternalServerError, "Failed to create domain")
		}
		return
	}

	log.Printf("Domain %s created by user %d", domainName, userID)

	// For HTMX, return empty response (page will reload)
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/domains")
		w.WriteHeader(http.StatusOK)
		return
	}

	respondJSON(w, http.StatusCreated, domain)
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
			respondHTMXError(w, r, http.StatusInternalServerError, "Failed to create mailbox")
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
