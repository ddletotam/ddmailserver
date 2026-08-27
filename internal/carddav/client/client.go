package client

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/carddav"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/tlsverify"
)

// Client is a CardDAV client for syncing with external address books
type Client struct {
	source     *models.ContactSource
	database   *db.DB
	client     *carddav.Client
	httpClient webdav.HTTPClient
}

// New creates a new CardDAV client
func New(source *models.ContactSource, database *db.DB) *Client {
	return &Client{
		source:   source,
		database: database,
	}
}

// Connect establishes a connection to the CardDAV server
func (c *Client) Connect() error {
	var client *carddav.Client
	var err error

	var httpClient *http.Client
	if c.source.AuthType == "password" {
		// tlsverify.Transport(): a CardDAV host that omits its intermediate
		// stays reachable, without giving up verification to get there.
		httpClient = &http.Client{
			Transport: tlsverify.Transport(),
			Timeout:   30 * time.Second,
		}
		authClient := webdav.HTTPClientWithBasicAuth(httpClient, c.source.CardDAVUsername, c.source.CardDAVPassword)
		client, err = carddav.NewClient(authClient, c.source.CardDAVURL)
		c.httpClient = authClient
	} else if c.source.AuthType == "oauth2_google" || c.source.AuthType == "oauth2_microsoft" {
		// Use OAuth2 bearer token
		transport := &oauthTransport{
			base:  tlsverify.Transport(),
			token: c.source.OAuthAccessToken,
		}
		httpClient = &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		}
		client, err = carddav.NewClient(httpClient, c.source.CardDAVURL)
		c.httpClient = httpClient
	} else {
		return fmt.Errorf("unsupported auth type: %s", c.source.AuthType)
	}

	if err != nil {
		return fmt.Errorf("failed to create CardDAV client: %w", err)
	}

	c.client = client
	return nil
}

// oauthTransport adds OAuth bearer token to requests
type oauthTransport struct {
	base  http.RoundTripper
	token string
}

func (t *oauthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(req)
}

// DiscoverAddressBooks discovers available address books from the server
func (c *Client) DiscoverAddressBooks(ctx context.Context) ([]*models.AddressBook, error) {
	if c.client == nil {
		return nil, fmt.Errorf("client not connected")
	}

	// Google uses People API - CardDAV has limited support
	if c.source.AuthType == "oauth2_google" {
		return c.discoverGoogleAddressBooks(ctx)
	}

	// Try standard CardDAV discovery
	principal, err := c.client.FindCurrentUserPrincipal(ctx)
	if err != nil {
		log.Printf("CardDAV discovery failed for %s: %v - using URL as direct address book", c.source.Name, err)
		return c.useDirectAddressBookURL(ctx)
	}

	// Find address book home set
	homeSet, err := c.client.FindAddressBookHomeSet(ctx, principal)
	if err != nil {
		log.Printf("Failed to find address book home set: %v - using URL as direct address book", err)
		return c.useDirectAddressBookURL(ctx)
	}

	// Find all address books
	addressBooks, err := c.client.FindAddressBooks(ctx, homeSet)
	if err != nil {
		return nil, fmt.Errorf("failed to find address books: %w", err)
	}

	var result []*models.AddressBook
	for _, ab := range addressBooks {
		book := &models.AddressBook{
			SourceID:    c.source.ID,
			UserID:      c.source.UserID,
			RemoteID:    ab.Path,
			Name:        ab.Name,
			Description: ab.Description,
			CanWrite:    true,
		}
		result = append(result, book)
	}

	return result, nil
}

// useDirectAddressBookURL creates an address book entry from a direct URL
func (c *Client) useDirectAddressBookURL(ctx context.Context) ([]*models.AddressBook, error) {
	log.Printf("Using direct address book URL: %s", c.source.CardDAVURL)

	book := &models.AddressBook{
		SourceID:    c.source.ID,
		UserID:      c.source.UserID,
		RemoteID:    c.source.CardDAVURL,
		Name:        c.source.Name,
		Description: "Direct CardDAV Address Book",
		CanWrite:    true,
	}

	return []*models.AddressBook{book}, nil
}

// discoverGoogleAddressBooks handles Google Contacts (People API, not CardDAV)
func (c *Client) discoverGoogleAddressBooks(ctx context.Context) ([]*models.AddressBook, error) {
	// Google uses People API, not CardDAV
	// Create a single address book entry
	book := &models.AddressBook{
		SourceID:    c.source.ID,
		UserID:      c.source.UserID,
		RemoteID:    "google-contacts",
		Name:        c.source.Name,
		Description: "Google Contacts",
		CanWrite:    true,
	}

	return []*models.AddressBook{book}, nil
}

// discoverMicrosoftAddressBooks handles Microsoft Contacts (Graph API, not CardDAV)
func (c *Client) discoverMicrosoftAddressBooks(ctx context.Context) ([]*models.AddressBook, error) {
	// Microsoft uses Graph API, not CardDAV
	// Create a single address book entry
	book := &models.AddressBook{
		SourceID:    c.source.ID,
		UserID:      c.source.UserID,
		RemoteID:    "microsoft-contacts",
		Name:        c.source.Name,
		Description: "Microsoft Contacts",
		CanWrite:    true,
	}

	return []*models.AddressBook{book}, nil
}

// SyncAddressBook synchronizes an address book from the server
func (c *Client) SyncAddressBook(ctx context.Context, book *models.AddressBook) error {
	if c.client == nil {
		return fmt.Errorf("client not connected")
	}

	// Google uses People API
	if c.source.AuthType == "oauth2_google" {
		return c.syncGoogleContacts(ctx, book)
	}

	log.Printf("Syncing address book %s (%s)", book.Name, book.RemoteID)

	// Get path for queries
	addressBookPath := book.RemoteID
	if strings.HasPrefix(book.RemoteID, "https://") || strings.HasPrefix(book.RemoteID, "http://") {
		addressBookPath = ""
	}

	// Query all contacts from the server
	// Try addressbook-query first, fallback to multiget if server doesn't support it (Yandex returns 400)
	query := &carddav.AddressBookQuery{
		DataRequest: carddav.AddressDataRequest{
			AllProp: true,
		},
	}

	objects, err := c.client.QueryAddressBook(ctx, addressBookPath, query)
	if err != nil {
		log.Printf("CardDAV addressbook-query failed for %s, trying multiget fallback: %v", book.Name, err)
		objects, err = c.syncViaMultiGet(ctx, addressBookPath)
		if err != nil {
			return fmt.Errorf("failed to query address book: %w", err)
		}
	}

	log.Printf("CardDAV query returned %d contacts for address book %s", len(objects), book.Name)

	// Get existing contacts from database
	existingContacts, err := c.database.GetContactsByAddressBookID(book.ID)
	if err != nil {
		return fmt.Errorf("failed to get existing contacts: %w", err)
	}

	// Build map of existing contacts by UID
	existingByUID := make(map[string]*models.Contact)
	for _, contact := range existingContacts {
		existingByUID[contact.UID] = contact
	}

	// Process remote contacts
	changes := &db.SyncContactChanges{
		Creates:    []*models.Contact{},
		Updates:    []*models.Contact{},
		DeleteUIDs: []string{},
	}

	seenUIDs := make(map[string]bool)
	dupUIDs := 0

	for _, obj := range objects {
		if obj.Card == nil {
			continue
		}

		// Parse vCard
		contact, err := c.parseVCard(obj.Card, book)
		if err != nil {
			log.Printf("Failed to parse vCard: %v", err)
			continue
		}

		contact.RemoteID = obj.Path
		contact.ETag = obj.ETag

		// Дедуп ВНУТРИ пачки. UID у карточки берётся с сервера, и он не всегда
		// уникален: у GAL-коллекций SOGo это адрес (`UID:1cteam@small.kz`), а
		// одна и та же запись попадает в выдачу дважды. Сверка ниже идёт только
		// с базой, поэтому обе копии уходили в Creates, второй INSERT ронял
		// уникальный индекс (address_book_id, uid) и с ним всю транзакцию —
		// книга оставалась пустой на каждом синке, бесконечно. Первая копия
		// побеждает: это одна и та же запись, выбирать между копиями нечего.
		if seenUIDs[contact.UID] {
			dupUIDs++
			continue
		}
		seenUIDs[contact.UID] = true

		// Check if contact exists
		existing, exists := existingByUID[contact.UID]
		if !exists {
			// New contact
			changes.Creates = append(changes.Creates, contact)
		} else if existing.LocalModified {
			// Skip - will be pushed by reverse sync
			continue
		} else if existing.ETag != obj.ETag {
			// Updated contact
			contact.ID = existing.ID
			changes.Updates = append(changes.Updates, contact)
		}
	}

	if dupUIDs > 0 {
		log.Printf("CardDAV %s: %d objects shared a UID with an earlier one and were skipped (source reuses UIDs)",
			book.Name, dupUIDs)
	}

	// Find deleted contacts
	for uid := range existingByUID {
		if !seenUIDs[uid] {
			changes.DeleteUIDs = append(changes.DeleteUIDs, uid)
		}
	}

	// Apply changes
	if len(changes.Creates) > 0 || len(changes.Updates) > 0 || len(changes.DeleteUIDs) > 0 {
		log.Printf("Applying changes: %d creates, %d updates, %d deletes",
			len(changes.Creates), len(changes.Updates), len(changes.DeleteUIDs))

		if err := c.database.ApplyContactSyncChanges(book.ID, changes); err != nil {
			return fmt.Errorf("failed to apply sync changes: %w", err)
		}
	}

	log.Printf("Synced address book: %s", book.Name)
	return nil
}

// parseMultistatusHrefs извлекает из `207 Multi-Status` пути ресурсов
// коллекции: по одному href на каждый `<response>`, без самой коллекции.
//
// Раньше здесь был поиск подстроки `<href` по всему телу ответа — и он брал
// hrefs откуда угодно, в том числе изнутри свойств (`owner`, `principal-URL`,
// `addressbook-home-set` и прочее, чего в allprop-ответе много). На GAL SOGo
// это давало 7628 «путей» вместо 1231 карточки: часть — мусор, который потом
// отваливался, а часть — та же карточка, запрошенная в multiget несколько раз,
// из-за чего сервер и присылал её несколько раз. Отсюда и брались дубликаты,
// ронявшие вставку. Берём только href, который является ПРЯМЫМ потомком
// `<response>` (encoding/xml сопоставляет только непосредственных детей, так
// что вложенные в свойства hrefs сюда не попадают), и дедуплицируем.
func parseMultistatusHrefs(body []byte, collectionPath string) ([]string, error) {
	var ms struct {
		Responses []struct {
			Href string `xml:"DAV: href"`
		} `xml:"DAV: response"`
	}
	if err := xml.Unmarshal(body, &ms); err != nil {
		return nil, err
	}

	collection := strings.TrimSuffix(collectionPath, "/")
	seen := make(map[string]bool, len(ms.Responses))
	var paths []string
	for _, r := range ms.Responses {
		href := strings.TrimSpace(r.Href)
		if href == "" || strings.HasSuffix(href, "/") {
			// Коллекции (в том числе сама запрашиваемая) всегда со слешем.
			continue
		}
		if collection != "" && href == collection {
			continue
		}
		if seen[href] {
			continue
		}
		seen[href] = true
		paths = append(paths, href)
	}
	return paths, nil
}

// syncViaMultiGet fetches contacts using PROPFIND + addressbook-multiget as fallback
func (c *Client) syncViaMultiGet(ctx context.Context, addressBookPath string) ([]carddav.AddressObject, error) {
	// First PROPFIND to get list of contact paths and ETags
	multiGet := &carddav.AddressBookMultiGet{
		DataRequest: carddav.AddressDataRequest{
			AllProp: true,
		},
	}

	// Build full URL for PROPFIND
	propfindURL := c.buildFullURL(addressBookPath)
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", propfindURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create PROPFIND request: %w", err)
	}
	req.Header.Set("Depth", "1")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("PROPFIND failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 207 {
		return nil, fmt.Errorf("PROPFIND returned %d", resp.StatusCode)
	}

	paths, err := parseMultistatusHrefs(body, addressBookPath)
	if err != nil {
		return nil, fmt.Errorf("failed to parse PROPFIND response: %w", err)
	}

	if len(paths) == 0 {
		log.Printf("No contact paths found in PROPFIND response for %s", addressBookPath)
		return nil, nil
	}

	log.Printf("Found %d contact paths via PROPFIND, fetching via multiget", len(paths))

	// Now use addressbook-multiget to fetch all contacts with data
	multiGet.Paths = paths
	objects, err := c.client.MultiGetAddressBook(ctx, addressBookPath, multiGet)
	if err != nil {
		// If multiget also fails, fetch contacts one by one via GET
		log.Printf("MultiGet failed, fetching contacts individually: %v", err)
		return c.fetchContactsIndividually(ctx, paths)
	}

	return objects, nil
}

// fetchContactsIndividually fetches each contact via GET as last resort fallback
func (c *Client) fetchContactsIndividually(ctx context.Context, paths []string) ([]carddav.AddressObject, error) {
	var objects []carddav.AddressObject

	for _, path := range paths {
		req, err := http.NewRequestWithContext(ctx, "GET", c.buildFullURL(path), nil)
		if err != nil {
			continue
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			log.Printf("GET %s failed: %v", path, err)
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			continue
		}

		// Parse vCard
		decoder := vcard.NewDecoder(strings.NewReader(string(body)))
		card, err := decoder.Decode()
		if err != nil {
			log.Printf("Failed to parse vCard from %s: %v", path, err)
			continue
		}

		objects = append(objects, carddav.AddressObject{
			Path: path,
			ETag: resp.Header.Get("ETag"),
			Card: card,
		})
	}

	log.Printf("Fetched %d contacts individually", len(objects))
	return objects, nil
}

// parseVCard parses a vCard into a Contact model
// buildFullURL constructs a full URL from a path, using the source's CardDAV URL as base
func (c *Client) buildFullURL(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	// Extract base URL (scheme + host) from source URL
	u, err := url.Parse(c.source.CardDAVURL)
	if err != nil {
		return c.source.CardDAVURL + path
	}
	if strings.HasPrefix(path, "/") {
		return u.Scheme + "://" + u.Host + path
	}
	return c.source.CardDAVURL + path
}

func (c *Client) parseVCard(card vcard.Card, book *models.AddressBook) (*models.Contact, error) {
	contact := &models.Contact{
		UserID:        c.source.UserID,
		AddressBookID: book.ID,
	}

	// Get UID
	if uid := card.Get(vcard.FieldUID); uid != nil {
		contact.UID = uid.Value
	}
	if contact.UID == "" {
		// Generate UID if not present
		contact.UID = fmt.Sprintf("%d-%d", time.Now().UnixNano(), book.ID)
	}

	// Store raw vCard
	var vcardBuilder strings.Builder
	if err := vcard.NewEncoder(&vcardBuilder).Encode(card); err == nil {
		contact.VCardData = vcardBuilder.String()
	}

	// Parse name
	if n := card.Get(vcard.FieldFormattedName); n != nil {
		contact.FullName = n.Value
	}
	if n := card.Get(vcard.FieldName); n != nil {
		// N field format: family;given;additional;prefix;suffix
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

	// Parse address
	if adr := card.Get(vcard.FieldAddress); adr != nil {
		contact.Address = adr.Value
	}

	// Parse note
	if note := card.Get(vcard.FieldNote); note != nil {
		contact.Notes = note.Value
	}

	// Parse photo — either a URL or inline base64. Inline values get stored as
	// a `data:` URL so the avatar fetcher can decode them without touching the
	// network. vCard parameters: TYPE=PNG / ENCODING=BASE64 hint at the MIME.
	if photo := card.Get(vcard.FieldPhoto); photo != nil {
		val := photo.Value
		switch {
		case strings.HasPrefix(val, "data:"):
			contact.PhotoURL = val
		case strings.HasPrefix(val, "http"):
			contact.PhotoURL = val
		case val != "":
			// Inline base64 (vCard 3.0) — wrap into a data URL.
			mime := "image/jpeg"
			if t := photo.Params.Get("TYPE"); t != "" {
				mime = "image/" + strings.ToLower(t)
			}
			contact.PhotoURL = "data:" + mime + ";base64," + val
		}
	}

	return contact, nil
}

// PutContactRaw uploads raw vCard data to a remote CardDAV path
func (c *Client) PutContactRaw(ctx context.Context, remotePath string, vcardData string) error {
	if c.httpClient == nil {
		return fmt.Errorf("client not connected")
	}

	req, err := http.NewRequestWithContext(ctx, "PUT", c.buildFullURL(remotePath), strings.NewReader(vcardData))
	if err != nil {
		return fmt.Errorf("failed to create PUT request: %w", err)
	}
	req.Header.Set("Content-Type", "text/vcard; charset=utf-8")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("PUT request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PUT failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// DeleteContact deletes a contact from a remote CardDAV server
func (c *Client) DeleteContact(ctx context.Context, remotePath string) error {
	if c.httpClient == nil {
		return fmt.Errorf("client not connected")
	}

	req, err := http.NewRequestWithContext(ctx, "DELETE", c.buildFullURL(remotePath), nil)
	if err != nil {
		return fmt.Errorf("failed to create DELETE request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 && resp.StatusCode != 404 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("DELETE failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// syncGoogleContacts syncs contacts from Google People API
func (c *Client) syncGoogleContacts(ctx context.Context, book *models.AddressBook) error {
	log.Printf("Syncing Google Contacts for %s", c.source.Name)

	// Google People API - use listDirectoryPeople for all contacts, or connections for personal
	apiURL := "https://people.googleapis.com/v1/people/me/connections?personFields=names,emailAddresses,phoneNumbers,organizations,addresses,biographies,photos,nicknames&pageSize=1000&sortOrder=LAST_MODIFIED_DESCENDING"

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.source.OAuthAccessToken)

	client := tlsverify.HTTPClient(30 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch Google contacts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Google API error: %s: %s", resp.Status, string(body)[:min(200, len(body))])
	}

	var result struct {
		Connections []googlePerson `json:"connections"`
		TotalItems  int            `json:"totalItems"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode Google response: %w", err)
	}

	log.Printf("Google People API returned %d contacts", len(result.Connections))

	// Build contacts from Google response
	existingContacts, err := c.database.GetContactsByAddressBookID(book.ID)
	if err != nil {
		return fmt.Errorf("failed to get existing contacts: %w", err)
	}
	existingByUID := make(map[string]*models.Contact)
	for _, contact := range existingContacts {
		existingByUID[contact.UID] = contact
	}

	changes := &db.SyncContactChanges{}
	seenUIDs := make(map[string]bool)
	dupUIDs := 0

	for _, person := range result.Connections {
		contact := person.toContact(c.source.UserID, book.ID)
		if contact.UID == "" || contact.FullName == "" {
			continue
		}
		// Тот же дедуп внутри пачки, что и в CardDAV-пути: два элемента с одним
		// UID дали бы два INSERT'а и уронили транзакцию целиком.
		if seenUIDs[contact.UID] {
			dupUIDs++
			continue
		}
		seenUIDs[contact.UID] = true

		if existing, ok := existingByUID[contact.UID]; ok {
			if existing.LocalModified {
				continue
			}
			if existing.ETag != contact.ETag {
				existing.FullName = contact.FullName
				existing.GivenName = contact.GivenName
				existing.FamilyName = contact.FamilyName
				existing.Email = contact.Email
				existing.Email2 = contact.Email2
				existing.Phone = contact.Phone
				existing.Phone2 = contact.Phone2
				existing.Organization = contact.Organization
				existing.Title = contact.Title
				existing.Notes = contact.Notes
				existing.PhotoURL = contact.PhotoURL
				existing.VCardData = contact.VCardData
				existing.ETag = contact.ETag
				changes.Updates = append(changes.Updates, existing)
			}
		} else {
			changes.Creates = append(changes.Creates, contact)
		}
	}

	if dupUIDs > 0 {
		log.Printf("Google Contacts %s: %d entries shared a UID with an earlier one and were skipped",
			book.Name, dupUIDs)
	}

	// Detect deletes
	for uid := range existingByUID {
		if !seenUIDs[uid] {
			changes.DeleteUIDs = append(changes.DeleteUIDs, uid)
		}
	}

	if len(changes.Creates) > 0 || len(changes.Updates) > 0 || len(changes.DeleteUIDs) > 0 {
		log.Printf("Google Contacts: %d creates, %d updates, %d deletes",
			len(changes.Creates), len(changes.Updates), len(changes.DeleteUIDs))
		if err := c.database.ApplyContactSyncChanges(book.ID, changes); err != nil {
			return fmt.Errorf("failed to apply changes: %w", err)
		}
	}

	log.Printf("Google Contacts sync completed for %s (%d contacts)", c.source.Name, len(result.Connections))
	return nil
}

type googlePerson struct {
	ResourceName string `json:"resourceName"` // "people/c123456"
	Etag         string `json:"etag"`
	Names        []struct {
		DisplayName string `json:"displayName"`
		GivenName   string `json:"givenName"`
		FamilyName  string `json:"familyName"`
	} `json:"names"`
	EmailAddresses []struct {
		Value string `json:"value"`
	} `json:"emailAddresses"`
	PhoneNumbers []struct {
		Value string `json:"value"`
	} `json:"phoneNumbers"`
	Organizations []struct {
		Name  string `json:"name"`
		Title string `json:"title"`
	} `json:"organizations"`
	Biographies []struct {
		Value string `json:"value"`
	} `json:"biographies"`
	Photos []struct {
		URL string `json:"url"`
	} `json:"photos"`
	Nicknames []struct {
		Value string `json:"value"`
	} `json:"nicknames"`
}

func (p *googlePerson) toContact(userID, addressBookID int64) *models.Contact {
	c := &models.Contact{
		UserID:        userID,
		AddressBookID: addressBookID,
		UID:           p.ResourceName, // "people/c123456"
		RemoteID:      p.ResourceName,
		ETag:          p.Etag,
	}
	if len(p.Names) > 0 {
		c.FullName = p.Names[0].DisplayName
		c.GivenName = p.Names[0].GivenName
		c.FamilyName = p.Names[0].FamilyName
	}
	if len(p.EmailAddresses) > 0 {
		c.Email = p.EmailAddresses[0].Value
	}
	if len(p.EmailAddresses) > 1 {
		c.Email2 = p.EmailAddresses[1].Value
	}
	if len(p.PhoneNumbers) > 0 {
		c.Phone = p.PhoneNumbers[0].Value
	}
	if len(p.PhoneNumbers) > 1 {
		c.Phone2 = p.PhoneNumbers[1].Value
	}
	if len(p.Organizations) > 0 {
		c.Organization = p.Organizations[0].Name
		c.Title = p.Organizations[0].Title
	}
	if len(p.Biographies) > 0 {
		c.Notes = p.Biographies[0].Value
	}
	if len(p.Photos) > 0 {
		c.PhotoURL = p.Photos[0].URL
	}
	if len(p.Nicknames) > 0 {
		c.Nickname = p.Nicknames[0].Value
	}

	// Generate vCard
	c.VCardData = fmt.Sprintf("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:%s\r\nFN:%s\r\nN:%s;%s;;;\r\n",
		c.UID, c.FullName, c.FamilyName, c.GivenName)
	if c.Email != "" {
		c.VCardData += fmt.Sprintf("EMAIL:%s\r\n", c.Email)
	}
	if c.Email2 != "" {
		c.VCardData += fmt.Sprintf("EMAIL:%s\r\n", c.Email2)
	}
	if c.Phone != "" {
		c.VCardData += fmt.Sprintf("TEL:%s\r\n", c.Phone)
	}
	if c.Organization != "" {
		c.VCardData += fmt.Sprintf("ORG:%s\r\n", c.Organization)
	}
	if c.Title != "" {
		c.VCardData += fmt.Sprintf("TITLE:%s\r\n", c.Title)
	}
	c.VCardData += "END:VCARD\r\n"

	return c
}

// syncMicrosoftContacts syncs contacts from Microsoft Graph API
func (c *Client) syncMicrosoftContacts(ctx context.Context, book *models.AddressBook) error {
	log.Printf("Syncing Microsoft Contacts for %s", c.source.Name)

	// Microsoft Graph API endpoint
	url := "https://graph.microsoft.com/v1.0/me/contacts?$top=1000"

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.source.OAuthAccessToken)

	client := tlsverify.HTTPClient(30 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch Microsoft contacts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Microsoft Graph API error: %s", resp.Status)
	}

	// Parse response and create contacts
	// This is a simplified implementation - full implementation would parse the JSON response
	log.Printf("Microsoft Contacts sync completed for %s", c.source.Name)
	return nil
}

// PushChanges pushes locally modified contacts to the server
func (c *Client) PushChanges(ctx context.Context, book *models.AddressBook) error {
	if c.client == nil {
		return fmt.Errorf("client not connected")
	}

	// Get locally modified contacts
	contacts, err := c.database.GetLocallyModifiedContacts(book.ID)
	if err != nil {
		return fmt.Errorf("failed to get modified contacts: %w", err)
	}

	if len(contacts) == 0 {
		return nil
	}

	log.Printf("Pushing %d modified contacts to %s", len(contacts), book.Name)

	for _, contact := range contacts {
		// Parse vCard data
		reader := strings.NewReader(contact.VCardData)
		decoder := vcard.NewDecoder(reader)
		card, err := decoder.Decode()
		if err != nil {
			log.Printf("Failed to decode vCard for contact %d: %v", contact.ID, err)
			continue
		}

		// Determine path for PUT
		path := contact.RemoteID
		if path == "" {
			path = fmt.Sprintf("%s/%s.vcf", book.RemoteID, contact.UID)
		}

		// PUT the vCard
		newPath, err := c.client.PutAddressObject(ctx, path, card)
		if err != nil {
			log.Printf("Failed to push contact %s: %v", contact.UID, err)
			continue
		}

		// Mark as synced
		if err := c.database.MarkContactSynced(contact.ID, ""); err != nil {
			log.Printf("Failed to mark contact synced: %v", err)
		}

		log.Printf("Pushed contact %s to %v", contact.UID, newPath)
	}

	return nil
}
