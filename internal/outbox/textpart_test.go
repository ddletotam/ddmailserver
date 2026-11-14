package outbox

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/models"
)

func is7bit(t *testing.T, raw []byte) {
	t.Helper()
	for i, line := range bytes.Split(raw, []byte("\r\n")) {
		if len(line) > 998 {
			t.Errorf("строка %d длиной %d", i, len(line))
		}
		for _, c := range line {
			if c >= 0x80 || c == '\n' || c == '\r' {
				t.Fatalf("строка %d не 7-битная: %q", i, line)
			}
		}
	}
}

func TestWriteTextPart(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		encoded    bool
	}{
		{"ascii", "plain body\nsecond line", false},
		{"кириллица", "Аудит ipad imap 070057", true},
		{"длинная строка", strings.Repeat("a", 200), true},
		{"голый CR", "a\rb", true},
	} {
		var b strings.Builder
		WriteTextPart(&b, "text/plain", tc.body)
		out := b.String()
		is7bit(t, []byte(out))
		hasCTE := strings.Contains(out, "Content-Transfer-Encoding: quoted-printable\r\n")
		if hasCTE != tc.encoded {
			t.Errorf("%s: quoted-printable=%v, ждали %v:\n%s", tc.name, hasCTE, tc.encoded, out)
		}
	}
}

// Письмо из аудита 2026-11-14 (Mail.ru ответил 550 spam): тело кириллицей
// уходило сырыми 8-битными байтами без Content-Transfer-Encoding. Теперь
// письмо 7-битное, а разобранное обратно — то же самое.
func TestComposeIs7bitAndRoundTrips(t *testing.T) {
	msg := &models.OutboxMessage{
		From:     `"lucky" <info@help-it-audit.ru>`,
		To:       "denis.danilin@internet.ru",
		Subject:  "Аудит 2026-11-14 ipad imap 070057",
		Body:     "Аудит ipad imap 070057\nвторая строка " + strings.Repeat("длинно ", 30),
		BodyHTML: "<div>Аудит ipad imap 070057</div>",
	}
	for _, atts := range [][]*models.OutboxAttachment{nil, {{Filename: "a.txt", ContentType: "text/plain", Data: []byte("x")}}} {
		raw := Compose(msg, atts, time.Unix(1794668489, 0))
		is7bit(t, raw)

		m, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		texts := collectTexts(t, m.Header.Get("Content-Type"), m.Body)
		if texts["text/plain"] != strings.ReplaceAll(msg.Body, "\n", "\r\n") {
			t.Errorf("text/plain: %q", texts["text/plain"])
		}
		if texts["text/html"] != msg.BodyHTML {
			t.Errorf("text/html: %q", texts["text/html"])
		}
	}
}

// collectTexts разбирает MIME-дерево и возвращает декодированные текстовые
// части (без завершающего CRLF перед границей).
func collectTexts(t *testing.T, contentType string, body io.Reader) map[string]string {
	t.Helper()
	out := map[string]string{}
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(mt, "multipart/") {
		b, _ := io.ReadAll(body)
		out[mt] = string(b)
		return out
	}
	mr := multipart.NewReader(body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		// multipart.Reader сам снимает quoted-printable.
		for k, v := range collectTexts(t, p.Header.Get("Content-Type"), p) {
			if _, ok := out[k]; !ok {
				out[k] = strings.TrimSuffix(v, "\r\n")
			}
		}
	}
}
