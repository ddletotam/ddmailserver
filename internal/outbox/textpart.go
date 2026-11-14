package outbox

import (
	"mime/quotedprintable"
	"strings"
)

// maxPlainLine — сколько октетов строки тела можно оставить как есть
// (RFC 5322 §2.1.1 рекомендует 78). Длиннее — тело уходит в
// quoted-printable, иначе ретранслятор вправе переломить строку, и подпись
// DKIM ломается.
const maxPlainLine = 78

// WriteTextPart пишет заголовки текстовой части (text/plain, text/html) и её
// тело, 7-битно чистое.
//
// Раньше тело кириллицей уходило сырыми 8-битными байтами без
// Content-Transfer-Encoding — по RFC 2045 это 7bit, то есть нарушение
// формата; Mail.ru и спам-фильтры за такое штрафуют, а SMTP без 8BITMIME
// вправе письмо отвергнуть (аудит 2026-11-14, №6). Теперь тело с не-ASCII или
// длинными строками кодируется quoted-printable; чистый короткий ASCII
// остаётся как есть. Переводы строк — CRLF.
//
// Пишет заголовок Content-Type, при необходимости
// Content-Transfer-Encoding, пустую строку и тело с завершающим CRLF.
func WriteTextPart(w *strings.Builder, mediaType, body string) {
	w.WriteString("Content-Type: " + mediaType + "; charset=utf-8\r\n")
	if !needsEncoding(body) {
		w.WriteString("\r\n")
		w.WriteString(toCRLF(body))
		w.WriteString("\r\n")
		return
	}
	w.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	qp := quotedprintable.NewWriter(w)
	_, _ = qp.Write([]byte(body))
	_ = qp.Close()
	w.WriteString("\r\n")
}

// needsEncoding — есть ли в теле не-ASCII, управляющие символы кроме
// перевода строки и табуляции, или строки длиннее maxPlainLine.
func needsEncoding(body string) bool {
	line := 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c == '\n':
			line = 0
			continue
		case c == '\r':
			if i+1 < len(body) && body[i+1] == '\n' {
				continue
			}
			return true
		case c == '\t':
		case c < 0x20 || c >= 0x7f:
			return true
		}
		line++
		if line > maxPlainLine {
			return true
		}
	}
	// Строка, начинающаяся с точки или «From », не беда: точку экранирует
	// SMTP, «From » — проблема mbox, не наша.
	return false
}

// toCRLF приводит голые LF к CRLF.
func toCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}
