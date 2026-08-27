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
