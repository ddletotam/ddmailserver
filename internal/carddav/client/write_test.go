package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/carddav/vcardedit"
	"github.com/ddletotam/ddmailserver/internal/models"
)

// Ответ SOGo на PROPFIND current-user-privilege-set у корпоративного
// каталога: только чтение.
const sogoReadOnlyPrivileges = `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:response>
    <D:href>/SOGo/dav/user@small.kz/Contacts/small.kz/</D:href>
    <D:propstat>
      <D:prop>
        <D:current-user-privilege-set>
          <D:privilege><D:read/></D:privilege>
          <D:privilege><D:read-current-user-privilege-set/></D:privilege>
          <D:privilege><C:addressbook-query xmlns:C="urn:ietf:params:xml:ns:carddav"/></D:privilege>
        </D:current-user-privilege-set>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`

// iCloud: личная книга, запись разрешена (префикс другой — пространство то же).
const icloudWritablePrivileges = `<?xml version="1.0" encoding="UTF-8"?>
<multistatus xmlns="DAV:">
 <response>
  <href>/123/carddavhome/card/</href>
  <propstat>
   <prop>
    <current-user-privilege-set>
     <privilege><read/></privilege>
     <privilege><write/></privilege>
    </current-user-privilege-set>
   </prop>
   <status>HTTP/1.1 200 OK</status>
  </propstat>
 </response>
</multistatus>`

// Сервер, который свойство не знает: 404 в propstat.
const unknownPrivileges = `<?xml version="1.0" encoding="UTF-8"?>
<d:multistatus xmlns:d="DAV:">
 <d:response>
  <d:href>/dav/book/</d:href>
  <d:propstat>
   <d:prop><d:current-user-privilege-set/></d:prop>
   <d:status>HTTP/1.1 404 Not Found</d:status>
  </d:propstat>
 </d:response>
</d:multistatus>`

func TestParsePrivilegeSet(t *testing.T) {
	for _, tc := range []struct {
		name            string
		body            string
		writable, known bool
	}{
		{"SOGo GAL", sogoReadOnlyPrivileges, false, true},
		{"iCloud", icloudWritablePrivileges, true, true},
		{"write-content", `<multistatus xmlns="DAV:"><response><propstat><prop><current-user-privilege-set><privilege><write-content/></privilege></current-user-privilege-set></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`, true, true},
		{"all", `<multistatus xmlns="DAV:"><response><propstat><prop><current-user-privilege-set><privilege><all/></privilege></current-user-privilege-set></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`, true, true},
		{"404", unknownPrivileges, false, false},
		{"garbage", "not xml", false, false},
	} {
		w, k := parsePrivilegeSet([]byte(tc.body))
		if w != tc.writable || k != tc.known {
			t.Errorf("%s: writable=%v known=%v, ждали %v %v", tc.name, w, k, tc.writable, tc.known)
		}
	}
}

// fakeCardServer — одна карточка с ETag; PUT проверяет If-Match.
type fakeCardServer struct {
	mu   sync.Mutex
	card string
	etag int
	// beforePut — вызывается перед обработкой PUT (имитация чужой правки
	// между нашими GET и PUT).
	beforePut func(s *fakeCardServer)
	puts      int
	forbid    bool
}

func (f *fakeCardServer) tag() string { return `"v` + string(rune('0'+f.etag)) + `"` }

func (f *fakeCardServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		if f.card == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", f.tag())
		_, _ = io.WriteString(w, f.card)
	case http.MethodPut:
		f.puts++
		if f.beforePut != nil {
			hook := f.beforePut
			f.beforePut = nil
			hook(f)
		}
		if f.forbid {
			http.Error(w, "read-only", http.StatusForbidden)
			return
		}
		if r.Header.Get("If-Match") != f.tag() {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.card = string(b)
		f.etag++
		w.WriteHeader(http.StatusNoContent)
	}
}

func connectTo(t *testing.T, url string) *Client {
	t.Helper()
	c := New(&models.ContactSource{Name: "test", CardDAVURL: url, AuthType: "password", CardDAVUsername: "u", CardDAVPassword: "p"}, nil)
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	return c
}

func renameTo(name string) func(string) (string, error) {
	return func(cur string) (string, error) {
		res, err := vcardedit.Apply(cur, vcardedit.Patch{FullName: &name}, time.Unix(0, 0))
		return res.Data, err
	}
}

func TestPatchContact_RetriesOnConflictKeepingRemoteChange(t *testing.T) {
	f := &fakeCardServer{card: "BEGIN:VCARD\r\nVERSION:3.0\r\nN:Петров;Иван;;;\r\nFN:Иван Петров\r\nEND:VCARD\r\n", etag: 1}
	// Между нашим GET и PUT в iCloud добавили заметку — PUT получает 412,
	// правка повторяется на свежей карточке, и заметка не теряется.
	f.beforePut = func(s *fakeCardServer) {
		s.card = strings.Replace(s.card, "END:VCARD", "NOTE:из iPhone\r\nEND:VCARD", 1)
		s.etag++
	}
	srv := httptest.NewServer(f)
	defer srv.Close()

	got, err := connectTo(t, srv.URL).PatchContact(context.Background(), "/book/x.vcf", renameTo("Иван Сидоров"))
	if err != nil {
		t.Fatal(err)
	}
	if f.puts != 2 {
		t.Errorf("PUT'ов %d, ждали 2 (412 и повтор)", f.puts)
	}
	for _, want := range []string{"FN:Иван Сидоров\r\n", "N:Сидоров;Иван;;;\r\n", "NOTE:из iPhone\r\n"} {
		if !strings.Contains(f.card, want) {
			t.Errorf("на сервере нет %q:\n%s", want, f.card)
		}
	}
	if got != f.card {
		t.Errorf("возвращено не то, что на сервере")
	}
}

func TestPatchContact_GoneAndForbidden(t *testing.T) {
	f := &fakeCardServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := connectTo(t, srv.URL)
	if _, err := c.PatchContact(context.Background(), "/book/x.vcf", renameTo("x")); !errors.Is(err, ErrRemoteGone) {
		t.Errorf("нет карточки: err = %v", err)
	}
	f.card = "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:a\r\nEND:VCARD\r\n"
	f.forbid = true
	if _, err := c.PatchContact(context.Background(), "/book/x.vcf", renameTo("b")); !errors.Is(err, ErrForbidden) {
		t.Errorf("403: err = %v", err)
	}
}

func TestPatchContact_NothingToChangeNoPut(t *testing.T) {
	f := &fakeCardServer{card: "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:a\r\nEND:VCARD\r\n", etag: 1}
	srv := httptest.NewServer(f)
	defer srv.Close()
	if _, err := connectTo(t, srv.URL).PatchContact(context.Background(), "/book/x.vcf", renameTo("a")); err != nil {
		t.Fatal(err)
	}
	if f.puts != 0 {
		t.Errorf("PUT без изменений")
	}
}

func TestBookWritable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" || r.Header.Get("Depth") != "0" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusMultiStatus)
		if strings.Contains(r.URL.Path, "small.kz") {
			_, _ = io.WriteString(w, sogoReadOnlyPrivileges)
			return
		}
		_, _ = io.WriteString(w, icloudWritablePrivileges)
	}))
	defer srv.Close()
	c := connectTo(t, srv.URL)
	if w, k := c.BookWritable(context.Background(), "/SOGo/dav/u/Contacts/small.kz/"); w || !k {
		t.Errorf("GAL: writable=%v known=%v", w, k)
	}
	if w, k := c.BookWritable(context.Background(), "/123/carddavhome/card/"); !w || !k {
		t.Errorf("iCloud: writable=%v known=%v", w, k)
	}
}
