package db

import (
	"fmt"
	"log"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/lib/pq"
)

// BackfillContactValues дописывает в contacts.emails / contacts.phones адреса
// и телефоны, которые до миграции 057 отбрасывались после третьего. SQL
// миграции заполнил массивы из трёх старых колонок; где были заняты все три
// (или массив пуст при заполненной колонке — строку записал прежний бинарник
// после отката), значений могло быть больше, и правду знает только
// vcard_data. read разбирает карточку (vcardedit.Read).
//
// Идемпотентна и дешёвая на каждом старте: пишет лишь строки, где
// разобранная карточка расходится с сохранённым.
func (db *DB) BackfillContactValues(read func(vcardData string) (emails, phones []string, err error)) (int, error) {
	rows, err := db.Query(`
		SELECT id, vcard_data, emails, phones
		FROM contacts
		WHERE cardinality(emails) >= 3 OR cardinality(phones) >= 3
		   OR (cardinality(emails) = 0 AND COALESCE(email, '') <> '')
		   OR (cardinality(phones) = 0 AND COALESCE(phone, '') <> '')
	`)
	if err != nil {
		return 0, fmt.Errorf("select contacts for backfill: %w", err)
	}
	var fixes []*models.Contact
	for rows.Next() {
		var (
			id             int64
			data           string
			emails, phones []string
		)
		if err := rows.Scan(&id, &data, pq.Array(&emails), pq.Array(&phones)); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan contact for backfill: %w", err)
		}
		gotEmails, gotPhones, err := read(data)
		if err != nil {
			continue // карточка не разбирается — оставляем как есть
		}
		c := &models.Contact{ID: id}
		c.SetEmails(gotEmails)
		c.SetPhones(gotPhones)
		if sameStrings(c.Emails, emails) && sameStrings(c.Phones, phones) {
			continue
		}
		fixes = append(fixes, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate contacts for backfill: %w", err)
	}
	rows.Close()

	done := 0
	for _, c := range fixes {
		// Ошибка одной строки (скажем, телефон длиннее старой колонки
		// phone VARCHAR(50)) не должна останавливать остальные.
		if _, err := db.Exec(`
			UPDATE contacts SET emails = $1, phones = $2,
				email = $3, email2 = $4, email3 = $5, phone = $6, phone2 = $7, phone3 = $8
			WHERE id = $9`,
			pq.Array(c.Emails), pq.Array(c.Phones),
			c.Email, c.Email2, c.Email3, c.Phone, c.Phone2, c.Phone3, c.ID); err != nil {
			log.Printf("BackfillContactValues: contact %d: %v", c.ID, err)
			continue
		}
		done++
	}
	return done, nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
