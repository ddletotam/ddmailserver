package web

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	carddavclient "github.com/yourusername/mailserver/internal/carddav/client"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/oauth"
)

// ContactsData holds data for the contacts page
type ContactsData struct {
	PageData
	Host                  string
	GoogleOAuthEnabled    bool
	MicrosoftOAuthEnabled bool
}

// ContactSourcesListData holds data for the contact sources list
type ContactSourcesListData struct {
	PageData
	Sources []*models.ContactSource
}

// AddressBookWithMeta is an address book with additional display info
type AddressBookWithMeta struct {
	*models.AddressBook
	ContactCount int
	SourceName   string
}

// AddressBooksListData holds data for the address books list
type AddressBooksListData struct {
	PageData
	AddressBooks []AddressBookWithMeta
}

// ContactsListData holds data for the contacts list
type ContactsListData struct {
	PageData
	Contacts []*models.Contact
	Query    string
}

// HandleContactsPage renders the contacts management page
func (s *Server) HandleContactsPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// Get host for CardDAV URL display
	host := r.Host
	if fwdHost := r.Header.Get("X-Forwarded-Host"); fwdHost != "" {
		host = fwdHost
	}

	data := ContactsData{
		PageData: PageData{
			Title: "Contacts",
			User:  user,
		},
		Host:                  host,
		GoogleOAuthEnabled:    s.googleOAuth != nil,
		MicrosoftOAuthEnabled: s.microsoftOAuth != nil,
	}

	s.renderTemplate(w, "contacts.html", data)
}

// HandleContactSourcesList returns the list of contact sources (HTMX partial)
func (s *Server) HandleContactSourcesList(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	sources, err := s.database.GetContactSourcesByUserID(user.ID)
	if err != nil {
		log.Printf("Failed to get contact sources: %v", err)
		http.Error(w, "Failed to load sources", http.StatusInternalServerError)
		return
	}

	data := ContactSourcesListData{
		PageData: PageData{User: user},
		Sources:  sources,
	}

	s.renderTemplatePartial(w, "contacts.html", "contact-sources-list", data)
}

// HandleAddressBooksList returns the list of address books (HTMX partial)
func (s *Server) HandleAddressBooksList(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	books, err := s.database.GetAddressBooksByUserID(user.ID)
	if err != nil {
		log.Printf("Failed to get address books: %v", err)
		http.Error(w, "Failed to load address books", http.StatusInternalServerError)
		return
	}

	// Add contact counts and source names
	var booksWithMeta []AddressBookWithMeta
	for _, book := range books {
		meta := AddressBookWithMeta{AddressBook: book}

		// Get contact count
		count, err := s.database.GetContactCountForAddressBook(book.ID)
		if err == nil {
			meta.ContactCount = count
		}

		// Get source name
		if book.SourceID > 0 {
			source, err := s.database.GetContactSourceByID(book.SourceID)
			if err == nil && source != nil {
				meta.SourceName = source.Name
			}
		}

		booksWithMeta = append(booksWithMeta, meta)
	}

	data := AddressBooksListData{
		PageData:     PageData{User: user},
		AddressBooks: booksWithMeta,
	}

	s.renderTemplatePartial(w, "contacts.html", "address-books-list", data)
}

// HandleContactsList returns the list of contacts for search (HTMX partial)
func (s *Server) HandleContactsList(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	query := r.URL.Query().Get("q")

	var contacts []*models.Contact
	var err error

	if query != "" {
		contacts, err = s.database.SearchContacts(user.ID, query, 100)
	} else {
		// Get all contacts (limited)
		contacts, err = s.database.GetAllUserContacts(user.ID, 100)
	}

	if err != nil {
		log.Printf("Failed to get contacts: %v", err)
		http.Error(w, "Failed to load contacts", http.StatusInternalServerError)
		return
	}

	data := ContactsListData{
		PageData: PageData{User: user},
		Contacts: contacts,
		Query:    query,
	}

	s.renderTemplatePartial(w, "contacts.html", "contacts-list", data)
}

// HandleCreateContactSourceWeb creates a new CardDAV source (web form handler)
func (s *Server) HandleCreateContactSourceWeb(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	source := &models.ContactSource{
		UserID:          user.ID,
		Name:            r.FormValue("name"),
		SourceType:      "carddav",
		CardDAVURL:      r.FormValue("carddav_url"),
		CardDAVUsername: r.FormValue("carddav_username"),
		CardDAVPassword: r.FormValue("carddav_password"),
		AuthType:        "password",
		SyncEnabled:     true,
		SyncInterval:    300, // 5 minutes
	}

	if source.Name == "" || source.CardDAVURL == "" {
		http.Error(w, "Name and URL are required", http.StatusBadRequest)
		return
	}

	if err := s.database.CreateContactSource(source); err != nil {
		log.Printf("Failed to create contact source: %v", err)
		http.Error(w, "Failed to create source", http.StatusInternalServerError)
		return
	}

	log.Printf("Created contact source %s for user %d", source.Name, user.ID)

	// Return updated list
	s.HandleContactSourcesList(w, r)
}

// HandleCreateLocalAddressBook creates a new local address book
func (s *Server) HandleCreateLocalAddressBook(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	name := r.FormValue("name")

	if name == "" {
		http.Error(w, "Name is required", http.StatusBadRequest)
		return
	}

	// Create a local source for this address book
	source := &models.ContactSource{
		UserID:      user.ID,
		Name:        name,
		SourceType:  "local",
		SyncEnabled: false,
	}

	if err := s.database.CreateContactSource(source); err != nil {
		log.Printf("Failed to create local source: %v", err)
		http.Error(w, "Failed to create address book", http.StatusInternalServerError)
		return
	}

	// Create the address book
	book := &models.AddressBook{
		SourceID: source.ID,
		UserID:   user.ID,
		Name:     name,
		CanWrite: true,
	}

	if err := s.database.CreateAddressBook(book); err != nil {
		log.Printf("Failed to create address book: %v", err)
		http.Error(w, "Failed to create address book", http.StatusInternalServerError)
		return
	}

	log.Printf("Created local address book %s for user %d", name, user.ID)

	// Return updated list
	s.HandleAddressBooksList(w, r)
}

// HandleSyncContactSource triggers sync for a contact source
func (s *Server) HandleSyncContactSource(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	source, err := s.database.GetContactSourceByID(id)
	if err != nil || source == nil || source.UserID != user.ID {
		http.Error(w, "Source not found", http.StatusNotFound)
		return
	}

	// Check source type
	if source.SourceType == "local" {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<span class="badge bg-secondary">Local address book</span>`)
		return
	}

	// Execute sync synchronously for immediate feedback
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	syncErr := s.syncContactSource(ctx, source)

	w.Header().Set("Content-Type", "text/html")
	if syncErr != nil {
		log.Printf("Contact sync failed for %s: %v", source.Name, syncErr)
		fmt.Fprintf(w, `<span class="badge bg-danger" title="%s">Sync failed</span>`,
			template.HTMLEscapeString(syncErr.Error()))
	} else {
		fmt.Fprintf(w, `<span class="badge bg-success">Synced</span>`)
	}
}

// refreshContactOAuthTokensIfNeeded refreshes OAuth tokens if they are expired or about to expire
func (s *Server) refreshContactOAuthTokensIfNeeded(source *models.ContactSource) error {
	// Only refresh for OAuth sources
	if source.AuthType != "oauth2_google" && source.AuthType != "oauth2_microsoft" {
		return nil
	}

	// Check if token is expired or expires within 5 minutes
	if source.OAuthTokenExpiry.IsZero() || time.Until(source.OAuthTokenExpiry) > 5*time.Minute {
		return nil // Token still valid
	}

	// Need refresh token
	if source.OAuthRefreshToken == "" {
		return fmt.Errorf("no refresh token available, please re-authenticate")
	}

	log.Printf("Refreshing OAuth token for contact source %s (expires: %v)", source.Name, source.OAuthTokenExpiry)

	var tokenResp *oauth.TokenResponse
	var err error

	if source.AuthType == "oauth2_google" {
		if s.googleOAuth == nil {
			return fmt.Errorf("Google OAuth not configured")
		}
		tokenResp, err = s.googleOAuth.RefreshToken(source.OAuthRefreshToken)
	} else if source.AuthType == "oauth2_microsoft" {
		if s.microsoftOAuth == nil {
			return fmt.Errorf("Microsoft OAuth not configured")
		}
		tokenResp, err = s.microsoftOAuth.RefreshToken(source.OAuthRefreshToken)
	}

	if err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}

	// Calculate new expiry
	expiry := oauth.TokenExpiry(tokenResp.ExpiresIn)

	// Update tokens in database
	newRefreshToken := tokenResp.RefreshToken
	if newRefreshToken == "" {
		newRefreshToken = source.OAuthRefreshToken // Keep existing if not returned
	}

	if err := s.database.UpdateContactSourceOAuthTokens(source.ID, tokenResp.AccessToken, newRefreshToken, expiry); err != nil {
		return fmt.Errorf("failed to save new tokens: %w", err)
	}

	// Update source in memory for immediate use
	source.OAuthAccessToken = tokenResp.AccessToken
	source.OAuthRefreshToken = newRefreshToken
	source.OAuthTokenExpiry = expiry

	log.Printf("OAuth token refreshed for contact source %s, new expiry: %v", source.Name, expiry)
	return nil
}

// syncContactSource performs synchronization for a contact source
func (s *Server) syncContactSource(ctx context.Context, source *models.ContactSource) error {
	log.Printf("Manual sync started for contact source %s (ID: %d, type: %s)", source.Name, source.ID, source.SourceType)

	// Refresh OAuth tokens if needed
	if err := s.refreshContactOAuthTokensIfNeeded(source); err != nil {
		s.database.UpdateContactSourceLastError(source.ID, err.Error())
		return fmt.Errorf("token refresh failed: %w", err)
	}

	// Create CardDAV client
	client := carddavclient.New(source, s.database)

	// Connect to CardDAV server
	if err := client.Connect(); err != nil {
		s.database.UpdateContactSourceLastError(source.ID, err.Error())
		return fmt.Errorf("failed to connect: %w", err)
	}

	// Discover address books
	addressBooks, err := client.DiscoverAddressBooks(ctx)
	if err != nil {
		s.database.UpdateContactSourceLastError(source.ID, err.Error())
		return fmt.Errorf("failed to discover address books: %w", err)
	}

	log.Printf("Discovered %d address books for %s", len(addressBooks), source.Name)

	var syncErrors []string

	// Sync each address book
	for _, remoteBook := range addressBooks {
		existingBook, err := s.database.GetAddressBookByRemoteID(source.ID, remoteBook.RemoteID)
		if err != nil {
			syncErrors = append(syncErrors, fmt.Sprintf("check %s: %v", remoteBook.Name, err))
			continue
		}

		var book *models.AddressBook
		if existingBook == nil {
			remoteBook.SourceID = source.ID
			remoteBook.UserID = source.UserID
			if err := s.database.CreateAddressBook(remoteBook); err != nil {
				syncErrors = append(syncErrors, fmt.Sprintf("create %s: %v", remoteBook.Name, err))
				continue
			}
			book = remoteBook
			log.Printf("Created new address book: %s", book.Name)
		} else {
			book = existingBook
		}

		if err := client.SyncAddressBook(ctx, book); err != nil {
			syncErrors = append(syncErrors, fmt.Sprintf("sync %s: %v", book.Name, err))
			continue
		}

		log.Printf("Synced address book: %s", book.Name)
	}

	// Update last sync time
	if err := s.database.UpdateContactSourceLastSync(source.ID, time.Now()); err != nil {
		log.Printf("Failed to update last sync time: %v", err)
	}

	if len(syncErrors) > 0 {
		errMsg := strings.Join(syncErrors, "; ")
		s.database.UpdateContactSourceLastError(source.ID, errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	log.Printf("Manual sync completed for contact source %s", source.Name)
	return nil
}

// HandleDeleteContactSourceWeb deletes a contact source (web UI)
func (s *Server) HandleDeleteContactSourceWeb(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	source, err := s.database.GetContactSourceByID(id)
	if err != nil || source == nil || source.UserID != user.ID {
		http.Error(w, "Source not found", http.StatusNotFound)
		return
	}

	if err := s.database.DeleteContactSource(id); err != nil {
		log.Printf("Failed to delete contact source: %v", err)
		http.Error(w, "Failed to delete source", http.StatusInternalServerError)
		return
	}

	log.Printf("Deleted contact source %d for user %d", id, user.ID)
	w.WriteHeader(http.StatusOK)
}

// HandleDeleteAddressBookWeb deletes an address book (web UI)
func (s *Server) HandleDeleteAddressBookWeb(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	book, err := s.database.GetAddressBookByID(id)
	if err != nil || book == nil || book.UserID != user.ID {
		http.Error(w, "Address book not found", http.StatusNotFound)
		return
	}

	if err := s.database.DeleteAddressBook(id); err != nil {
		log.Printf("Failed to delete address book: %v", err)
		http.Error(w, "Failed to delete address book", http.StatusInternalServerError)
		return
	}

	log.Printf("Deleted address book %d for user %d", id, user.ID)
	w.WriteHeader(http.StatusOK)
}
