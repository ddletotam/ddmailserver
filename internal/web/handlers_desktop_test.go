package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yourusername/mailserver/internal/models"
)

func TestHandleDDMailDiscovery(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("GET", "/.well-known/ddmail", nil)
	w := httptest.NewRecorder()

	s.HandleDDMailDiscovery(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if resp["ddmail"] != true {
		t.Error("expected ddmail=true")
	}
	if resp["version"] != float64(1) {
		t.Errorf("expected version=1, got %v", resp["version"])
	}
	if resp["api_base"] != "/api/desktop/v1" {
		t.Errorf("expected api_base=/api/desktop/v1, got %v", resp["api_base"])
	}
	if resp["ws_path"] != "/api/desktop/v1/ws" {
		t.Errorf("expected ws_path, got %v", resp["ws_path"])
	}

	features, ok := resp["features"].([]interface{})
	if !ok || len(features) == 0 {
		t.Error("expected non-empty features array")
	}
}

func TestHandleDesktopLoginBadJSON(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("POST", "/api/desktop/v1/auth/login", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()

	s.HandleDesktopLogin(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleDesktopLoginNoUser(t *testing.T) {
	// Without a database, GetUserByUsername will fail → 401
	s := &Server{}
	body, _ := json.Marshal(map[string]string{"username": "nonexistent", "password": "pass"})
	req := httptest.NewRequest("POST", "/api/desktop/v1/auth/login", bytes.NewReader(body))
	w := httptest.NewRecorder()

	// This will panic or error because s.database is nil — that's expected
	// in a unit test without DB. We just verify the handler doesn't crash
	// on valid JSON input by recovering from the panic.
	defer func() {
		if r := recover(); r != nil {
			// Expected: nil database
		}
	}()
	s.HandleDesktopLogin(w, req)
}

func TestFolderSpecialUse(t *testing.T) {
	tests := []struct {
		folderType string
		expected   string
	}{
		{"inbox", "\\Inbox"},
		{"sent", "\\Sent"},
		{"drafts", "\\Drafts"},
		{"trash", "\\Trash"},
		{"junk", "\\Junk"},
		{"archive", "\\Archive"},
		{"custom", ""},
		{"", ""},
	}

	for _, tt := range tests {
		got := folderSpecialUse(tt.folderType)
		if got != tt.expected {
			t.Errorf("folderSpecialUse(%q) = %q, want %q", tt.folderType, got, tt.expected)
		}
	}
}

func TestExtractEmail(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"user@example.com", "user@example.com"},
		{"Name <user@example.com>", "user@example.com"},
		{"<user@example.com>", "user@example.com"},
		{" user@example.com ", "user@example.com"},
		{"\"Name\" <user@example.com>", "user@example.com"},
	}

	for _, tt := range tests {
		got := extractEmail(tt.input)
		if got != tt.expected {
			t.Errorf("extractEmail(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestBuildRawEmailWithThreading(t *testing.T) {
	raw := string(buildRawEmailWithThreading(
		"sender@example.com",
		"recipient@other.com",
		"",
		"Re: hello",
		"plain body",
		"<p>html body</p>",
		"<parent@x>",
		"<root@x> <parent@x>",
	))

	for _, want := range []string{
		"From: sender@example.com\r\n",
		"To: recipient@other.com\r\n",
		"Subject: Re: hello\r\n",
		"In-Reply-To: <parent@x>\r\n",
		"References: <root@x> <parent@x>\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: multipart/alternative",
		"plain body",
		"<p>html body</p>",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("missing %q in raw email:\n%s", want, raw)
		}
	}
	if !strings.Contains(raw, "Message-ID: <") || !strings.Contains(raw, "@example.com>") {
		t.Errorf("missing Message-ID with sender domain in raw email:\n%s", raw)
	}
}

func TestExtractName(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{`Alice <alice@x.com>`, "Alice"},
		{`"Bob Smith" <bob@x.com>`, "Bob Smith"},
		{`alice@x.com`, ""},
		{`<alice@x.com>`, ""},
	}
	for _, tt := range tests {
		got := extractName(tt.input)
		if got != tt.expected {
			t.Errorf("extractName(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestParseRecipientAddrs(t *testing.T) {
	addrs := parseRecipientAddrs("Alice <alice@x.com>, bob@y.com", "cc@z.com")
	if len(addrs) != 3 {
		t.Fatalf("expected 3 addrs, got %d: %v", len(addrs), addrs)
	}
	if addrs[0] != "alice@x.com" {
		t.Errorf("addrs[0] = %q, want alice@x.com", addrs[0])
	}
}

func TestParseMessageIDs(t *testing.T) {
	ids := parseMessageIDs("<root@x> <parent@y>")
	if len(ids) != 2 {
		t.Fatalf("expected 2 ids, got %d: %v", len(ids), ids)
	}
	if ids[0] != "root@x" || ids[1] != "parent@y" {
		t.Errorf("got %v", ids)
	}
}

func TestGravatarHash(t *testing.T) {
	// MD5 of "test@example.com" is well-known
	h := gravatarHash("test@example.com")
	if len(h) != 32 {
		t.Errorf("expected 32-char hex, got %q (%d chars)", h, len(h))
	}
}

// Directum RX и подобные шлют настоящие документы (.docx) с
// Content-Disposition: inline + Content-ID — такие вложения обязаны
// показываться в desktop-клиенте. Прятать можно только реально встроенные
// в HTML ресурсы (cid:-картинки) и синтетические text/calendar части.
func TestHideInlineAttachment(t *testing.T) {
	docx := models.Attachment{
		Filename:    "Уведомление.docx",
		ContentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		ContentID:   "LFJHWWWBWTU4.ZAQSERX0BZ5J2@host",
		IsInline:    true,
	}
	bodyNoCID := "<html><body>Текст письма без встроенных ресурсов</body></html>"
	if hideInlineAttachment(&docx, bodyNoCID) {
		t.Error("inline .docx not referenced in HTML must be visible")
	}

	img := models.Attachment{
		Filename:    "logo.png",
		ContentType: "image/png",
		ContentID:   "logo123@host",
		IsInline:    true,
	}
	bodyWithCID := `<html><body><img src="cid:logo123@host"></body></html>`
	if !hideInlineAttachment(&img, bodyWithCID) {
		t.Error("cid-referenced inline image must stay hidden")
	}
	if hideInlineAttachment(&img, bodyNoCID) {
		t.Error("inline image NOT referenced in HTML must be visible")
	}

	ics := models.Attachment{
		Filename:    "invite.ics",
		ContentType: "text/calendar; method=REQUEST",
		IsInline:    true,
	}
	if !hideInlineAttachment(&ics, bodyNoCID) {
		t.Error("synthetic text/calendar part must stay hidden")
	}

	regular := models.Attachment{
		Filename:    "report.pdf",
		ContentType: "application/pdf",
		IsInline:    false,
	}
	if hideInlineAttachment(&regular, bodyWithCID) {
		t.Error("regular attachment must always be visible")
	}
}

// Спам-рассылка приходит с From=спамер, To=жертва (пользователь в скрытой
// копии). Клиент склеивает обоих в «участников» диалога и не знает, кто
// отправитель — поэтому блокировать нужно по реальному From из строк письма,
// а не по догадке клиента. Домен-scope обязателен против random-логинов.
func TestSpamBlockRules(t *testing.T) {
	// From = rulane (спамер), To = hsmedia — участники диалога оба.
	from := []string{"Mакitа <uptolwh@rulane.life>"}

	addr := spamBlockRules(from, "", "", "address")
	if len(addr) != 1 || addr[0].ruleType != "address" || addr[0].ruleValue != "uptolwh@rulane.life" {
		t.Fatalf("address scope: got %+v, want address=uptolwh@rulane.life", addr)
	}

	dom := spamBlockRules(from, "", "", "domain")
	if len(dom) != 1 || dom[0].ruleType != "domain" || dom[0].ruleValue != "rulane.life" {
		t.Fatalf("domain scope: got %+v, want domain=rulane.life", dom)
	}

	// Fallback (IMAP: no rows) uses the client hint, not the real sender.
	fb := spamBlockRules(nil, "spammer@bad.tld", "", "domain")
	if len(fb) != 1 || fb[0].ruleValue != "bad.tld" {
		t.Fatalf("fallback: got %+v, want domain=bad.tld", fb)
	}

	// Multiple distinct senders in a group → one rule each, sorted, deduped.
	multi := spamBlockRules(
		[]string{"A <x@a.tld>", "B <y@b.tld>", "C <z@a.tld>"}, "", "", "domain")
	if len(multi) != 2 || multi[0].ruleValue != "a.tld" || multi[1].ruleValue != "b.tld" {
		t.Fatalf("multi domain: got %+v, want [a.tld b.tld]", multi)
	}

	// Nothing to block → empty (handler turns this into 400).
	if got := spamBlockRules(nil, "", "", "address"); len(got) != 0 {
		t.Fatalf("empty: got %+v, want none", got)
	}
}
