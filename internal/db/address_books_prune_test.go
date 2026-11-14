package db

import (
	"sort"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/models"
)

// TestPruneVanishedAddressBooks воспроизводит прод (аудит 2026-11-14, №7):
// у источника Яндекса книги /1/ и /2/ живые, а organization:…:0/ и
// user:…:0/ остались от сентябрьского обнаружения и больше не приходят.
// Запись в «призрачную» книгу Яндекс клал в /1/, и контакт появлялся дважды.
func TestPruneVanishedAddressBooks(t *testing.T) {
	db := requireTestDB(t)
	userID, sourceID, _ := contactTestBook(t, db) // книга "/book/" — тоже пропала
	const srcURL = "https://carddav.example.org"

	mk := func(remote, name string) int64 {
		t.Helper()
		b := &models.AddressBook{UserID: userID, SourceID: sourceID, RemoteID: remote, Name: name, CanWrite: true, Enabled: true, ReverseSync: true}
		if err := db.CreateAddressBook(b); err != nil {
			t.Fatalf("CreateAddressBook %s: %v", remote, err)
		}
		return b.ID
	}
	personal := mk("/addressbook/u@example.org/1/", "Personal")
	shared := mk("/addressbook/u@example.org/2/", "Shared")
	ghostUser := mk("/addressbook/u@example.org/user:1:0/", "")
	ghostOrg := mk("/addressbook/u@example.org/organization:2:0/", "Org")
	pending := mk("/addressbook/u@example.org/3/", "External")
	placeholder := mk(srcURL+"/", "Placeholder")

	// Контакт в призрачной книге уходит вместе с ней.
	c := &models.Contact{UserID: userID, AddressBookID: ghostUser, UID: "ghost-1", FullName: "Призрак"}
	if err := db.CreateContact(c); err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	// У «External» неотправленная правка — книгу пока не трогаем.
	pc := &models.Contact{UserID: userID, AddressBookID: pending, UID: "pending-1", FullName: "Ждёт"}
	if err := db.CreateContact(pc); err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	if err := db.QueueContactSync(pc.ID, pending, sourceID, pc.UID, "", "BEGIN:VCARD\r\nEND:VCARD\r\n", "create"); err != nil {
		t.Fatalf("QueueContactSync: %v", err)
	}

	exists := func(id int64) bool {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM address_books WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}

	// Обнаружение упало — пришла заглушка: ничего не удаляем.
	if got, err := db.PruneVanishedAddressBooks(sourceID, srcURL, []*models.AddressBook{{RemoteID: srcURL}}); err != nil || len(got) != 0 {
		t.Fatalf("заглушка: удалено %v, err %v", got, err)
	}
	// Пустой ответ — тоже ничего.
	if got, err := db.PruneVanishedAddressBooks(sourceID, srcURL, nil); err != nil || len(got) != 0 {
		t.Fatalf("пусто: удалено %v, err %v", got, err)
	}

	// Настоящий ответ: только /1/ и /2/, href с %40, как отдаёт Яндекс.
	discovered := []*models.AddressBook{
		{RemoteID: "/addressbook/u%40example.org/1/"},
		{RemoteID: "/addressbook/u@example.org/2/"},
	}
	got, err := db.PruneVanishedAddressBooks(sourceID, srcURL, discovered)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if want := []string{"", "Org", "Книга"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("удалены %q, ждали %q", got, want)
	}
	for id, want := range map[int64]bool{personal: true, shared: true, ghostUser: false, ghostOrg: false, pending: true, placeholder: true} {
		if exists(id) != want {
			t.Errorf("книга %d: есть=%v, ждали %v", id, !want, want)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM contacts WHERE id = $1`, c.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("контакт призрачной книги остался: %d %v", n, err)
	}

	// Заглушка со слэшем на конце при адресе источника без слэша — пустая,
	// её убирает PruneDirectURLAddressBook.
	if pruned, err := db.PruneDirectURLAddressBookIfDiscovered(sourceID, srcURL, discovered); err != nil || !pruned {
		t.Errorf("заглушка со слэшем не удалена: %v %v", pruned, err)
	}
	if exists(placeholder) {
		t.Errorf("заглушка %d осталась", placeholder)
	}
}
