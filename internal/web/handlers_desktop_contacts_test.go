package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/carddav/vcardedit"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/gorilla/mux"
)

const contactUserID = 7

// memContactStore — книги, контакты и очередь обратной синхронизации в памяти.
type memContactStore struct {
	mu       sync.Mutex
	books    map[int64]*models.AddressBook
	contacts map[int64]*models.Contact
	nextID   int64
	// queue — contact_id → запись, как UNIQUE(contact_id) у настоящей очереди.
	queue map[int64]queuedContact
}

type queuedContact struct {
	op, vcard, patch string
}

func newMemContactStore(books ...*models.AddressBook) *memContactStore {
	m := &memContactStore{books: map[int64]*models.AddressBook{}, contacts: map[int64]*models.Contact{}, queue: map[int64]queuedContact{}, nextID: 100}
	for _, b := range books {
		m.books[b.ID] = b
	}
	return m
}

func (m *memContactStore) GetAllUserContacts(userID int64, limit int) ([]*models.Contact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*models.Contact
	for _, c := range m.contacts {
		if c.UserID == userID {
			cp := *c
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (m *memContactStore) SearchContacts(userID int64, q string, limit int) ([]*models.Contact, error) {
	all, _ := m.GetAllUserContacts(userID, limit)
	var out []*models.Contact
	for _, c := range all {
		if strings.Contains(strings.Join(append([]string{c.FullName}, c.Emails...), " "), q) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memContactStore) GetAddressBooksByUserID(userID int64) ([]*models.AddressBook, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*models.AddressBook
	for _, b := range m.books {
		if b.UserID == userID {
			cp := *b
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (m *memContactStore) GetAddressBookByID(id int64) (*models.AddressBook, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.books[id]; ok {
		cp := *b
		return &cp, nil
	}
	return nil, fmt.Errorf("address book not found")
}

func (m *memContactStore) GetContactByID(id int64) (*models.Contact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.contacts[id]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, fmt.Errorf("contact not found")
}

func (m *memContactStore) CreateContact(c *models.Contact) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	c.ID = m.nextID
	cp := *c
	m.contacts[c.ID] = &cp
	return nil
}

func (m *memContactStore) UpdateContact(c *models.Contact) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *c
	m.contacts[c.ID] = &cp
	return nil
}

func (m *memContactStore) DeleteContact(id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.contacts, id)
	return nil
}

func (m *memContactStore) QueueContactSync(contactID, addressBookID, sourceID int64, uid, remoteID, vcardData, operation string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queue[contactID] = queuedContact{op: operation, vcard: vcardData}
	return nil
}

func (m *memContactStore) QueueContactPatch(contactID, addressBookID, sourceID int64, uid, remoteID, vcardData, patch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queue[contactID] = queuedContact{op: "update", vcard: vcardData, patch: patch}
	return nil
}

func (m *memContactStore) DefaultIdentityEmail(userID int64) (string, error) {
	return "me@example.org", nil
}

func (m *memContactStore) DefaultWriteAddressBookID(userID int64, identity string) (int64, error) {
	return 1, nil
}

func contactFixture() *memContactStore {
	return newMemContactStore(
		&models.AddressBook{ID: 1, UserID: contactUserID, SourceID: 10, Name: "iCloud", CanWrite: true, Enabled: true, ReverseSync: true, SourceType: "carddav", RemoteID: "/123/carddavhome/card/"},
		&models.AddressBook{ID: 2, UserID: contactUserID, SourceID: 11, Name: "GAL small.kz", CanWrite: false, Enabled: true, ReverseSync: true, SourceType: "carddav"},
		&models.AddressBook{ID: 3, UserID: contactUserID, SourceID: 12, Name: "Contacts", CanWrite: true, Enabled: true, SourceType: "local"},
		&models.AddressBook{ID: 4, UserID: contactUserID, SourceID: 13, Name: "LDAP", CanWrite: true, Enabled: true, SourceType: "ldap"},
		&models.AddressBook{ID: 5, UserID: 99, SourceID: 14, Name: "Чужая", CanWrite: true, Enabled: true, SourceType: "carddav"},
	)
}

// addCard кладёт контакт с карточкой card так, как его положил бы синк.
func (m *memContactStore) addCard(t *testing.T, bookID int64, card string) *models.Contact {
	t.Helper()
	c := &models.Contact{UserID: m.books[bookID].UserID, AddressBookID: bookID, UID: "uid-" + fmt.Sprint(len(m.contacts)), RemoteID: "/123/carddavhome/card/x.vcf", VCardData: card, ETag: `"e1"`}
	if err := fillFromCard(c); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateContact(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func contactRouter(store contactStore) *mux.Router {
	a := contactAPI{
		store: store,
		t:     func(_ *models.User, key string) string { return "T(" + key + ")" },
		now:   func() time.Time { return time.Date(2026, 11, 14, 1, 2, 3, 0, time.UTC) },
	}
	r := mux.NewRouter()
	r.HandleFunc("/api/desktop/v1/contacts/search", a.search).Methods("GET")
	r.HandleFunc("/api/desktop/v1/contacts", a.list).Methods("GET")
	r.HandleFunc("/api/desktop/v1/contacts", a.create).Methods("POST")
	r.HandleFunc("/api/desktop/v1/contacts/{id}", a.patchContact).Methods("PATCH")
	r.HandleFunc("/api/desktop/v1/contacts/{id}", a.deleteContact).Methods("DELETE")
	return r
}

func contactCall(t *testing.T, store contactStore, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(setUserContext(req.Context(), &models.User{ID: contactUserID, Username: "alice"}))
	rec := httptest.NewRecorder()
	contactRouter(store).ServeHTTP(rec, req)
	return rec
}

// fullCard — карточка iCloud: фото, день рождения, адрес, X-свойства, заметка
// и четыре адреса (два — в группах с подписями Apple).
const fullCard = "BEGIN:VCARD\r\n" +
	"VERSION:3.0\r\n" +
	"PRODID:-//Apple Inc.//iOS 18.4//EN\r\n" +
	"N:Петров;Иван;;;\r\n" +
	"FN:Иван Петров\r\n" +
	"ORG:ACME;\r\n" +
	"EMAIL;type=INTERNET;type=HOME;type=pref:ivan@home.example\r\n" +
	"EMAIL;type=INTERNET;type=WORK:ivan@work.example\r\n" +
	"item1.EMAIL;type=INTERNET:ivan@third.example\r\n" +
	"item1.X-ABLabel:_$!<Other>!$_\r\n" +
	"item2.EMAIL;type=INTERNET:ivan@fourth.example\r\n" +
	"item2.X-ABLabel:Дача\r\n" +
	"TEL;type=CELL;type=VOICE;type=pref:+7 900 111-22-33\r\n" +
	"item3.ADR;type=HOME;type=pref:;;Тверская\\, 1;Москва;;125009;Россия\r\n" +
	"item3.X-ABADR:ru\r\n" +
	"NOTE:Любит чай\r\n" +
	"BDAY;value=date:1985-03-02\r\n" +
	"X-ABRELATEDNAMES;type=pref:Анна\r\n" +
	"X-ABShowAs:PERSON\r\n" +
	"X-SOCIALPROFILE;type=telegram:x-apple:ivanp\r\n" +
	"PHOTO;ENCODING=b;TYPE=JPEG:/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAMCAgICAgMCAgIDAwMDBAYEBA\r\n" +
	" QFBQQEBAQEBQcFBQcIDAoHBwcKDQoMDA0MCxMUExEREhEVFxIUFxQRDw0SFBMHBwcI\r\n" +
	"UID:0A1B2C3D-0000-4000-8000-000000000001\r\n" +
	"REV:2026-10-01T10:00:00Z\r\n" +
	"END:VCARD\r\n"

func TestContactList_AllValuesBookAndCanWrite(t *testing.T) {
	store := contactFixture()
	icloud := store.addCard(t, 1, fullCard)
	gal := store.addCard(t, 2, "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Коллега\r\nEMAIL:c@small.kz\r\nEND:VCARD\r\n")
	local := store.addCard(t, 3, "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Сосед\r\nEND:VCARD\r\n")
	ldap := store.addCard(t, 4, "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Каталог\r\nEND:VCARD\r\n")

	rec := contactCall(t, store, "GET", "/api/desktop/v1/contacts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var list []DesktopContact
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	byID := map[int64]DesktopContact{}
	for _, c := range list {
		byID[c.ID] = c
	}
	got := byID[icloud.ID]
	if len(got.Emails) != 4 || got.Emails[3] != "ivan@fourth.example" {
		t.Errorf("адресов не все: %v", got.Emails)
	}
	if got.AddressBookID != 1 || !got.CanWrite {
		t.Errorf("iCloud: book=%d can_write=%v", got.AddressBookID, got.CanWrite)
	}
	for _, c := range []struct {
		id   int64
		book int64
		want bool
	}{{gal.ID, 2, false}, {local.ID, 3, true}, {ldap.ID, 4, false}} {
		if byID[c.id].AddressBookID != c.book || byID[c.id].CanWrite != c.want {
			t.Errorf("контакт %d: %+v, ждали книгу %d can_write=%v", c.id, byID[c.id], c.book, c.want)
		}
	}
	// Поле есть в JSON и при false — клиент не должен путать «нет» с «не прислали».
	if !strings.Contains(rec.Body.String(), `"can_write":false`) {
		t.Errorf("can_write:false не сериализован: %s", rec.Body)
	}
}

func TestContactPatch_RenameKeepsCardAndQueuesOnlyName(t *testing.T) {
	store := contactFixture()
	c := store.addCard(t, 1, fullCard)

	// Клиент шлёт форму целиком: имя новое, остальное — как было.
	body := `{"full_name":"Иван Петров-Водкин","emails":["ivan@home.example","ivan@work.example","ivan@third.example","ivan@fourth.example"],` +
		`"phones":["+7 900 111-22-33"],"organization":"ACME;","title":""}`
	rec := contactCall(t, store, "PATCH", fmt.Sprintf("/api/desktop/v1/contacts/%d", c.ID), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	saved, _ := store.GetContactByID(c.ID)
	want := strings.Replace(fullCard, "N:Петров;Иван;;;", "N:Петров-Водкин;Иван;;;", 1)
	want = strings.Replace(want, "FN:Иван Петров", "FN:Иван Петров-Водкин", 1)
	want = strings.Replace(want, "REV:2026-10-01T10:00:00Z", "REV:20261114T010203Z", 1)
	if saved.VCardData != want {
		t.Errorf("карточка изменилась не только в имени:\n%s", saved.VCardData)
	}
	if saved.FullName != "Иван Петров-Водкин" || saved.FamilyName != "Петров-Водкин" || len(saved.Emails) != 4 || !saved.LocalModified {
		t.Errorf("поля контакта: %+v", saved)
	}

	q, ok := store.queue[c.ID]
	if !ok || q.op != "update" {
		t.Fatalf("в очереди: %+v", q)
	}
	var p vcardedit.Patch
	if err := json.Unmarshal([]byte(q.patch), &p); err != nil {
		t.Fatal(err)
	}
	if p.FullName == nil || *p.FullName != "Иван Петров-Водкин" || p.Emails != nil || p.Phones != nil || p.Organization != nil || p.Title != nil {
		t.Errorf("в очередь ушло лишнее: %s", q.patch)
	}

	var resp DesktopContact
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.FullName != "Иван Петров-Водкин" || !resp.CanWrite {
		t.Errorf("ответ: %s", rec.Body)
	}
}

func TestContactPatch_AbsentFieldsUntouched(t *testing.T) {
	store := contactFixture()
	c := store.addCard(t, 1, fullCard)
	// Только телефоны: имя, адреса и организация в запросе отсутствуют —
	// значит, не трогаются (раньше пустые значения стирали их).
	rec := contactCall(t, store, "PATCH", fmt.Sprintf("/api/desktop/v1/contacts/%d", c.ID), `{"phones":["+7 900 111-22-33","+7 495 000-00-00"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	saved, _ := store.GetContactByID(c.ID)
	if saved.FullName != "Иван Петров" || len(saved.Emails) != 4 || len(saved.Phones) != 2 || saved.Organization != "ACME;" {
		t.Errorf("сохранено: %+v", saved)
	}
	if !strings.Contains(saved.VCardData, "TEL;type=CELL;type=VOICE;type=pref:+7 900 111-22-33\r\nTEL:+7 495 000-00-00\r\n") {
		t.Errorf("новый телефон не за старым:\n%s", saved.VCardData)
	}
}

func TestContactPatch_NoChangeNoQueue(t *testing.T) {
	store := contactFixture()
	c := store.addCard(t, 1, fullCard)
	rec := contactCall(t, store, "PATCH", fmt.Sprintf("/api/desktop/v1/contacts/%d", c.ID), `{"full_name":"Иван Петров"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if _, queued := store.queue[c.ID]; queued {
		t.Errorf("пустая правка ушла в очередь")
	}
	saved, _ := store.GetContactByID(c.ID)
	if saved.VCardData != fullCard {
		t.Errorf("карточка изменилась")
	}
}

func TestContactWrite_ReadOnlyBook403(t *testing.T) {
	store := contactFixture()
	gal := store.addCard(t, 2, "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Коллега\r\nEND:VCARD\r\n")
	ldap := store.addCard(t, 4, "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Каталог\r\nEND:VCARD\r\n")

	for _, tc := range []struct{ method, path, body string }{
		{"PATCH", fmt.Sprintf("/api/desktop/v1/contacts/%d", gal.ID), `{"full_name":"x"}`},
		{"DELETE", fmt.Sprintf("/api/desktop/v1/contacts/%d", gal.ID), ""},
		{"PATCH", fmt.Sprintf("/api/desktop/v1/contacts/%d", ldap.ID), `{"full_name":"x"}`},
		{"POST", "/api/desktop/v1/contacts", `{"full_name":"x","address_book_id":2}`},
	} {
		rec := contactCall(t, store, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status %d, ждали 403", tc.method, tc.path, rec.Code)
			continue
		}
		var e map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if e["code"] != "address_book_read_only" || e["message"] != "T(contacts.error.read_only)" || e["error"] == "" {
			t.Errorf("%s %s: тело %s", tc.method, tc.path, rec.Body)
		}
	}
	if _, ok := store.contacts[gal.ID]; !ok {
		t.Errorf("контакт из книги только для чтения удалён")
	}
	if len(store.queue) != 0 {
		t.Errorf("очередь: %+v", store.queue)
	}
}

func TestContactWrite_ForeignContact404(t *testing.T) {
	store := contactFixture()
	other := store.addCard(t, 5, "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Чужой\r\nEND:VCARD\r\n")
	rec := contactCall(t, store, "PATCH", fmt.Sprintf("/api/desktop/v1/contacts/%d", other.ID), `{"full_name":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d", rec.Code)
	}
}

func TestContactCreate_AllValues(t *testing.T) {
	store := contactFixture()
	body := `{"full_name":"Анна Каренина","emails":["a@1.ru","a@2.ru","a@3.ru","a@4.ru","a@5.ru"],"phones":["1","2","3","4"],"organization":"Свет","title":"","address_book_id":1}`
	rec := contactCall(t, store, "POST", "/api/desktop/v1/contacts", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp struct{ ID int64 }
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	c, _ := store.GetContactByID(resp.ID)
	if len(c.Emails) != 5 || len(c.Phones) != 4 || c.Email3 != "a@3.ru" || c.FamilyName != "Каренина" || c.GivenName != "Анна" {
		t.Errorf("контакт: %+v", c)
	}
	f, err := vcardedit.Read(c.VCardData)
	if err != nil || len(f.Emails) != 5 || f.Organization != "Свет" {
		t.Errorf("карточка %v:\n%s", err, c.VCardData)
	}
	if q := store.queue[c.ID]; q.op != "create" || q.vcard != c.VCardData {
		t.Errorf("очередь: %+v", q)
	}

	// Локальная книга — без очереди: её отдаёт наш CardDAV-сервер.
	rec = contactCall(t, store, "POST", "/api/desktop/v1/contacts", `{"full_name":"Сосед","address_book_id":3}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, ok := store.queue[resp.ID]; ok {
		t.Errorf("локальная книга ушла в очередь")
	}
	c, _ = store.GetContactByID(resp.ID)
	if !strings.Contains(c.VCardData, "\r\nN:;Сосед;;;\r\n") && !strings.Contains(c.VCardData, "\r\nN:;;;;\r\n") {
		t.Errorf("нет N:\n%s", c.VCardData)
	}
}
