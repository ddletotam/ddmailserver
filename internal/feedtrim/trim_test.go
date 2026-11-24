package feedtrim

import (
	"strings"
	"testing"
	"time"
)

// Cases are shaped after real threads from the dry run (2026-11-24), with
// names, addresses and text replaced.

var msk = time.FixedZone("MSK", 3*3600)

const parentHTML = `<div>Привет!</div><div>Я приеду на машине. Соответственно, чеки с меня.</div>`

func parent() Ancestor {
	return Ancestor{
		From:    `"Иван Петров" <ivan@example.org>`,
		Subject: "Командировка 3 декабря",
		Date:    time.Date(2026, 11, 24, 11, 10, 32, 0, msk),
		Body:    ParseHTML(parentHTML),
	}
}

// Yandex 360: separator, «Кому/Тема/дата, отправитель:» and a blockquote.
const yandexReply = `<div>Командировку оформила. Будем ждать.</div><div><div>Спасибо,</div><div>Мария.</div></div>
<div>--</div><div><table><tr><td><b>Мария Сидорова</b><br>Административный менеджер<br>+7 000 000 0000</td></tr>
<tr><td>Это сообщение, включая любые вложения, является конфиденциальной информацией.</td></tr></table></div>
<div>----------------</div>
<div>Кому: maria@example.org (maria@example.org);</div>
<div>Тема: Командировка 3 декабря;</div>
<div>24.11.2026, 11:10, "ivan@example.org" &lt;ivan@example.org&gt;:</div>
<blockquote><div>Привет!</div><div>Я приеду на машине. Соответственно, чеки с меня.</div></blockquote>`

func lineIndex(t *testing.T, b *Body, text string) int {
	t.Helper()
	for i, l := range b.Lines {
		if l.Text == text {
			return i
		}
	}
	t.Fatalf("no line %q", text)
	return -1
}

func TestHistory_yandexHeaderAndQuote(t *testing.T) {
	b := ParseHTML(yandexReply)
	r := Trim(b, "", []Ancestor{parent()}, nil)
	if want := lineIndex(t, b, "----------------"); r.History != want {
		t.Fatalf("history cut at %d, want %d (separator)", r.History, want)
	}
	out := b.CutAt(r.History)
	if strings.Contains(out, "blockquote") || strings.Contains(out, "Кому:") || strings.Contains(out, "------") {
		t.Fatalf("quote left behind: %s", out)
	}
	if !strings.Contains(out, "Командировку оформила") || !strings.Contains(out, "Мария.") {
		t.Fatalf("own text lost: %s", out)
	}
}

func TestHistory_noAncestorNoCut(t *testing.T) {
	r := Trim(ParseHTML(yandexReply), "", nil, nil)
	if r.History != -1 {
		t.Fatalf("cut without ancestor at %d", r.History)
	}
}

func TestHistory_unrelatedAncestorNoCut(t *testing.T) {
	a := parent()
	a.Body = ParseHTML(`<div>Совсем другое письмо про отчёт за квартал и сроки сдачи.</div>`)
	if r := Trim(ParseHTML(yandexReply), "", []Ancestor{a}, nil); r.History != -1 {
		t.Fatalf("cut against an unrelated ancestor at %d", r.History)
	}
}

// Short own reply whose words occur in the quote stays.
func TestHistory_shortReplyStays(t *testing.T) {
	b := ParseHTML(`<div>Привет!</div><div>27.07.2026 11:10, Иван Петров пишет:</div>` +
		`<blockquote><div>Привет!</div><div>Я приеду на машине. Соответственно, чеки с меня.</div></blockquote>`)
	r := Trim(b, "", []Ancestor{parent()}, nil)
	if r.History != 1 {
		t.Fatalf("history cut at %d, want 1 (header)", r.History)
	}
	if out := b.CutAt(r.History); !strings.Contains(out, "Привет!") || strings.Contains(out, "пишет") {
		t.Fatalf("got %s", out)
	}
}

// Answers between quoted fragments: the quote is part of the answer.
func TestHistory_interleavedStays(t *testing.T) {
	b := ParseHTML(`<blockquote>Я приеду на машине. Соответственно, чеки с меня.</blockquote>` +
		`<div>Хорошо, тогда парковку бронируем заранее, напиши номер машины.</div>`)
	if r := Trim(b, "", []Ancestor{parent()}, nil); r.History != -1 {
		t.Fatalf("interleaved reply cut at %d", r.History)
	}
}

// Nothing of the sender's own text: keep the message as it is.
func TestHistory_onlyQuoteStays(t *testing.T) {
	b := ParseHTML(`<blockquote><div>Привет!</div><div>Я приеду на машине. Соответственно, чеки с меня.</div></blockquote>`)
	if r := Trim(b, "", []Ancestor{parent()}, nil); r.History != -1 {
		t.Fatalf("cut everything at %d", r.History)
	}
}

// Outlook: From/Sent/To/Subject block, gateway warning, the quote.
func TestHistory_outlookHeader(t *testing.T) {
	b := ParseHTML(`<p>Согласовано, отправляю договор.</p>` +
		`<div style="border-top:solid 1pt">` +
		`<p><b>From:</b> Иван Петров &lt;ivan@example.org&gt;<br><b>Sent:</b> Tuesday, November 24, 2026 8:10 AM<br>` +
		`<b>To:</b> Мария<br><b>Subject:</b> RE: Командировка 3 декабря</p></div>` +
		`<p>Внимание! Сообщение получено от внешнего отправителя.</p>` +
		`<p>Привет!</p><p>Я приеду на машине. Соответственно, чеки с меня.</p>`)
	r := Trim(b, "", []Ancestor{parent()}, nil)
	if r.History < 0 {
		t.Fatal("no cut")
	}
	out := b.CutAt(r.History)
	if strings.Contains(out, "From:") || strings.Contains(out, "Внимание") || strings.Contains(out, "приеду") {
		t.Fatalf("header or quote left: %s", out)
	}
	if !strings.Contains(out, "Согласовано") {
		t.Fatalf("own text lost: %s", out)
	}
}

func TestHistory_plainText(t *testing.T) {
	b := ParseText("Ок, договорились.\n\nВ Вт, 24 нояб. 2026 г. в 11:10, Иван Петров <ivan@example.org>:\n> Привет!\n> Я приеду на машине. Соответственно,\n> чеки с меня.\n")
	r := Trim(b, "", []Ancestor{parent()}, nil)
	if r.History != 1 {
		t.Fatalf("history cut at %d, want 1", r.History)
	}
	if got := b.CutAt(r.History); got != "Ок, договорились." {
		t.Fatalf("got %q", got)
	}
}

// Nested history (reply to a reply) goes in one cut.
func TestHistory_nested(t *testing.T) {
	mid := Ancestor{
		From: `"Мария Сидорова" <maria@example.org>`, Subject: "Re: Командировка 3 декабря",
		Date: time.Date(2026, 11, 24, 11, 30, 0, 0, msk),
		Body: ParseHTML(`<div>Принято.</div><div>----------------</div><div>24.11.2026, 11:10, "ivan@example.org":</div>` +
			`<blockquote>` + parentHTML + `</blockquote>`),
	}
	b := ParseHTML(`<div>И ещё вопрос: нужна ли парковка у гостиницы?</div><div>----------------</div>` +
		`<div>24.11.2026, 11:30, "Мария Сидорова" &lt;maria@example.org&gt;:</div>` +
		`<blockquote><div>Принято.</div><div>----------------</div><div>24.11.2026, 11:10, "ivan@example.org":</div>` +
		`<blockquote>` + parentHTML + `</blockquote></blockquote>`)
	r := Trim(b, "", []Ancestor{mid, parent()}, nil)
	if r.History != 1 {
		t.Fatalf("history cut at %d, want 1", r.History)
	}
	if out := b.CutAt(1); strings.Contains(out, "Принято") || strings.Contains(out, "приеду") {
		t.Fatalf("nested quote left: %s", out)
	}
}

func peer(t *testing.T, subject, html string) Peer {
	t.Helper()
	return Peer{Subject: subject, Own: Trim(ParseHTML(html), subject, nil, nil).Own}
}

const sigBlock = `<div>--</div><div>Мария Сидорова</div><div>Административный менеджер</div><div>Это сообщение конфиденциально.</div>`

func TestSignature_repeatedWithDelimiter(t *testing.T) {
	peers := []Peer{
		peer(t, "Билеты", `<div>Билеты купила.</div><div>Мария.</div>`+sigBlock),
		peer(t, "Гостиница", `<div>Гостиница подтверждена.</div><div>Мария.</div>`+sigBlock),
	}
	b := ParseHTML(`<div>Командировку оформила.</div><div>Мария.</div>` + sigBlock)
	r := Trim(b, "Re: Командировка", nil, peers)
	if want := lineIndex(t, b, "--"); r.Signature != want {
		t.Fatalf("signature at %d, want %d", r.Signature, want)
	}
	if out := b.CutAt(r.Cut()); strings.Contains(out, "менеджер") || !strings.Contains(out, "Мария.") {
		t.Fatalf("got %s", out)
	}
}

// Robot footer without a delimiter, present in most of the sender's threads.
// Updates of one meeting repeat its date but count as one thread.
func TestSignature_repeatedFooter(t *testing.T) {
	foot := `<p>Вы получили это письмо, потому что у вас включены уведомления.</p><p>Настроить уведомления</p>`
	peers := []Peer{
		peer(t, "Встреча А", `<p>Встреча А</p><p>Дата начала: 07.08.2026 15:00</p>`+foot),
		peer(t, "Встреча Б", `<p>Встреча Б</p><p>Дата начала: 08.08.2026 10:00</p>`+foot),
		peer(t, "Встреча В", `<p>Встреча В</p><p>Дата начала: 09.08.2026 12:00</p>`+foot),
		peer(t, "Встреча Г", `<p>Встреча Г</p><p>Дата начала: 07.08.2026 15:00</p>`+foot),
		peer(t, "Re: Встреча Г", `<p>Встреча Г</p><p>Дата начала: 07.08.2026 15:00</p>`+foot),
	}
	b := ParseHTML(`<p>Встреча Г</p><p>Дата начала: 07.08.2026 15:00</p>` + foot)
	r := Trim(b, "Встреча Г", nil, peers)
	if r.Signature != 2 {
		t.Fatalf("signature at %d, want 2 (footer only, the date stays)", r.Signature)
	}
}

// Two messages that happen to end alike are not a signature.
func TestSignature_needsMajorityOfThree(t *testing.T) {
	peers := []Peer{peer(t, "Встреча А", `<p>Встреча А</p><p>Дата начала: 07.08.2026 15:00</p><p>Описание: планёрка</p>`)}
	b := ParseHTML(`<p>Встреча А, перенос</p><p>Дата начала: 07.08.2026 15:00</p><p>Описание: планёрка</p>`)
	if r := Trim(b, "Встреча Б", nil, peers); r.Signature != -1 {
		t.Fatalf("signature at %d", r.Signature)
	}
}

func TestSignature_firstMessageDelimiterOnly(t *testing.T) {
	b := ParseText("Добрый день, пришлите реестр.\n-- \nМария Сидорова\n+7 000 000 0000\n")
	r := Trim(b, "", nil, nil)
	if r.Signature != 1 {
		t.Fatalf("signature at %d, want 1", r.Signature)
	}
	if got := b.CutAt(1); got != "Добрый день, пришлите реестр." {
		t.Fatalf("got %q", got)
	}
}

func TestSignature_nothingElseStays(t *testing.T) {
	b := ParseText("--\nМария Сидорова\n")
	if r := Trim(b, "", nil, nil); r.Cut() != -1 {
		t.Fatalf("cut at %d", r.Cut())
	}
}

func TestCutAt_doesNotMutateBody(t *testing.T) {
	b := ParseHTML(yandexReply)
	r := Trim(b, "", []Ancestor{parent()}, nil)
	first := b.CutAt(r.History)
	if second := b.CutAt(r.History); second != first {
		t.Fatal("second cut differs: CutAt changed the parsed body")
	}
	if b.CutAt(0) == first {
		t.Fatal("cut at 0 equals cut at history")
	}
}
