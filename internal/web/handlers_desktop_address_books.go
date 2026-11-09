package web

import (
	"net/http"
)

// DesktopAddressBook is one address book as the native clients see it: enough
// to let the user pick where a new contact goes. The unified address book
// (/contacts) deliberately hides membership; this list exists only so that a
// client never has to guess the target book (a guessed "default" one may be a
// work book on a corporate server).
type DesktopAddressBook struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	SourceName    string `json:"source_name,omitempty"`
	SourceType    string `json:"source_type"`
	IdentityEmail string `json:"identity_email,omitempty"`
	CanWrite      bool   `json:"can_write"`
}

// HandleDesktopAddressBooks lists the user's enabled address books.
// GET /api/desktop/v1/address-books
func (s *Server) HandleDesktopAddressBooks(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	books, err := s.database.GetEnabledAddressBooksByUserID(user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load address books")
		return
	}
	sources, err := s.database.GetContactSourcesByUserID(user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load contact sources")
		return
	}
	type src struct{ name, identity string }
	bySource := make(map[int64]src, len(sources))
	for _, cs := range sources {
		bySource[cs.ID] = src{cs.Name, cs.IdentityEmail}
	}
	out := make([]DesktopAddressBook, 0, len(books))
	for _, b := range books {
		sv := bySource[b.SourceID]
		out = append(out, DesktopAddressBook{
			ID:            b.ID,
			Name:          b.Name,
			SourceName:    sv.name,
			SourceType:    b.SourceType,
			IdentityEmail: sv.identity,
			CanWrite:      b.CanWrite,
		})
	}
	respondJSON(w, http.StatusOK, out)
}
