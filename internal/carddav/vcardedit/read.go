package vcardedit

import (
	"strings"

	"github.com/emersion/go-vcard"
)

// Fields — поля контакта, которые правит клиент, прочитанные из карточки тем
// же разборщиком (go-vcard), что и при синхронизации: после правки по
// строкам индексированные колонки контакта берутся отсюда, а не из запроса.
type Fields struct {
	FullName     string
	GivenName    string
	FamilyName   string
	Emails       []string
	Phones       []string
	Organization string
	Title        string
}

// Read разбирает первую карточку data.
func Read(data string) (Fields, error) {
	card, err := vcard.NewDecoder(strings.NewReader(data)).Decode()
	if err != nil {
		return Fields{}, err
	}
	return FromCard(card), nil
}

// FromCard — поля из уже разобранной карточки. Адреса и телефоны — все, без
// предела.
func FromCard(card vcard.Card) Fields {
	var f Fields
	if fn := card.Get(vcard.FieldFormattedName); fn != nil {
		f.FullName = fn.Value
	}
	if n := card.Get(vcard.FieldName); n != nil {
		parts := strings.Split(n.Value, ";")
		f.FamilyName = parts[0]
		if len(parts) > 1 {
			f.GivenName = parts[1]
		}
	}
	if f.FullName == "" && (f.GivenName != "" || f.FamilyName != "") {
		f.FullName = strings.TrimSpace(f.GivenName + " " + f.FamilyName)
	}
	f.Emails = card.Values(vcard.FieldEmail)
	f.Phones = card.Values(vcard.FieldTelephone)
	if org := card.Get(vcard.FieldOrganization); org != nil {
		f.Organization = org.Value
	}
	if title := card.Get(vcard.FieldTitle); title != nil {
		f.Title = title.Value
	}
	return f
}
