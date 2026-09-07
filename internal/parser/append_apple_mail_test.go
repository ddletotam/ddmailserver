package parser

import (
	"strings"
	"testing"
)

// Письмо в форме, которую кладёт Apple Mail: multipart/mixed, внутри крошечный
// HTML и вложение. Именно такие письма приезжают к нам ТОЛЬКО через IMAP
// APPEND (клиент отправляет через чужой SMTP, а копию в «Отправленные» пишет
// нам), поэтому если парсер их не разбирает, вложение теряется безвозвратно —
// второго источника у нас нет.
func TestParseAppleMailAppendWithAttachment(t *testing.T) {
	raw := strings.ReplaceAll(`From: Denis <info@example.org>
To: someone@example.com
Subject: Re: invoice
Message-ID: <E5DFF90D-5351-4F3A-B455-ECBFF0578CE5@example.org>
In-Reply-To: <c67fece84b614b728f09444be95856c2@example.com>
References: <a1@example.com> <c67fece84b614b728f09444be95856c2@example.com>
Content-Type: multipart/mixed; boundary="Apple-Mail-42"
MIME-Version: 1.0

--Apple-Mail-42
Content-Type: text/html; charset=utf-8
Content-Transfer-Encoding: quoted-printable

<html><body dir=3D"auto">=D0=B2=D0=BE=D1=82 =D0=BF=D0=BB=D0=B0=D1=82=D0=B5=
=D0=B6=D0=BA=D0=B0</body></html>
--Apple-Mail-42
Content-Disposition: attachment; filename=payment.pdf
Content-Type: application/pdf; x-unix-mode=0644; name="payment.pdf"
Content-Transfer-Encoding: base64

JVBERi0xLjQKJcOkw7zDtsOfCjEgMCBvYmoKPDwvVHlwZS9DYXRhbG9nPj4KZW5kb2JqCg==
--Apple-Mail-42--
`, "\n", "\r\n")

	parsed, err := New().ParseBytes([]byte(raw))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}

	if len(parsed.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(parsed.Attachments))
	}
	att := parsed.Attachments[0]
	if att.Filename != "payment.pdf" {
		t.Errorf("filename = %q, want payment.pdf", att.Filename)
	}
	if !strings.HasPrefix(att.ContentType, "application/pdf") {
		t.Errorf("content type = %q, want application/pdf", att.ContentType)
	}
	// Декодированное содержимое, а не base64: в базу кладётся именно оно.
	if !strings.HasPrefix(string(att.Data), "%PDF-") {
		t.Errorf("data starts with %q, want %%PDF-", string(att.Data[:min(5, len(att.Data))]))
	}
	if att.Size == 0 {
		t.Error("size = 0, want the decoded length")
	}

	// Заголовки цепочки: без References ответ теряет связь с тем, на что отвечает.
	if len(parsed.References) != 2 {
		t.Errorf("references = %v, want 2 ids", parsed.References)
	}
	if parsed.InReplyTo == "" {
		t.Error("in-reply-to is empty")
	}
	if parsed.BodyHTML == "" {
		t.Error("html body is empty")
	}
}

// Настоящая форма письма с iPhone (снято с потерянного письма от 05.09.2026):
// multipart/alternative с ОДНИМ ребёнком multipart/mixed, внутри — текст
// ответа, вложение и цитата. Вложение помечено `Content-Disposition: inline`
// и БЕЗ Content-ID: go-message из-за диспозиции считает такую часть
// «текстовой», и разбор её терял целиком.
func TestParseIPhoneInlineAttachmentWithoutContentID(t *testing.T) {
	raw := strings.ReplaceAll(`From: Denis <info@example.org>
To: someone@example.com
Subject: Re: invoice
Message-Id: <E5DFF90D@example.org>
References: <c67fece84@example.com>
In-Reply-To: <c67fece84@example.com>
X-Mailer: iPhone Mail (23G83)
Content-Type: multipart/alternative; boundary=Apple-Mail-55D9
MIME-Version: 1.0

--Apple-Mail-55D9
Content-Type: multipart/mixed; boundary=Apple-Mail-AFA6
Content-Transfer-Encoding: 7bit

--Apple-Mail-AFA6
Content-Type: text/html; charset=utf-8
Content-Transfer-Encoding: quoted-printable

<html><body dir=3D"auto">payment attached</body></html>
--Apple-Mail-AFA6
Content-Type: application/pdf; x-unix-mode=0644; name="payment.pdf"
Content-Disposition: inline; filename="payment.pdf"
Content-Transfer-Encoding: base64

JVBERi0xLjQKJcOkw7zDtsOfCjEgMCBvYmoKPDwvVHlwZS9DYXRhbG9nPj4KZW5kb2JqCg==
--Apple-Mail-AFA6
Content-Type: text/html; charset=utf-8
Content-Transfer-Encoding: quoted-printable

<html><body>&gt; quoted original</body></html>
--Apple-Mail-AFA6--
--Apple-Mail-55D9--
`, "\n", "\r\n")

	parsed, err := New().ParseBytes([]byte(raw))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}

	if len(parsed.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1 (inline PDF without Content-ID)", len(parsed.Attachments))
	}
	att := parsed.Attachments[0]
	if att.Filename != "payment.pdf" {
		t.Errorf("filename = %q, want payment.pdf", att.Filename)
	}
	if !strings.HasPrefix(string(att.Data), "%PDF-") {
		t.Errorf("data not decoded: %q", string(att.Data[:min(5, len(att.Data))]))
	}
	// Content-ID нет → файл не «встроен в тело», и прятать его из списка
	// вложений нельзя (иначе он снова невидим, только теперь уже в API).
	if att.IsInline {
		t.Error("IsInline = true без Content-ID: файл спрячется из списка вложений")
	}
	if att.ContentID != "" {
		t.Errorf("content-id = %q, want empty", att.ContentID)
	}
	if parsed.BodyHTML == "" {
		t.Error("html body is empty")
	}
}

// Встроенная картинка (Content-ID есть) остаётся встроенной: от IsInline
// зависит, прячет ли её API из списка вложений при ссылке из HTML.
func TestParseInlineImageKeepsContentID(t *testing.T) {
	raw := strings.ReplaceAll(`From: a@example.org
To: b@example.com
Subject: pic
Content-Type: multipart/related; boundary=B1
MIME-Version: 1.0

--B1
Content-Type: text/html; charset=utf-8

<html><body><img src="cid:logo@x"></body></html>
--B1
Content-Type: image/png
Content-ID: <logo@x>
Content-Disposition: inline
Content-Transfer-Encoding: base64

iVBORw0KGgo=
--B1--
`, "\n", "\r\n")

	parsed, err := New().ParseBytes([]byte(raw))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if len(parsed.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(parsed.Attachments))
	}
	att := parsed.Attachments[0]
	if !att.IsInline || att.ContentID != "logo@x" {
		t.Errorf("inline=%v cid=%q, want true/logo@x", att.IsInline, att.ContentID)
	}
	if att.Filename != "logo@x.png" {
		t.Errorf("filename = %q, want logo@x.png", att.Filename)
	}
}
