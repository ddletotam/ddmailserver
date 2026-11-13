package web

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/carddav/vcardedit"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/gorilla/mux"
)

// contactStore — то, что обработчикам контактов нужно от базы. *db.DB его
// реализует; тесты подставляют хранилище в памяти.
type contactStore interface {
	GetAllUserContacts(userID int64, limit int) ([]*models.Contact, error)
	SearchContacts(userID int64, query string, limit int) ([]*models.Contact, error)
	GetAddressBooksByUserID(userID int64) ([]*models.AddressBook, error)
	GetAddressBookByID(id int64) (*models.AddressBook, error)
	GetContactByID(id int64) (*models.Contact, error)
	CreateContact(contact *models.Contact) error
	UpdateContact(contact *models.Contact) error
	DeleteContact(id int64) error
	QueueContactSync(contactID, addressBookID, sourceID int64, uid, remoteID, vcardData, operation string) error
	QueueContactPatch(contactID, addressBookID, sourceID int64, uid, remoteID, vcardData, patch string) error
	DefaultIdentityEmail(userID int64) (string, error)
	DefaultWriteAddressBookID(userID int64, identityEmail string) (int64, error)
}

// contactAPI — обработчики /contacts. Отдельный тип, а не методы Server, чтобы
// тесты гоняли их на httptest без PostgreSQL (как taskAPI).
type contactAPI struct {
	store contactStore
	// t — текст для человека на языке пользователя.
	t   func(user *models.User, key string) string
	now func() time.Time
}

func (s *Server) contactAPI() contactAPI {
	return contactAPI{store: s.database, t: s.userText, now: time.Now}
}

// userText — строка i18n на языке пользователя (его настройка, иначе язык
// сервера по умолчанию).
func (s *Server) userText(user *models.User, key string) string {
	if s.i18nManager == nil {
		return key
	}
	locale := ""
	if user != nil {
		locale = user.Language
	}
	if locale == "" && s.i18n != nil {
		locale = s.i18n.GetLocale()
	}
	return s.i18nManager.Get(locale).T(key)
}

// contactWriteRequest — тело POST и PATCH. На PATCH поле, которого нет в
// запросе, не трогается; присланное значение, совпадающее с карточкой, — тоже
// (клиенты шлют форму целиком). `from_identity` и `address_book_id` — только
// для создания: книгу нового контакта выбирает клиент.
type contactWriteRequest struct {
	FullName      *string   `json:"full_name"`
	Emails        *[]string `json:"emails"`
	Phones        *[]string `json:"phones"`
	Organization  *string   `json:"organization"`
	Title         *string   `json:"title"`
	FromIdentity  string    `json:"from_identity"`
	AddressBookID int64     `json:"address_book_id"`
}

func (req *contactWriteRequest) patch() vcardedit.Patch {
	return vcardedit.Patch{
		FullName:     req.FullName,
		Emails:       req.Emails,
		Phones:       req.Phones,
		Organization: req.Organization,
		Title:        req.Title,
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

func derefList(l *[]string) []string {
	if l == nil {
		return nil
	}
	return *l
}

// buildContactVCard собирает vCard 3.0 нового контакта. Только для создания:
// существующую карточку правит vcardedit по строкам.
func buildContactVCard(uid, fullName string, emails, phones []string, org, title string) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCARD\r\nVERSION:3.0\r\n")
	b.WriteString("UID:" + uid + "\r\n")
	b.WriteString("END:VCARD\r\n")
	// Поля — той же правкой, что и PATCH: одно экранирование и одна
	// свёртка строк на оба пути.
	p := vcardedit.Patch{Emails: &emails, Phones: &phones, Organization: &org, Title: &title}
	if fullName != "" {
		p.FullName = &fullName
	}
	res, err := vcardedit.Apply(b.String(), p, time.Time{})
	if err != nil {
		return b.String() // не бывает: карточка своя
	}
	data := res.Data
	if !strings.Contains(data, "\r\nN:") {
		// N обязателен в vCard 3.0; без имени — пустой.
		data = strings.Replace(data, "VERSION:3.0\r\n", "VERSION:3.0\r\nN:;;;;\r\n", 1)
	}
	return data
}

// DesktopContact is the address-book row served to the native clients: the
// unified address book (all sources together), each row saying which book it
// lives in and whether that book takes edits.
type DesktopContact struct {
	ID            int64    `json:"id"`
	FullName      string   `json:"full_name"`
	Emails        []string `json:"emails,omitempty"`
	Phones        []string `json:"phones,omitempty"`
	Organization  string   `json:"organization,omitempty"`
	Title         string   `json:"title,omitempty"`
	PhotoURL      string   `json:"photo_url,omitempty"`
	AddressBookID int64    `json:"address_book_id"`
	// CanWrite — книга контакта принимает правку и удаление. false — PATCH и
	// DELETE ответят 403; клиент не показывает «Изменить» и «Удалить».
	CanWrite bool `json:"can_write"`
}

func toDesktopContact(c *models.Contact, canWrite bool) DesktopContact {
	name := c.FullName
	if name == "" {
		name = strings.TrimSpace(c.GivenName + " " + c.FamilyName)
	}
	return DesktopContact{
		ID:            c.ID,
		FullName:      name,
		Emails:        c.ListEmails(),
		Phones:        c.ListPhones(),
		Organization:  c.Organization,
		Title:         c.Title,
		PhotoURL:      c.PhotoURL,
		AddressBookID: c.AddressBookID,
		CanWrite:      canWrite,
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

// respondReadOnly — 403 на запись в книгу только для чтения: машинный код и
// текст для человека на языке пользователя.
func (a contactAPI) respondReadOnly(w http.ResponseWriter, user *models.User, key string) {
	code := "address_book_read_only"
	errText := "address book is read-only"
	if key == "contacts.error.no_writable_book" {
		code = "no_writable_address_book"
		errText = "identity has no writable address book"
	}
	respondJSON(w, http.StatusForbidden, map[string]string{
		"error":   errText,
		"code":    code,
		"message": a.t(user, key),
	})
}

// writableBooks — книга → можно ли в неё писать, для всех книг пользователя.
func (a contactAPI) writableBooks(userID int64) (map[int64]bool, error) {
	books, err := a.store.GetAddressBooksByUserID(userID)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(books))
	for _, b := range books {
		out[b.ID] = b.Writable()
	}
	return out, nil
}

func (a contactAPI) respondContacts(w http.ResponseWriter, userID int64, contacts []*models.Contact) {
	writable, err := a.writableBooks(userID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load address books")
		return
	}
	out := make([]DesktopContact, 0, len(contacts))
	for _, c := range contacts {
		out = append(out, toDesktopContact(c, writable[c.AddressBookID]))
	}
	respondJSON(w, http.StatusOK, out)
}

// HandleDesktopContacts returns the user's full unified address book.
// GET /api/desktop/v1/contacts?limit=N
func (s *Server) HandleDesktopContacts(w http.ResponseWriter, r *http.Request) {
	s.contactAPI().list(w, r)
}

func (a contactAPI) list(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	limit := clampLimit(r.URL.Query().Get("limit"), 500, 5000)
	contacts, err := a.store.GetAllUserContacts(user.ID, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load contacts")
		return
	}
	a.respondContacts(w, user.ID, contacts)
}

// HandleDesktopContactSearch does the autocomplete lookup across all sources.
// GET /api/desktop/v1/contacts/search?q=term&limit=N
func (s *Server) HandleDesktopContactSearch(w http.ResponseWriter, r *http.Request) {
	s.contactAPI().search(w, r)
}

func (a contactAPI) search(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
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
	contacts, err := a.store.SearchContacts(user.ID, query, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "search failed")
		return
	}
	a.respondContacts(w, user.ID, contacts)
}

// reverseSynced — правка книги уходит наверх воркером обратной синхронизации
// (тот же отбор, что у нашего CardDAV-сервера, queueContactSync): только
// включённые CardDAV-книги с обратной синхронизацией. Локальные книги наш
// CardDAV-сервер отдаёт клиентам сам.
func reverseSynced(book *models.AddressBook) bool {
	return book.SourceType == "carddav" && book.Enabled && book.ReverseSync
}

// HandleDesktopContactCreate adds a contact to a writable address book (the
// identity's default one when not given explicitly) and queues CardDAV
// reverse-sync. POST /api/desktop/v1/contacts
func (s *Server) HandleDesktopContactCreate(w http.ResponseWriter, r *http.Request) {
	s.contactAPI().create(w, r)
}

func (a contactAPI) create(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
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
			identity, _ = a.store.DefaultIdentityEmail(user.ID)
		}
		id, err := a.store.DefaultWriteAddressBookID(user.ID, identity)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to resolve address book")
			return
		}
		if id == 0 {
			a.respondReadOnly(w, user, "contacts.error.no_writable_book")
			return
		}
		bookID = id
	}
	book, err := a.store.GetAddressBookByID(bookID)
	if err != nil || book == nil || book.UserID != user.ID {
		respondError(w, http.StatusNotFound, "address book not found")
		return
	}
	if !book.Writable() {
		a.respondReadOnly(w, user, "contacts.error.read_only")
		return
	}

	c := &models.Contact{
		UserID:        user.ID,
		AddressBookID: bookID,
		UID:           generateUID(),
		LocalModified: true,
	}
	c.VCardData = buildContactVCard(c.UID, deref(req.FullName), derefList(req.Emails), derefList(req.Phones),
		deref(req.Organization), deref(req.Title))
	if err := fillFromCard(c); err != nil {
		respondError(w, http.StatusInternalServerError, "create failed")
		return
	}
	if err := a.store.CreateContact(c); err != nil {
		respondError(w, http.StatusInternalServerError, "create failed")
		return
	}
	if reverseSynced(book) {
		if err := a.store.QueueContactSync(c.ID, bookID, book.SourceID, c.UID, "", c.VCardData, "create"); err != nil {
			log.Printf("contact create: failed to queue reverse sync for %s: %v", c.UID, err)
		}
	}

	respondJSON(w, http.StatusCreated, map[string]any{"id": c.ID, "uid": c.UID})
}

// fillFromCard переносит в индексированные колонки контакта то, что клиент
// правит, из самой карточки — тем же разборщиком, что и синк.
func fillFromCard(c *models.Contact) error {
	f, err := vcardedit.Read(c.VCardData)
	if err != nil {
		return err
	}
	c.FullName = f.FullName
	c.GivenName = f.GivenName
	c.FamilyName = f.FamilyName
	c.SetEmails(f.Emails)
	c.SetPhones(f.Phones)
	c.Organization = f.Organization
	c.Title = f.Title
	return nil
}

// ownedContact загружает контакт пользователя и его книгу. Чужой контакт
// отвечает 404, как несуществующий. При false ответ уже записан.
func (a contactAPI) ownedContact(w http.ResponseWriter, r *http.Request, user *models.User) (*models.Contact, *models.AddressBook, bool) {
	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid id")
		return nil, nil, false
	}
	c, err := a.store.GetContactByID(id)
	if err != nil || c == nil || c.UserID != user.ID {
		respondError(w, http.StatusNotFound, "contact not found")
		return nil, nil, false
	}
	book, err := a.store.GetAddressBookByID(c.AddressBookID)
	if err != nil || book == nil || book.UserID != user.ID {
		respondError(w, http.StatusNotFound, "address book not found")
		return nil, nil, false
	}
	return c, book, true
}

// HandleDesktopContactPatch правит контакт. PATCH /contacts/{id}
//
// Карточка правится по строкам (vcardedit): меняются только присланные поля,
// значение которых отличается от карточки; фото, дни рождения, адреса,
// заметки, X-свойства, группы, четвёртый и дальше адреса — остаются байт в
// байт. Во внешний CardDAV уходит не карточка, а сама правка: воркер
// применит её к свежей версии с сервера и запишет с If-Match.
func (s *Server) HandleDesktopContactPatch(w http.ResponseWriter, r *http.Request) {
	s.contactAPI().patchContact(w, r)
}

func (a contactAPI) patchContact(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req contactWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	c, book, ok := a.ownedContact(w, r, user)
	if !ok {
		return
	}
	if !book.Writable() {
		a.respondReadOnly(w, user, "contacts.error.read_only")
		return
	}

	base := c.VCardData
	if _, err := vcardedit.Read(base); err != nil {
		// В строке нет разбираемой vCard (пустая или битая) — терять в ней
		// нечего: правим карточку, собранную из полей контакта.
		log.Printf("contact patch: contact %d has no parsable vCard, rebuilding from fields: %v", c.ID, err)
		base = buildContactVCard(c.UID, c.FullName, c.ListEmails(), c.ListPhones(), c.Organization, c.Title)
	}
	res, err := vcardedit.Apply(base, req.patch(), a.now())
	if err != nil {
		log.Printf("contact patch: contact %d has no parsable vCard: %v", c.ID, err)
		respondError(w, http.StatusUnprocessableEntity, "contact card is not a vCard")
		return
	}
	if len(res.Changed) == 0 {
		respondJSON(w, http.StatusOK, toDesktopContact(c, true))
		return
	}
	c.VCardData = res.Data
	if err := fillFromCard(c); err != nil {
		log.Printf("contact patch: edited card of contact %d does not parse: %v", c.ID, err)
		respondError(w, http.StatusInternalServerError, "update failed")
		return
	}
	c.LocalModified = true
	if err := a.store.UpdateContact(c); err != nil {
		respondError(w, http.StatusInternalServerError, "update failed")
		return
	}

	if reverseSynced(book) {
		// В очередь — только поля, которые правка реально поменяла: совпавшие
		// с карточкой значения не должны перезаписать на сервере то, что
		// там успели поменять.
		changed := onlyFields(req.patch(), res.Changed)
		patchJSON, err := json.Marshal(changed)
		if err == nil {
			err = a.store.QueueContactPatch(c.ID, c.AddressBookID, book.SourceID, c.UID, c.RemoteID, c.VCardData, string(patchJSON))
		}
		if err != nil {
			log.Printf("contact patch: failed to queue reverse sync for %s: %v", c.UID, err)
		}
	}
	respondJSON(w, http.StatusOK, toDesktopContact(c, true))
}

// onlyFields оставляет в правке только перечисленные поля (имена — как в
// vcardedit.Result.Changed).
func onlyFields(p vcardedit.Patch, names []string) vcardedit.Patch {
	var out vcardedit.Patch
	for _, n := range names {
		switch n {
		case "full_name":
			out.FullName = p.FullName
		case "emails":
			out.Emails = p.Emails
		case "phones":
			out.Phones = p.Phones
		case "organization":
			out.Organization = p.Organization
		case "title":
			out.Title = p.Title
		}
	}
	return out
}

// HandleDesktopContactDelete removes a contact. DELETE /contacts/{id}
func (s *Server) HandleDesktopContactDelete(w http.ResponseWriter, r *http.Request) {
	s.contactAPI().deleteContact(w, r)
}

func (a contactAPI) deleteContact(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	if user == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	c, book, ok := a.ownedContact(w, r, user)
	if !ok {
		return
	}
	if !book.Writable() {
		// Удалённый локально контакт книги только для чтения вернулся бы
		// следующим синком, а удаление наверху получило бы отказ.
		a.respondReadOnly(w, user, "contacts.error.read_only")
		return
	}
	// В очередь — до удаления: строка очереди ссылается на контакт.
	if reverseSynced(book) {
		if err := a.store.QueueContactSync(c.ID, c.AddressBookID, book.SourceID, c.UID, c.RemoteID, "", "delete"); err != nil {
			log.Printf("contact delete: failed to queue reverse sync for %s: %v", c.UID, err)
		}
	}
	if err := a.store.DeleteContact(c.ID); err != nil {
		respondError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}
