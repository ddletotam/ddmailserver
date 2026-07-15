package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/models"
)

// contactWriteRequest is the body for creating/updating a contact. All fields
// optional on PATCH (present fields replace). `from_identity` (create only)
// routes the new contact to that identity's default-write address book.
type contactWriteRequest struct {
	FullName      string   `json:"full_name"`
	Emails        []string `json:"emails"`
	Phones        []string `json:"phones"`
	Organization  string   `json:"organization"`
	Title         string   `json:"title"`
	FromIdentity  string   `json:"from_identity"`
	AddressBookID int64    `json:"address_book_id"`
}

// applyTo writes the request fields onto a Contact (emails/phones fan out into
// the three fixed slots the model exposes).
func (req *contactWriteRequest) applyTo(c *models.Contact) {
	c.FullName = req.FullName
	c.Organization = req.Organization
	c.Title = req.Title
	emails := padTo3(req.Emails)
	c.Email, c.Email2, c.Email3 = emails[0], emails[1], emails[2]
	phones := padTo3(req.Phones)
	c.Phone, c.Phone2, c.Phone3 = phones[0], phones[1], phones[2]
}

func padTo3(in []string) [3]string {
	var out [3]string
	for i := 0; i < 3 && i < len(in); i++ {
		out[i] = strings.TrimSpace(in[i])
	}
	return out
}

// buildContactVCard renders a vCard 3.0 from the contact's parsed fields.
func buildContactVCard(c *models.Contact) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCARD\r\nVERSION:3.0\r\n")
	fmt.Fprintf(&b, "UID:%s\r\n", c.UID)
	if c.FullName != "" {
		fmt.Fprintf(&b, "FN:%s\r\n", vcardEscape(c.FullName))
	}
	fmt.Fprintf(&b, "N:%s;%s;;;\r\n", vcardEscape(c.FamilyName), vcardEscape(c.GivenName))
	for _, e := range []string{c.Email, c.Email2, c.Email3} {
		if e != "" {
			fmt.Fprintf(&b, "EMAIL:%s\r\n", vcardEscape(e))
		}
	}
	for _, p := range []string{c.Phone, c.Phone2, c.Phone3} {
		if p != "" {
			fmt.Fprintf(&b, "TEL:%s\r\n", vcardEscape(p))
		}
	}
	if c.Organization != "" {
		fmt.Fprintf(&b, "ORG:%s\r\n", vcardEscape(c.Organization))
	}
	if c.Title != "" {
		fmt.Fprintf(&b, "TITLE:%s\r\n", vcardEscape(c.Title))
	}
	b.WriteString("END:VCARD\r\n")
	return b.String()
}

func vcardEscape(s string) string {
	r := strings.NewReplacer("\\", "\\\\", ",", "\\,", ";", "\\;", "\n", "\\n")
	return r.Replace(s)
}

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

// HandleDesktopContactCreate adds a contact to a writable address book (the
// identity's default one when not given explicitly) and queues CardDAV
// reverse-sync. POST /api/desktop/v1/contacts
func (s *Server) HandleDesktopContactCreate(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req contactWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	bookID := req.AddressBookID
	if bookID == 0 {
		identity := strings.TrimSpace(req.FromIdentity)
		if identity == "" {
			identity, _ = s.database.DefaultIdentityEmail(user.ID)
		}
		id, err := s.database.DefaultWriteAddressBookID(user.ID, identity)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to resolve address book")
			return
		}
		if id == 0 {
			respondError(w, http.StatusForbidden, "identity has no writable address book")
			return
		}
		bookID = id
	}
	book, err := s.database.GetAddressBookByID(bookID)
	if err != nil || book == nil || book.UserID != user.ID {
		respondError(w, http.StatusNotFound, "address book not found")
		return
	}
	if !book.CanWrite {
		respondError(w, http.StatusForbidden, "address book is read-only")
		return
	}

	c := &models.Contact{
		UserID:        user.ID,
		AddressBookID: bookID,
		UID:           generateUID(),
		LocalModified: true,
	}
	req.applyTo(c)
	c.VCardData = buildContactVCard(c)
	if err := s.database.CreateContact(c); err != nil {
		respondError(w, http.StatusInternalServerError, "create failed")
		return
	}
	_ = s.database.QueueContactSync(c.ID, bookID, book.SourceID, c.UID, "", c.VCardData, "create")

	respondJSON(w, http.StatusCreated, map[string]any{"id": c.ID, "uid": c.UID})
}

// HandleDesktopContactPatch replaces a contact's fields. PATCH /contacts/{id}
func (s *Server) HandleDesktopContactPatch(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req contactWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	c, err := s.database.GetContactByID(id)
	if err != nil || c == nil || c.UserID != user.ID {
		respondError(w, http.StatusNotFound, "contact not found")
		return
	}
	book, err := s.database.GetAddressBookByID(c.AddressBookID)
	if err != nil || book == nil || !book.CanWrite {
		respondError(w, http.StatusForbidden, "address book is read-only")
		return
	}
	req.applyTo(c)
	c.VCardData = buildContactVCard(c)
	c.LocalModified = true
	if err := s.database.UpdateContact(c); err != nil {
		respondError(w, http.StatusInternalServerError, "update failed")
		return
	}
	_ = s.database.QueueContactSync(c.ID, c.AddressBookID, book.SourceID, c.UID, c.RemoteID, c.VCardData, "update")
	respondJSON(w, http.StatusOK, map[string]any{"id": c.ID})
}

// HandleDesktopContactDelete removes a contact. DELETE /contacts/{id}
func (s *Server) HandleDesktopContactDelete(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid id")
		return
	}
	c, err := s.database.GetContactByID(id)
	if err != nil || c == nil || c.UserID != user.ID {
		respondError(w, http.StatusNotFound, "contact not found")
		return
	}
	book, err := s.database.GetAddressBookByID(c.AddressBookID)
	if err != nil || book == nil {
		respondError(w, http.StatusNotFound, "address book not found")
		return
	}
	if err := s.database.DeleteContact(id); err != nil {
		respondError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	_ = s.database.QueueContactSync(c.ID, c.AddressBookID, book.SourceID, c.UID, c.RemoteID, "", "delete")
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
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
