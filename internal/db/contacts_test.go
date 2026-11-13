package db

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/migrations"
	"github.com/lib/pq"
)

// contactTestBook заводит пользователя, CardDAV-источник и книгу; всё
// убирается после теста.
func contactTestBook(t *testing.T, db *DB) (userID, sourceID, bookID int64) {
	t.Helper()
	u, err := db.CreateUser(fmt.Sprintf("zzcontacts%d", time.Now().UnixNano()), "x", "", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM contact_sync_queue WHERE address_book_id IN (SELECT id FROM address_books WHERE user_id = $1)`,
			`DELETE FROM contacts WHERE user_id = $1`,
			`DELETE FROM address_books WHERE user_id = $1`,
			`DELETE FROM contact_sources WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`,
		} {
			if _, err := db.DB.Exec(q, u.ID); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	})
	if err := db.QueryRow(`INSERT INTO contact_sources (user_id, name, source_type, identity_email, created_at, updated_at)
		VALUES ($1, 'test', 'carddav', 'me@example.org', 0, 0) RETURNING id`, u.ID).Scan(&sourceID); err != nil {
		t.Fatalf("source: %v", err)
	}
	book := &models.AddressBook{UserID: u.ID, SourceID: sourceID, RemoteID: "/book/", Name: "Книга", CanWrite: true, Enabled: true, ReverseSync: true}
	if err := db.CreateAddressBook(book); err != nil {
		t.Fatalf("CreateAddressBook: %v", err)
	}
	return u.ID, sourceID, book.ID
}

// TestContacts_AllEmailsAndPhones: больше трёх адресов и телефонов хранятся,
// читаются, ищутся и находятся по адресу; старые колонки — первые три.
func TestContacts_AllEmailsAndPhones(t *testing.T) {
	db := requireTestDB(t)
	userID, _, bookID := contactTestBook(t, db)

	c := &models.Contact{UserID: userID, AddressBookID: bookID, UID: "u1", VCardData: "BEGIN:VCARD\r\nEND:VCARD\r\n", FullName: "Пётр"}
	c.SetEmails([]string{"a@1.ru", "b@2.ru", "c@3.ru", "Fourth@4.ru", " ", "e@5.ru"})
	c.SetPhones([]string{"+1", "+2", "+3", "+4"})
	if err := db.CreateContact(c); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetContactByID(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Emails, ",") != "a@1.ru,b@2.ru,c@3.ru,Fourth@4.ru,e@5.ru" || len(got.Phones) != 4 {
		t.Errorf("прочитано: %v %v", got.Emails, got.Phones)
	}
	if got.Email3 != "c@3.ru" || got.Phone3 != "+3" {
		t.Errorf("старые колонки: %q %q", got.Email3, got.Phone3)
	}

	byMail, err := db.GetContactByEmail(userID, "fourth@4.ru")
	if err != nil || byMail == nil || byMail.ID != c.ID {
		t.Errorf("по четвёртому адресу: %v %v", byMail, err)
	}
	found, err := db.SearchContacts(userID, "e@5", 10)
	if err != nil || len(found) != 1 {
		t.Errorf("поиск по пятому адресу: %d %v", len(found), err)
	}
	found, err = db.SearchContacts(userID, "+4", 10)
	if err != nil || len(found) != 1 {
		t.Errorf("поиск по четвёртому телефону: %d %v", len(found), err)
	}

	got.SetEmails([]string{"only@1.ru"})
	if err := db.UpdateContact(got); err != nil {
		t.Fatal(err)
	}
	again, _ := db.GetContactByID(c.ID)
	if len(again.Emails) != 1 || again.Email2 != "" || again.Email3 != "" {
		t.Errorf("после правки: %v %q %q", again.Emails, again.Email2, again.Email3)
	}

	// Синк: upsert и update тоже пишут все значения.
	up := &models.Contact{UserID: userID, UID: "u2", VCardData: "x"}
	up.SetEmails([]string{"1@x", "2@x", "3@x", "4@x"})
	if err := db.ApplyContactSyncChanges(bookID, &SyncContactChanges{Creates: []*models.Contact{up}}); err != nil {
		t.Fatal(err)
	}
	synced, _ := db.GetContactByUID(bookID, "u2")
	if synced == nil || len(synced.Emails) != 4 {
		t.Fatalf("синк: %+v", synced)
	}
	synced.SetEmails([]string{"1@x", "2@x", "3@x", "4@x", "5@x"})
	if err := db.ApplyContactSyncChanges(bookID, &SyncContactChanges{Updates: []*models.Contact{synced}}); err != nil {
		t.Fatal(err)
	}
	synced, _ = db.GetContactByUID(bookID, "u2")
	if len(synced.Emails) != 5 {
		t.Errorf("синк-обновление: %v", synced.Emails)
	}
}

// TestBackfillContactValues: карточка с пятью адресами, у которой в колонках
// осталось три (так её записал код до миграции 057), получает все пять;
// второй прогон ничего не пишет.
func TestBackfillContactValues(t *testing.T) {
	db := requireTestDB(t)
	userID, _, bookID := contactTestBook(t, db)
	card := "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:x\r\nEMAIL:1@x\r\nEMAIL:2@x\r\nEMAIL:3@x\r\nEMAIL:4@x\r\nEMAIL:5@x\r\nTEL:+1\r\nEND:VCARD\r\n"
	var id int64
	if err := db.QueryRow(`INSERT INTO contacts (user_id, address_book_id, uid, vcard_data, email, email2, email3, phone, emails, phones, created_at, updated_at)
		VALUES ($1, $2, 'b1', $3, '1@x', '2@x', '3@x', '+1', '{1@x,2@x,3@x}', '{+1}', 0, 0) RETURNING id`, userID, bookID, card).Scan(&id); err != nil {
		t.Fatal(err)
	}
	read := func(data string) ([]string, []string, error) {
		var emails, phones []string
		for _, l := range strings.Split(data, "\r\n") {
			if v, ok := strings.CutPrefix(l, "EMAIL:"); ok {
				emails = append(emails, v)
			}
			if v, ok := strings.CutPrefix(l, "TEL:"); ok {
				phones = append(phones, v)
			}
		}
		return emails, phones, nil
	}
	n, err := db.BackfillContactValues(read)
	if err != nil || n < 1 {
		t.Fatalf("backfill: %d %v", n, err)
	}
	got, _ := db.GetContactByID(id)
	if len(got.Emails) != 5 || got.Emails[4] != "5@x" {
		t.Errorf("после backfill: %v", got.Emails)
	}
	if _, err := db.BackfillContactValues(read); err != nil {
		t.Fatal(err)
	}
	var stillDiffers int
	if err := db.QueryRow(`SELECT count(*) FROM contacts WHERE id = $1 AND cardinality(emails) <> 5`, id).Scan(&stillDiffers); err != nil || stillDiffers != 0 {
		t.Errorf("второй прогон: %d %v", stillDiffers, err)
	}
}

// TestQueueContactPatch: правки по полям сливаются; целая карточка в очереди
// (создание, PUT через CardDAV-сервер) остаётся целой.
func TestQueueContactPatch(t *testing.T) {
	db := requireTestDB(t)
	userID, sourceID, bookID := contactTestBook(t, db)
	c := &models.Contact{UserID: userID, AddressBookID: bookID, UID: "q1", RemoteID: "/book/q1.vcf", VCardData: "v0"}
	if err := db.CreateContact(c); err != nil {
		t.Fatal(err)
	}
	entry := func() *models.ContactSyncEntry {
		t.Helper()
		list, err := db.GetPendingContactSync(sourceID, 10)
		if err != nil || len(list) != 1 {
			t.Fatalf("очередь: %d %v", len(list), err)
		}
		return list[0]
	}

	if err := db.QueueContactPatch(c.ID, bookID, sourceID, c.UID, c.RemoteID, "v1", `{"full_name":"A"}`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueueContactPatch(c.ID, bookID, sourceID, c.UID, c.RemoteID, "v2", `{"emails":["x@y"],"full_name":"B"}`); err != nil {
		t.Fatal(err)
	}
	e := entry()
	var p map[string]any
	if err := json.Unmarshal([]byte(e.Patch), &p); err != nil {
		t.Fatalf("patch %q: %v", e.Patch, err)
	}
	if e.Operation != "update" || e.VCardData != "v2" || p["full_name"] != "B" || p["emails"] == nil {
		t.Errorf("слияние: %+v %v", e, p)
	}

	// Целая карточка сбрасывает правку по полям...
	if err := db.QueueContactSync(c.ID, bookID, sourceID, c.UID, c.RemoteID, "v3", "update"); err != nil {
		t.Fatal(err)
	}
	if e := entry(); e.Patch != "" || e.VCardData != "v3" {
		t.Errorf("после целой карточки: %+v", e)
	}
	// ...и последующая правка в неё уже вошла: запись остаётся целой.
	if err := db.QueueContactPatch(c.ID, bookID, sourceID, c.UID, c.RemoteID, "v4", `{"title":"T"}`); err != nil {
		t.Fatal(err)
	}
	if e := entry(); e.Patch != "" || e.VCardData != "v4" || e.Operation != "update" {
		t.Errorf("правка поверх целой: %+v", e)
	}

	// Создание остаётся созданием.
	if _, err := db.DB.Exec(`DELETE FROM contact_sync_queue WHERE contact_id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueueContactSync(c.ID, bookID, sourceID, c.UID, "", "v5", "create"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueueContactPatch(c.ID, bookID, sourceID, c.UID, "", "v6", `{"title":"T"}`); err != nil {
		t.Fatal(err)
	}
	if e := entry(); e.Operation != "create" || e.Patch != "" || e.VCardData != "v6" {
		t.Errorf("правка поверх создания: %+v", e)
	}
}

// TestRefreshAddressBookCanWrite: права меняются только по ответу сервера.
func TestRefreshAddressBookCanWrite(t *testing.T) {
	db := requireTestDB(t)
	_, _, bookID := contactTestBook(t, db)
	book, _ := db.GetAddressBookByID(bookID)

	if err := db.RefreshAddressBookCanWrite(book, &models.AddressBook{CanWrite: false}); err != nil {
		t.Fatal(err)
	}
	if b, _ := db.GetAddressBookByID(bookID); !b.CanWrite {
		t.Errorf("без ответа сервера права поменялись")
	}
	if err := db.RefreshAddressBookCanWrite(book, &models.AddressBook{CanWrite: false, CanWriteKnown: true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := db.GetAddressBookByID(bookID); b.CanWrite || book.CanWrite {
		t.Errorf("can_write не снят: db=%v mem=%v", b.CanWrite, book.CanWrite)
	}
}

// TestMigration057_KeepsLegacyContacts строит схему до 056 на ПУСТОЙ базе
// (MAILSERVER_MIGRATE_057_DSN; тест её меняет), кладёт контакты в старом
// виде и применяет 057: массивы заполняются из трёх колонок без потерь и в
// том же порядке, повторный прогон файла ничего не меняет.
func TestMigration057_KeepsLegacyContacts(t *testing.T) {
	db := openMigrateTestDB(t, "MAILSERVER_MIGRATE_057_DSN")
	ctx := context.Background()

	upTo := func(last string) fs.FS {
		all, err := LoadMigrations(migrations.FS())
		if err != nil {
			t.Fatal(err)
		}
		m := fstest.MapFS{}
		for _, mig := range all {
			if ok, _ := mig.versionAtMost(last); ok {
				m[mig.File] = &fstest.MapFile{Data: []byte(mig.SQL)}
			}
		}
		return m
	}
	if err := db.RunMigrations(ctx, upTo("056")); err != nil {
		t.Fatalf("migrate to 056: %v", err)
	}

	u, err := db.CreateUser("zzmig057", "x", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var sourceID, bookID int64
	if err := db.QueryRow(`INSERT INTO contact_sources (user_id, name, source_type, identity_email, created_at, updated_at)
		VALUES ($1, 's', 'carddav', 'me@x', 0, 0) RETURNING id`, u.ID).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`INSERT INTO address_books (user_id, source_id, name, can_write, created_at, updated_at)
		VALUES ($1, $2, 'b', true, 0, 0) RETURNING id`, u.ID, sourceID).Scan(&bookID); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		uid                  string
		e1, e2, e3, p1, p2   any
		wantEmails, wantTels string
	}{
		{"full", "a@x", "b@x", "c@x", "+1", "+2", "a@x,b@x,c@x", "+1,+2"},
		{"gap", "a@x", "", "c@x", nil, "+2", "a@x,c@x", "+2"},
		{"none", nil, nil, nil, nil, nil, "", ""},
		{"spaces", "  ", "b@x", nil, " ", nil, "b@x", ""},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO contacts (user_id, address_book_id, uid, vcard_data, email, email2, email3, phone, phone2, created_at, updated_at)
			VALUES ($1, $2, $3, 'x', $4, $5, $6, $7, $8, 0, 0)`, u.ID, bookID, r.uid, r.e1, r.e2, r.e3, r.p1, r.p2); err != nil {
			t.Fatal(err)
		}
	}

	check := func(stage string) {
		t.Helper()
		for _, r := range rows {
			var emails, phones []string
			if err := db.QueryRow(`SELECT emails, phones FROM contacts WHERE uid = $1`, r.uid).Scan(pq.Array(&emails), pq.Array(&phones)); err != nil {
				t.Fatalf("%s %s: %v", stage, r.uid, err)
			}
			if strings.Join(emails, ",") != r.wantEmails || strings.Join(phones, ",") != r.wantTels {
				t.Errorf("%s %s: emails=%v phones=%v", stage, r.uid, emails, phones)
			}
		}
	}
	if err := db.RunMigrations(ctx, migrations.FS()); err != nil {
		t.Fatalf("migrate to latest: %v", err)
	}
	check("057")

	// Идемпотентность: тот же файл ещё раз (как при ручном прогоне).
	raw, err := fs.ReadFile(migrations.FS(), "057_contact_all_values.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(raw)); err != nil {
		t.Fatalf("re-run 057: %v", err)
	}
	check("057 again")
}
