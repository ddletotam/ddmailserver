package lists

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func TestIssueKeepsContentAddsUnsubscribe(t *testing.T) {
	l := Letter{ListAddr: "news@example.com", ListTitle: "Новости", To: "sub@example.org", Token: "tok123456789abcdef",
		BaseURL: "https://mail.lunlun.email", BotName: "robotohobot", Now: time.Unix(0, 0)}
	owner := "From: Owner <owner@example.com>\r\nTo: news@example.com\r\nBcc: secret@example.com\r\nSubject: =?utf-8?b?0J/RgNC40LLQtdGC?=\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\nПривет всем\r\n"
	out, err := Issue(l, []byte(owner))
	if err != nil {
		t.Fatal(err)
	}
	m, err := mail.ReadMessage(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	h := m.Header
	if got := h.Get("List-Unsubscribe"); got != "<https://mail.lunlun.email/l/u/tok123456789abcdef>" {
		t.Fatalf("List-Unsubscribe %q", got)
	}
	if h.Get("List-Unsubscribe-Post") != "List-Unsubscribe=One-Click" || h.Get("Reply-To") != "Owner <owner@example.com>" ||
		h.Get("To") != "<sub@example.org>" || h.Get("Bcc") != "" || !strings.Contains(h.Get("From"), "news@example.com") {
		t.Fatalf("headers: %v", h)
	}
	if !strings.Contains(h.Get("List-Id"), "<news.example.com>") {
		t.Fatalf("List-Id %q", h.Get("List-Id"))
	}
	_, params, _ := mime.ParseMediaType(h.Get("Content-Type"))
	r := multipart.NewReader(m.Body, params["boundary"])
	p1, err := r.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := io.ReadAll(p1)
	if !strings.Contains(string(b1), "Привет всем") {
		t.Fatalf("content: %q", b1)
	}
	p2, err := r.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(p2)
	if !strings.Contains(string(b2), "https://mail.lunlun.email/l/u/tok123456789abcdef") {
		t.Fatalf("footer: %q", b2)
	}
}

func TestConfirmationHasLink(t *testing.T) {
	l := Letter{ListAddr: "news@example.com", ListTitle: "Новости", To: "sub@example.org", Token: "T0K",
		BaseURL: "https://mail.lunlun.email", BotName: "robotohobot", Now: time.Unix(0, 0)}
	m, err := mail.ReadMessage(bytes.NewReader(Confirmation(l)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(m.Body)
	if !strings.Contains(string(body), "https://mail.lunlun.email/l/c/T0K") {
		t.Fatalf("body %q", body)
	}
}
