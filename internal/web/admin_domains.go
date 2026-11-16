package web

import (
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/gorilla/mux"
)

// Domains and mailboxes are the administrator's (stage 4,
// docs/server-admin-split-plan.md). A mailbox created here gets its own
// account and password (docs/mailbox-accounts-plan.md); the person it is for
// adds it to their DDMail as an ordinary IMAP/SMTP account.

type adminMailboxRow struct {
	ID         int64
	Address    string
	Enabled    bool
	HasAccount bool   // own account and password
	Owner      string // old-style mailboxes: whose mail it is
}

type adminDomainRow struct {
	ID        int64
	Domain    string
	Enabled   bool
	Verified  bool
	Owner     string
	Mailboxes []adminMailboxRow
}

// HandleAdminDomainsPage lists every domain and mailbox of the server.
func (s *Server) HandleAdminDomainsPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	domains, err := s.database.GetAllDomains()
	if err != nil {
		log.Printf("admin domains: %v", err)
	}
	names := map[int64]string{}
	if users, err := s.database.ListUsers(); err == nil {
		for _, u := range users {
			names[u.ID] = u.Username
		}
	}
	var rows []adminDomainRow
	for _, d := range domains {
		row := adminDomainRow{ID: d.ID, Domain: d.Domain, Enabled: d.Enabled, Verified: d.Verified, Owner: names[d.UserID]}
		mbs, err := s.database.GetMailboxesByDomainID(d.ID)
		if err != nil {
			log.Printf("admin domains: mailboxes of %s: %v", d.Domain, err)
		}
		for _, m := range mbs {
			full, _ := s.database.GetMailboxByID(m.ID) // carries principal_id
			mr := adminMailboxRow{ID: m.ID, Address: m.LocalPart + "@" + d.Domain, Enabled: m.Enabled, Owner: names[m.UserID]}
			if full != nil && full.PrincipalID != 0 {
				mr.HasAccount = true
			}
			row.Mailboxes = append(row.Mailboxes, mr)
		}
		rows = append(rows, row)
	}
	host := s.publicEndpoints.Hostname
	s.renderTemplate(w, "admin_domains.html", struct {
		PageData
		Domains  []adminDomainRow
		MailHost string
	}{PageData: PageData{Title: "Domains", User: user}, Domains: rows, MailHost: host})
}

func adminPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil || id <= 0 {
		respondError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

// HandleAdminCreateDomain adds a domain; added by an administrator, it is
// verified at once.
func (s *Server) HandleAdminCreateDomain(w http.ResponseWriter, r *http.Request) {
	admin := s.GetUserFromContext(r.Context())
	name := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
	if !validDomainName(name) {
		respondError(w, http.StatusBadRequest, "invalid domain name")
		return
	}
	d := &models.Domain{Domain: name, UserID: admin.ID, Enabled: true, Verified: true, VerifiedMethod: models.DomainVerifiedAdmin}
	if err := s.database.CreateDomain(d); err != nil {
		if errors.Is(err, db.ErrDomainTaken) {
			respondError(w, http.StatusConflict, "domain already exists")
			return
		}
		log.Printf("admin create domain: %v", err)
		respondError(w, http.StatusInternalServerError, "create failed")
		return
	}
	s.auditAdmin(r, admin, "domain.create", "domain", d.ID, name)
	respondJSON(w, http.StatusCreated, map[string]int64{"id": d.ID})
}

func (s *Server) HandleAdminToggleDomain(w http.ResponseWriter, r *http.Request) {
	id, ok := adminPathID(w, r)
	if !ok {
		return
	}
	d, err := s.database.GetDomainByID(id)
	if err != nil {
		respondError(w, http.StatusNotFound, "domain not found")
		return
	}
	if err := s.database.SetDomainEnabled(id, !d.Enabled); err != nil {
		respondError(w, http.StatusInternalServerError, "update failed")
		return
	}
	s.auditAdmin(r, s.GetUserFromContext(r.Context()), map[bool]string{true: "domain.enable", false: "domain.disable"}[!d.Enabled], "domain", id, d.Domain)
	respondJSON(w, http.StatusOK, map[string]bool{"enabled": !d.Enabled})
}

func (s *Server) HandleAdminDeleteDomain(w http.ResponseWriter, r *http.Request) {
	id, ok := adminPathID(w, r)
	if !ok {
		return
	}
	d, err := s.database.GetDomainByID(id)
	if err != nil {
		respondError(w, http.StatusNotFound, "domain not found")
		return
	}
	accounts, err := s.database.DeleteDomainAndAccounts(id)
	if err != nil {
		log.Printf("admin delete domain: %v", err)
		respondError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	for _, a := range accounts {
		s.revokeSessions(a)
	}
	s.auditAdmin(r, s.GetUserFromContext(r.Context()), "domain.delete", "domain", id, d.Domain)
	respondJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// HandleAdminCreateMailbox creates a mailbox with its own account; the
// generated password is returned once.
func (s *Server) HandleAdminCreateMailbox(w http.ResponseWriter, r *http.Request) {
	admin := s.GetUserFromContext(r.Context())
	domainID, err := strconv.ParseInt(r.FormValue("domain_id"), 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid domain")
		return
	}
	d, err := s.database.GetDomainByID(domainID)
	if err != nil {
		respondError(w, http.StatusNotFound, "domain not found")
		return
	}
	local := strings.ToLower(strings.TrimSpace(r.FormValue("local_part")))
	if local == "" || strings.ContainsAny(local, "@ \t<>\"") || len(local) > 64 {
		respondError(w, http.StatusBadRequest, "invalid mailbox name")
		return
	}
	mb := &models.Mailbox{UserID: d.UserID, DomainID: d.ID, LocalPart: local, Enabled: true}
	if err := s.database.CreateMailbox(mb); err != nil {
		respondError(w, http.StatusConflict, "mailbox already exists")
		return
	}
	address := local + "@" + d.Domain
	password, err := s.giveMailboxAccount(mb.ID, address)
	if err != nil {
		_, _ = s.database.DeleteMailboxAndAccount(mb.ID)
		log.Printf("admin create mailbox account: %v", err)
		respondError(w, http.StatusInternalServerError, "create failed")
		return
	}
	s.auditAdmin(r, admin, "mailbox.create", "mailbox", mb.ID, address)
	respondJSON(w, http.StatusCreated, map[string]string{"address": address, "password": password})
}

func (s *Server) giveMailboxAccount(mailboxID int64, address string) (string, error) {
	password, err := RandomPassword(20)
	if err != nil {
		return "", err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return "", err
	}
	if _, err := s.database.CreateMailboxPrincipal(mailboxID, address, hash); err != nil {
		return "", err
	}
	return password, nil
}

// HandleAdminMailboxPassword sets a new generated password. An old-style
// mailbox (no account yet) gets its own account here — from then on its new
// mail goes to the mailbox, and the former owner connects it as an IMAP
// account (docs/mailbox-accounts-plan.md, step 5).
func (s *Server) HandleAdminMailboxPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := adminPathID(w, r)
	if !ok {
		return
	}
	mb, err := s.database.GetMailboxByID(id)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	d, err := s.database.GetDomainByID(mb.DomainID)
	if err != nil {
		respondError(w, http.StatusNotFound, "domain not found")
		return
	}
	address := mb.LocalPart + "@" + d.Domain
	admin := s.GetUserFromContext(r.Context())
	if mb.PrincipalID == 0 {
		password, err := s.giveMailboxAccount(mb.ID, address)
		if err != nil {
			log.Printf("convert mailbox: %v", err)
			respondError(w, http.StatusInternalServerError, "convert failed")
			return
		}
		s.auditAdmin(r, admin, "mailbox.convert", "mailbox", mb.ID, address)
		respondJSON(w, http.StatusOK, map[string]string{"address": address, "password": password})
		return
	}
	password, err := RandomPassword(20)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "password generation failed")
		return
	}
	hash, err := HashPassword(password)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "password hashing failed")
		return
	}
	if err := s.database.SetMailboxPassword(mb.PrincipalID, hash); err != nil {
		respondError(w, http.StatusInternalServerError, "update failed")
		return
	}
	s.revokeSessions(mb.PrincipalID)
	s.auditAdmin(r, admin, "mailbox.password", "mailbox", mb.ID, address)
	respondJSON(w, http.StatusOK, map[string]string{"address": address, "password": password})
}

func (s *Server) HandleAdminToggleMailbox(w http.ResponseWriter, r *http.Request) {
	id, ok := adminPathID(w, r)
	if !ok {
		return
	}
	mb, err := s.database.GetMailboxByID(id)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	principal, err := s.database.SetMailboxEnabled(id, !mb.Enabled)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "update failed")
		return
	}
	if mb.Enabled && principal != 0 {
		s.revokeSessions(principal)
	}
	s.auditAdmin(r, s.GetUserFromContext(r.Context()), map[bool]string{true: "mailbox.enable", false: "mailbox.disable"}[!mb.Enabled], "mailbox", id, mb.LocalPart)
	respondJSON(w, http.StatusOK, map[string]bool{"enabled": !mb.Enabled})
}

func (s *Server) HandleAdminDeleteMailbox(w http.ResponseWriter, r *http.Request) {
	id, ok := adminPathID(w, r)
	if !ok {
		return
	}
	mb, err := s.database.GetMailboxByID(id)
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	principal, err := s.database.DeleteMailboxAndAccount(id)
	if err != nil {
		log.Printf("admin delete mailbox: %v", err)
		respondError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if principal != 0 {
		s.revokeSessions(principal)
	}
	s.auditAdmin(r, s.GetUserFromContext(r.Context()), "mailbox.delete", "mailbox", id, mb.LocalPart)
	respondJSON(w, http.StatusOK, map[string]bool{"deleted": true})
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

// userI18n: translations in the user's language (English by default).
func (s *Server) userI18n(user *models.User) *I18n {
	lang := "en"
	if user != nil && user.Language != "" {
		lang = user.Language
	}
	return s.i18nManager.Get(lang)
}
