package client

import "testing"

// Ответ в стиле SOGo на `PROPFIND Depth: 1` без тела (allprop): у каждого
// `<response>` есть свой href, но внутри свойств лежат ЕЩЁ hrefs — owner,
// principal-URL, и т.п. Прежний поиск подстроки `<href` тянул их все, поэтому
// на реальном GAL получалось 7628 «путей» на 1231 карточку: мусор, который
// потом отваливался, и повторы одной и той же карточки, из-за которых сервер
// присылал её в multiget по нескольку раз, а вставка падала на уникальном
// индексе (address_book_id, uid) и откатывала всю книгу.
const sogoAllpropBody = `<?xml version="1.0" encoding="UTF-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:response>
    <D:href>/SOGo/dav/user@small.kz/Contacts/small.kz/</D:href>
    <D:propstat>
      <D:prop>
        <D:owner><D:href>/SOGo/dav/user@small.kz/</D:href></D:owner>
        <D:current-user-principal><D:href>/SOGo/dav/user@small.kz/</D:href></D:current-user-principal>
        <C:addressbook-home-set><D:href>/SOGo/dav/user@small.kz/Contacts/</D:href></C:addressbook-home-set>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
  <D:response>
    <D:href>/SOGo/dav/user@small.kz/Contacts/small.kz/1cteam@small.kz</D:href>
    <D:propstat>
      <D:prop>
        <D:getetag>"etag-1"</D:getetag>
        <D:owner><D:href>/SOGo/dav/user@small.kz/</D:href></D:owner>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
  <D:response>
    <D:href>/SOGo/dav/user@small.kz/Contacts/small.kz/a.abdeyev@small.kz</D:href>
    <D:propstat>
      <D:prop>
        <D:getetag>"etag-2"</D:getetag>
        <D:owner><D:href>/SOGo/dav/user@small.kz/</D:href></D:owner>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
  <D:response>
    <D:href>/SOGo/dav/user@small.kz/Contacts/small.kz/a.abdeyev@small.kz</D:href>
    <D:propstat>
      <D:prop><D:getetag>"etag-2"</D:getetag></D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`

func TestParseMultistatusHrefsTakesOnlyResourceHrefs(t *testing.T) {
	collection := "/SOGo/dav/user@small.kz/Contacts/small.kz/"

	paths, err := parseMultistatusHrefs([]byte(sogoAllpropBody), collection)
	if err != nil {
		t.Fatalf("parseMultistatusHrefs: %v", err)
	}

	want := []string{
		"/SOGo/dav/user@small.kz/Contacts/small.kz/1cteam@small.kz",
		"/SOGo/dav/user@small.kz/Contacts/small.kz/a.abdeyev@small.kz",
	}
	if len(paths) != len(want) {
		t.Fatalf("got %d paths %v, want %d %v", len(paths), paths, len(want), want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("path %d = %q, want %q", i, paths[i], want[i])
		}
	}
}

// Коллекция может прийти и без завершающего слеша в аргументе (RemoteID хранит
// путь по-разному), и как полный URL — тогда collectionPath пустой, а сама
// коллекция отсекается по слешу в конце href.
func TestParseMultistatusHrefsDropsCollectionItself(t *testing.T) {
	cases := map[string]string{
		"со слешем":    "/SOGo/dav/user@small.kz/Contacts/small.kz/",
		"без слеша":    "/SOGo/dav/user@small.kz/Contacts/small.kz",
		"пустой (URL)": "",
	}
	for name, collection := range cases {
		paths, err := parseMultistatusHrefs([]byte(sogoAllpropBody), collection)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, p := range paths {
			if p == "/SOGo/dav/user@small.kz/Contacts/small.kz/" || p == "/SOGo/dav/user@small.kz/Contacts/" {
				t.Errorf("%s: коллекция %q попала в список ресурсов", name, p)
			}
		}
		if len(paths) != 2 {
			t.Errorf("%s: got %d paths %v, want 2", name, len(paths), paths)
		}
	}
}

func TestParseMultistatusHrefsRejectsGarbage(t *testing.T) {
	if _, err := parseMultistatusHrefs([]byte("это не XML"), "/x/"); err == nil {
		t.Fatal("ожидалась ошибка разбора, получили nil")
	}
}

// Ответ SOGo на REPORT addressbook-multiget: рядом с нужными нам getetag и
// address-data лежит getlastmodified в формате, на котором декодер go-webdav
// падает целиком («cannot parse ", 27 Aug 2026 ..." as " "»). Из-за этого
// multiget считался неработающим и код уходил в поштучные GET'ы — тысячи
// запросов, дедлайн и разная книга каждый прогон. Наш разбор читает только
// href и address-data и на остальные свойства не смотрит.
const sogoMultigetBody = `<?xml version="1.0" encoding="UTF-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:response>
    <D:href>/SOGo/dav/user@small.kz/Contacts/small.kz/a.abdeyev@small.kz</D:href>
    <D:propstat>
      <D:prop>
        <D:getetag>"etag-1"</D:getetag>
        <D:getlastmodified>Thu, 27 Aug 2026 13:47:39 +0500</D:getlastmodified>
        <C:address-data>BEGIN:VCARD
VERSION:3.0
UID:a.abdeyev@small.kz
FN:Алибек Абдеев
EMAIL:a.abdeyev@small.kz
END:VCARD
</C:address-data>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
  <D:response>
    <D:href>/SOGo/dav/user@small.kz/Contacts/small.kz/gone@small.kz</D:href>
    <D:propstat>
      <D:prop><C:address-data/></D:prop>
      <D:status>HTTP/1.1 404 Not Found</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`

func TestParseAddressDataMultistatusIgnoresUnparseableProps(t *testing.T) {
	objects, err := parseAddressDataMultistatus([]byte(sogoMultigetBody))
	if err != nil {
		t.Fatalf("parseAddressDataMultistatus: %v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("got %d objects, want 1 (пустой address-data пропускается)", len(objects))
	}

	obj := objects[0]
	if obj.Path != "/SOGo/dav/user@small.kz/Contacts/small.kz/a.abdeyev@small.kz" {
		t.Errorf("path = %q", obj.Path)
	}
	if obj.ETag != `"etag-1"` {
		t.Errorf("etag = %q, want \"etag-1\"", obj.ETag)
	}
	if obj.Card == nil {
		t.Fatal("card не разобрана")
	}
	if uid := obj.Card.Value("UID"); uid != "a.abdeyev@small.kz" {
		t.Errorf("UID = %q", uid)
	}
	if fn := obj.Card.Value("FN"); fn != "Алибек Абдеев" {
		t.Errorf("FN = %q", fn)
	}
}
