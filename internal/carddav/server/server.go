package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/emersion/go-vcard"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"golang.org/x/crypto/bcrypt"
)

// textMatchRe pulls the search term out of an addressbook-query
// <C:text-match>…</C:text-match> element regardless of namespace prefix.
var textMatchRe = regexp.MustCompile(`(?is)<[a-z0-9]*:?text-match[^>]*>(.*?)</[a-z0-9]*:?text-match>`)

// hrefRe pulls <D:href> targets out of an addressbook-multiget body.
var hrefRe = regexp.MustCompile(`(?is)<[a-z0-9]*:?href[^>]*>(.*?)</[a-z0-9]*:?href>`)

// Server is a CardDAV server
type Server struct {
	database *db.DB
	prefix   string
}

// New creates a new CardDAV server
func New(database *db.DB, prefix string) *Server {
	return &Server{
		database: database,
		prefix:   prefix,
	}
}

// ServeHTTP handles CardDAV requests
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Set DAV header on all responses - required by iOS
	w.Header().Set("DAV", "1, 2, addressbook")

	// Authenticate user
	user, err := s.authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="CardDAV"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Log request
	log.Printf("CardDAV %s %s (user: %s)", r.Method, r.URL.Path, user.Username)

	// Route based on method
	switch r.Method {
	case "OPTIONS":
		s.handleOptions(w, r)
	case "PROPFIND":
		s.handlePropfind(w, r, user)
	case "REPORT":
		s.handleReport(w, r, user)
	case "GET":
		s.handleGet(w, r, user)
	case "PUT":
		s.handlePut(w, r, user)
	case "DELETE":
		s.handleDelete(w, r, user)
	case "MKCOL":
		s.handleMkcol(w, r, user)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// authenticate authenticates the user from Basic Auth
func (s *Server) authenticate(r *http.Request) (*models.User, error) {
	username, password, ok := r.BasicAuth()
	if !ok {
		return nil, fmt.Errorf("no credentials")
	}

	user, err := s.database.GetUserByUsername(username)
	if err != nil {
		return nil, fmt.Errorf("user not found")
	}

	// Check password
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, fmt.Errorf("invalid password")
	}

	return user, nil
}

func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "OPTIONS, GET, PUT, DELETE, PROPFIND, REPORT, MKCOL")
	w.Header().Set("DAV", "1, 2, addressbook")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handlePropfind(w http.ResponseWriter, r *http.Request, user *models.User) {
	path := strings.TrimPrefix(r.URL.Path, s.prefix)
	path = strings.TrimSuffix(path, "/")

	// Parse depth header
	depth := r.Header.Get("Depth")
	if depth == "" {
		depth = "1"
	}

	// Determine what we're querying
	parts := strings.Split(path, "/")

	var response string
	if path == "" || path == fmt.Sprintf("%d", user.ID) {
		// Ensure user has a local address book
		s.ensureLocalAddressBook(user)
		// Principal URL
		response = s.propfindPrincipal(user, depth)
	} else if len(parts) == 2 && parts[1] == "addressbooks" {
		// Address book home set
		response = s.propfindAddressBookHome(user, depth)
	} else if len(parts) == 3 && parts[1] == "addressbooks" {
		// Specific address book
		bookID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			http.Error(w, "Invalid address book ID", http.StatusBadRequest)
			return
		}
		response = s.propfindAddressBook(user, bookID, depth)
	} else {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	w.Write([]byte(response))
}

func (s *Server) propfindPrincipal(user *models.User, depth string) string {
	principalURL := fmt.Sprintf("%s%d/", s.prefix, user.ID)
	addressBookHomeURL := fmt.Sprintf("%s%d/addressbooks/", s.prefix, user.ID)

	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype><D:principal/></D:resourcetype>
        <D:displayname>%s</D:displayname>
        <C:addressbook-home-set><D:href>%s</D:href></C:addressbook-home-set>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`, principalURL, user.Username, addressBookHomeURL)
}

func (s *Server) propfindAddressBookHome(user *models.User, depth string) string {
	homeURL := fmt.Sprintf("%s%d/addressbooks/", s.prefix, user.ID)

	response := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype><D:collection/></D:resourcetype>
        <D:displayname>Address Books</D:displayname>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>`, homeURL)

	// If depth > 0, include address books
	if depth != "0" {
		books, err := s.database.GetAddressBooksByUserID(user.ID)
		if err == nil {
			for _, book := range books {
				bookURL := fmt.Sprintf("%s%d/addressbooks/%d/", s.prefix, user.ID, book.ID)
				response += fmt.Sprintf(`
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype><D:collection/><C:addressbook/></D:resourcetype>
        <D:displayname>%s</D:displayname>
        <C:supported-address-data>
          <C:address-data-type content-type="text/vcard" version="3.0"/>
          <C:address-data-type content-type="text/vcard" version="4.0"/>
        </C:supported-address-data>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>`, bookURL, xmlEscape(book.Name))
			}
		}
	}

	response += `
</D:multistatus>`
	return response
}

func (s *Server) propfindAddressBook(user *models.User, bookID int64, depth string) string {
	book, err := s.database.GetAddressBookByID(bookID)
	if err != nil || book.UserID != user.ID {
		return `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:">
  <D:response>
    <D:href>/</D:href>
    <D:propstat>
      <D:status>HTTP/1.1 404 Not Found</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`
	}

	bookURL := fmt.Sprintf("%s%d/addressbooks/%d/", s.prefix, user.ID, book.ID)

	response := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype><D:collection/><C:addressbook/></D:resourcetype>
        <D:displayname>%s</D:displayname>
        <C:supported-address-data>
          <C:address-data-type content-type="text/vcard" version="3.0"/>
          <C:address-data-type content-type="text/vcard" version="4.0"/>
        </C:supported-address-data>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>`, bookURL, xmlEscape(book.Name))

	// If depth > 0, include contacts
	if depth != "0" {
		contacts, err := s.database.GetContactsByAddressBookID(bookID)
		if err == nil {
			for _, contact := range contacts {
				contactURL := fmt.Sprintf("%s%d/addressbooks/%d/%s.vcf", s.prefix, user.ID, book.ID, contact.UID)
				response += fmt.Sprintf(`
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype/>
        <D:getetag>"%s"</D:getetag>
        <D:getcontenttype>text/vcard; charset=utf-8</D:getcontenttype>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>`, contactURL, contact.ETag)
			}
		}
	}

	response += `
</D:multistatus>`
	return response
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request, user *models.User) {
	path := strings.TrimPrefix(r.URL.Path, s.prefix)
	parts := strings.Split(strings.Trim(path, "/"), "/")

	if len(parts) < 3 || parts[1] != "addressbooks" {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	bookID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.Error(w, "Invalid address book ID", http.StatusBadRequest)
		return
	}

	book, err := s.database.GetAddressBookByID(bookID)
	if err != nil || book.UserID != user.ID {
		http.Error(w, "Address book not found", http.StatusNotFound)
		return
	}

	contacts, err := s.database.GetContactsByAddressBookID(bookID)
	if err != nil {
		http.Error(w, "Failed to get contacts", http.StatusInternalServerError)
		return
	}

	body, _ := io.ReadAll(r.Body)
	report := string(body)

	// Two REPORT flavours land here. addressbook-query is a server-side search
	// (this is what powers a standard client's autocomplete — filter here
	// instead of dumping the whole book). addressbook-multiget asks for a
	// specific set of hrefs. A body without either (or without filters) keeps
	// the old "return everything" behaviour.
	switch {
	case strings.Contains(report, "addressbook-query"):
		if terms := extractLower(textMatchRe, report); len(terms) > 0 {
			contacts = filterContactsByTerms(contacts, terms)
		}
	case strings.Contains(report, "addressbook-multiget"):
		if uids := hrefUIDs(report); len(uids) > 0 {
			contacts = filterContactsByUID(contacts, uids)
		}
	}

	response := `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">`
	for _, contact := range contacts {
		contactURL := fmt.Sprintf("%s%d/addressbooks/%d/%s.vcf", s.prefix, user.ID, book.ID, contact.UID)
		response += fmt.Sprintf(`
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:getetag>"%s"</D:getetag>
        <C:address-data>%s</C:address-data>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>`, contactURL, contact.ETag, xmlEscape(contact.VCardData))
	}
	response += `
</D:multistatus>`

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	w.Write([]byte(response))
}

// extractLower returns the trimmed, lower-cased capture groups of re over s.
func extractLower(re *regexp.Regexp, s string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		v := strings.ToLower(strings.TrimSpace(m[1]))
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// filterContactsByTerms keeps contacts matching ANY term across the common
// autocomplete fields (name, emails, organization).
func filterContactsByTerms(contacts []*models.Contact, terms []string) []*models.Contact {
	var out []*models.Contact
	for _, c := range contacts {
		hay := strings.ToLower(strings.Join([]string{
			c.FullName, c.GivenName, c.FamilyName, c.Nickname,
			c.Email, c.Email2, c.Email3, c.Organization,
		}, " "))
		for _, t := range terms {
			if strings.Contains(hay, t) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// hrefUIDs extracts contact UIDs from the <D:href> targets of a multiget body.
func hrefUIDs(report string) map[string]bool {
	uids := make(map[string]bool)
	for _, m := range hrefRe.FindAllStringSubmatch(report, -1) {
		href := strings.TrimSpace(m[1])
		if href == "" {
			continue
		}
		seg := href[strings.LastIndex(href, "/")+1:]
		uids[strings.TrimSuffix(seg, ".vcf")] = true
	}
	return uids
}

func filterContactsByUID(contacts []*models.Contact, uids map[string]bool) []*models.Contact {
	var out []*models.Contact
	for _, c := range contacts {
		if uids[c.UID] {
			out = append(out, c)
		}
	}
	return out
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, user *models.User) {
	path := strings.TrimPrefix(r.URL.Path, s.prefix)
	parts := strings.Split(strings.Trim(path, "/"), "/")

	if len(parts) < 4 || parts[1] != "addressbooks" {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	bookID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.Error(w, "Invalid address book ID", http.StatusBadRequest)
		return
	}

	book, err := s.database.GetAddressBookByID(bookID)
	if err != nil || book.UserID != user.ID {
		http.Error(w, "Address book not found", http.StatusNotFound)
		return
	}

	// Get contact UID from path (remove .vcf extension)
	uid := strings.TrimSuffix(parts[3], ".vcf")

	contact, err := s.database.GetContactByUID(bookID, uid)
	if err != nil || contact == nil {
		http.Error(w, "Contact not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/vcard; charset=utf-8")
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, contact.ETag))
	w.Write([]byte(contact.VCardData))
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, user *models.User) {
	path := strings.TrimPrefix(r.URL.Path, s.prefix)
	parts := strings.Split(strings.Trim(path, "/"), "/")

	if len(parts) < 4 || parts[1] != "addressbooks" {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	bookID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.Error(w, "Invalid address book ID", http.StatusBadRequest)
		return
	}

	book, err := s.database.GetAddressBookByID(bookID)
	if err != nil || book.UserID != user.ID {
		http.Error(w, "Address book not found", http.StatusNotFound)
		return
	}

	// Parse vCard
	decoder := vcard.NewDecoder(r.Body)
	card, err := decoder.Decode()
	if err != nil {
		http.Error(w, "Invalid vCard", http.StatusBadRequest)
		return
	}

	// Get UID from path or vCard
	uid := strings.TrimSuffix(parts[3], ".vcf")
	if cardUID := card.Get(vcard.FieldUID); cardUID != nil && cardUID.Value != "" {
		uid = cardUID.Value
	}

	// Serialize vCard
	var vcardBuilder strings.Builder
	if err := vcard.NewEncoder(&vcardBuilder).Encode(card); err != nil {
		http.Error(w, "Failed to encode vCard", http.StatusInternalServerError)
		return
	}
	vcardData := vcardBuilder.String()

	// Generate ETag
	etag := generateETag(vcardData)

	// Check if contact exists
	existing, err := s.database.GetContactByUID(bookID, uid)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	contact := &models.Contact{
		UserID:        user.ID,
		AddressBookID: bookID,
		UID:           uid,
		VCardData:     vcardData,
		ETag:          etag,
		LocalModified: true,
	}

	// Parse vCard fields
	parseVCardFields(card, contact)

	if existing == nil {
		// Create new contact
		if err := s.database.CreateContact(contact); err != nil {
			http.Error(w, "Failed to create contact", http.StatusInternalServerError)
			return
		}
		s.queueContactSync(book, contact.ID, uid, "", contact.VCardData, "create")
		w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
		w.WriteHeader(http.StatusCreated)
	} else {
		// Update existing contact
		contact.ID = existing.ID
		if err := s.database.UpdateContact(contact); err != nil {
			http.Error(w, "Failed to update contact", http.StatusInternalServerError)
			return
		}
		s.queueContactSync(book, existing.ID, uid, existing.RemoteID, contact.VCardData, "update")
		w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, user *models.User) {
	path := strings.TrimPrefix(r.URL.Path, s.prefix)
	parts := strings.Split(strings.Trim(path, "/"), "/")

	if len(parts) < 4 || parts[1] != "addressbooks" {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	bookID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.Error(w, "Invalid address book ID", http.StatusBadRequest)
		return
	}

	book, err := s.database.GetAddressBookByID(bookID)
	if err != nil || book.UserID != user.ID {
		http.Error(w, "Address book not found", http.StatusNotFound)
		return
	}

	uid := strings.TrimSuffix(parts[3], ".vcf")

	// Get contact before deletion to preserve RemoteID for sync
	existingContact, err := s.database.GetContactByUID(bookID, uid)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	// Queue sync BEFORE delete (FK constraint would fail after delete)
	if existingContact != nil {
		s.queueContactSync(book, existingContact.ID, uid, existingContact.RemoteID, "", "delete")
	}

	if err := s.database.DeleteContactByUID(bookID, uid); err != nil {
		http.Error(w, "Failed to delete contact", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMkcol(w http.ResponseWriter, r *http.Request, user *models.User) {
	// Create new address book
	path := strings.TrimPrefix(r.URL.Path, s.prefix)
	parts := strings.Split(strings.Trim(path, "/"), "/")

	if len(parts) < 3 || parts[1] != "addressbooks" {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	name := parts[2]
	if name == "" {
		name = "New Address Book"
	}

	// Get or create a local source for the user
	sources, err := s.database.GetContactSourcesByUserID(user.ID)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	var localSource *models.ContactSource
	for _, src := range sources {
		if src.SourceType == "local" {
			localSource = src
			break
		}
	}

	if localSource == nil {
		// Create local source
		localSource = &models.ContactSource{
			UserID:       user.ID,
			Name:         "Local Contacts",
			SourceType:   "local",
			AuthType:     "password",
			SyncEnabled:  false,
			SyncInterval: 0,
		}
		if err := s.database.CreateContactSource(localSource); err != nil {
			http.Error(w, "Failed to create source", http.StatusInternalServerError)
			return
		}
	}

	book := &models.AddressBook{
		UserID:   user.ID,
		SourceID: localSource.ID,
		Name:     name,
		CanWrite: true,
	}

	if err := s.database.CreateAddressBook(book); err != nil {
		http.Error(w, "Failed to create address book", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
}

// queueContactSync queues a contact change for push to remote CardDAV server
func (s *Server) queueContactSync(book *models.AddressBook, contactID int64, uid, remoteID, vcardData, operation string) {
	// Only queue for enabled external CardDAV sources with reverse sync enabled
	if book.SourceType != "carddav" {
		return
	}
	if !book.Enabled {
		return
	}
	if !book.ReverseSync {
		return
	}
	if err := s.database.QueueContactSync(contactID, book.ID, book.SourceID, uid, remoteID, vcardData, operation); err != nil {
		log.Printf("Failed to queue contact sync: %v", err)
	}
}

// ensureLocalAddressBook creates a local address book for the user if one doesn't exist
func (s *Server) ensureLocalAddressBook(user *models.User) {
	// Check if user already has a local contact source
	sources, err := s.database.GetContactSourcesByUserID(user.ID)
	if err != nil {
		return
	}
	for _, src := range sources {
		if src.SourceType == "local" {
			return // Already has local source
		}
	}

	// Create local source
	source := &models.ContactSource{
		UserID:     user.ID,
		Name:       "Local",
		SourceType: "local",
	}
	if err := s.database.CreateContactSource(source); err != nil {
		log.Printf("Failed to create local contact source: %v", err)
		return
	}

	// Create address book
	book := &models.AddressBook{
		UserID:   user.ID,
		SourceID: source.ID,
		Name:     "Contacts",
		CanWrite: true,
	}
	if err := s.database.CreateAddressBook(book); err != nil {
		log.Printf("Failed to create local address book: %v", err)
	}
}

// Helper functions

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

func generateETag(data string) string {
	return fmt.Sprintf("%x", len(data))
}

func parseVCardFields(card vcard.Card, contact *models.Contact) {
	// Parse name
	if n := card.Get(vcard.FieldFormattedName); n != nil {
		contact.FullName = n.Value
	}
	if n := card.Get(vcard.FieldName); n != nil {
		parts := strings.Split(n.Value, ";")
		if len(parts) > 0 {
			contact.FamilyName = parts[0]
		}
		if len(parts) > 1 {
			contact.GivenName = parts[1]
		}
	}
	if contact.FullName == "" && (contact.GivenName != "" || contact.FamilyName != "") {
		contact.FullName = strings.TrimSpace(contact.GivenName + " " + contact.FamilyName)
	}

	// Parse nickname
	if nn := card.Get(vcard.FieldNickname); nn != nil {
		contact.Nickname = nn.Value
	}

	// Parse emails
	emails := card.Values(vcard.FieldEmail)
	if len(emails) > 0 {
		contact.Email = emails[0]
	}
	if len(emails) > 1 {
		contact.Email2 = emails[1]
	}
	if len(emails) > 2 {
		contact.Email3 = emails[2]
	}

	// Parse phones
	phones := card.Values(vcard.FieldTelephone)
	if len(phones) > 0 {
		contact.Phone = phones[0]
	}
	if len(phones) > 1 {
		contact.Phone2 = phones[1]
	}
	if len(phones) > 2 {
		contact.Phone3 = phones[2]
	}

	// Parse organization
	if org := card.Get(vcard.FieldOrganization); org != nil {
		contact.Organization = org.Value
	}
	if title := card.Get(vcard.FieldTitle); title != nil {
		contact.Title = title.Value
	}

	// Parse note
	if note := card.Get(vcard.FieldNote); note != nil {
		contact.Notes = note.Value
	}
}
