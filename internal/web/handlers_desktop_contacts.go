package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/yourusername/mailserver/internal/models"
)

// DesktopContact is the address-book row served to the native client. The
// client sees one unified address book (aggregated across all sources on the
// server); source/book membership is deliberately not exposed.
type DesktopContact struct {
	ID           int64    `json:"id"`
	FullName     string   `json:"full_name"`
	Emails       []string `json:"emails,omitempty"`
	Phones       []string `json:"phones,omitempty"`
	Organization string   `json:"organization,omitempty"`
	Title        string   `json:"title,omitempty"`
	PhotoURL     string   `json:"photo_url,omitempty"`
}

func toDesktopContact(c *models.Contact) DesktopContact {
	emails := make([]string, 0, 3)
	for _, e := range []string{c.Email, c.Email2, c.Email3} {
		if strings.TrimSpace(e) != "" {
			emails = append(emails, e)
		}
	}
	phones := make([]string, 0, 3)
	for _, p := range []string{c.Phone, c.Phone2, c.Phone3} {
		if strings.TrimSpace(p) != "" {
			phones = append(phones, p)
		}
	}
	name := c.FullName
	if name == "" {
		name = strings.TrimSpace(c.GivenName + " " + c.FamilyName)
	}
	return DesktopContact{
		ID:           c.ID,
		FullName:     name,
		Emails:       emails,
		Phones:       phones,
		Organization: c.Organization,
		Title:        c.Title,
		PhotoURL:     c.PhotoURL,
	}
}

// clampLimit parses a limit query param, applying a default and a hard cap.
func clampLimit(raw string, def, max int) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// HandleDesktopContacts returns the user's full unified address book.
// GET /api/desktop/v1/contacts?limit=N
func (s *Server) HandleDesktopContacts(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	limit := clampLimit(r.URL.Query().Get("limit"), 500, 5000)
	contacts, err := s.database.GetAllUserContacts(user.ID, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load contacts")
		return
	}
	out := make([]DesktopContact, 0, len(contacts))
	for _, c := range contacts {
		out = append(out, toDesktopContact(c))
	}
	respondJSON(w, http.StatusOK, out)
}

// HandleDesktopContactSearch does the autocomplete lookup across all sources.
// GET /api/desktop/v1/contacts/search?q=term&limit=N
func (s *Server) HandleDesktopContactSearch(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		respondJSON(w, http.StatusOK, []DesktopContact{})
		return
	}
	limit := clampLimit(r.URL.Query().Get("limit"), 25, 100)
	contacts, err := s.database.SearchContacts(user.ID, query, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "search failed")
		return
	}
	out := make([]DesktopContact, 0, len(contacts))
	for _, c := range contacts {
		out = append(out, toDesktopContact(c))
	}
	respondJSON(w, http.StatusOK, out)
}
