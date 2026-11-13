// Package vcardedit правит vCard по строкам: меняет только свойства, которые
// правит клиент (FN/N, EMAIL, TEL, ORG, TITLE, REV), а всё остальное
// оставляет байт в байт — порядок, свёртку строк, группы, параметры.
//
// Разбор и пересборка (go-vcard) для правки не годятся: пересборка сортирует
// свойства, снимает свёртку, а строки, которых разборщик не понял, молча
// выбрасывает. Карточка iCloud несёт фото, дни рождения, адреса, заметки,
// X-свойства Apple, связанные через группы item1.… подписи — правка имени не
// должна ничего из этого трогать. Тот же приём, что у задач
// (caldav.SetTodoFields) и у событий десктопа (merge_ical).
package vcardedit

import (
	"errors"
	"strings"
	"time"
)

// Patch — правка контакта. nil-поле не трогается; присланное значение,
// совпадающее с тем, что уже в карточке, тоже ничего не меняет.
//
// Поля сериализуются в contact_sync_queue.patch: воркер обратной
// синхронизации применяет ту же правку к свежей карточке с сервера.
type Patch struct {
	FullName     *string   `json:"full_name,omitempty"`
	Emails       *[]string `json:"emails,omitempty"`
	Phones       *[]string `json:"phones,omitempty"`
	Organization *string   `json:"organization,omitempty"`
	Title        *string   `json:"title,omitempty"`
}

// Empty — в правке нет ни одного поля.
func (p Patch) Empty() bool {
	return p.FullName == nil && p.Emails == nil && p.Phones == nil && p.Organization == nil && p.Title == nil
}

// ErrNoVCard — в данных нет BEGIN:VCARD … END:VCARD.
var ErrNoVCard = errors.New("vcardedit: no vCard in data")

// line — логическая строка vCard: физические строки (первая и свёрнутые
// продолжения) и разбор развёрнутого текста.
type line struct {
	raw []string
	// group — группа в нижнем регистре ("item1" у "item1.EMAIL"), name —
	// имя свойства заглавными.
	group string
	name  string
	// prefix — всё до двоеточия значения как есть (группа, имя, параметры);
	// value — значение как есть, без снятия экранирования.
	prefix string
	value  string
	// drop — строку убрать при сборке; repl — заменить на эти физические
	// строки.
	drop bool
	repl []string
}

type card struct {
	lines []*line
	eol   string
	// begin, end — индексы BEGIN:VCARD и парного END:VCARD первой карточки.
	begin, end int
}

func parse(data string) (*card, error) {
	eol := "\n"
	if strings.Contains(data, "\r\n") {
		eol = "\r\n"
	}
	physical := strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n")
	c := &card{eol: eol, begin: -1, end: -1}
	for _, p := range physical {
		if (strings.HasPrefix(p, " ") || strings.HasPrefix(p, "\t")) && len(c.lines) > 0 {
			last := c.lines[len(c.lines)-1]
			last.raw = append(last.raw, p)
			continue
		}
		c.lines = append(c.lines, &line{raw: []string{p}})
	}
	for _, l := range c.lines {
		unfolded := l.raw[0]
		for _, cont := range l.raw[1:] {
			unfolded += cont[1:]
		}
		l.prefix, l.value = splitValue(unfolded)
		head := l.prefix
		if i := strings.IndexByte(head, ';'); i >= 0 {
			head = head[:i]
		}
		if i := strings.IndexByte(head, '.'); i >= 0 {
			l.group = strings.ToLower(head[:i])
			head = head[i+1:]
		}
		l.name = strings.ToUpper(strings.TrimSpace(head))
	}

	depth := 0
	for i, l := range c.lines {
		switch l.name {
		case "BEGIN":
			if strings.EqualFold(strings.TrimSpace(l.value), "VCARD") {
				depth++
				if c.begin < 0 {
					c.begin = i
				}
			}
		case "END":
			if strings.EqualFold(strings.TrimSpace(l.value), "VCARD") && depth > 0 {
				depth--
				if depth == 0 && c.begin >= 0 {
					c.end = i
					return c, nil
				}
			}
		}
	}
	return nil, ErrNoVCard
}

// splitValue делит развёрнутую строку на «имя с параметрами» и значение по
// первому двоеточию вне кавычек (значение параметра в кавычках может нести
// двоеточие: TYPE="x:y").
func splitValue(s string) (prefix, value string) {
	quoted := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			quoted = !quoted
		case ':':
			if !quoted {
				return s[:i], s[i+1:]
			}
		}
	}
	return s, ""
}

// props — строки свойства name самой карточки (не вложенных, вроде AGENT в
// vCard 2.1), ещё не удалённые.
func (c *card) props(name string) []*line {
	var out []*line
	depth := 0
	for i := c.begin; i <= c.end; i++ {
		l := c.lines[i]
		switch l.name {
		case "BEGIN":
			if strings.EqualFold(strings.TrimSpace(l.value), "VCARD") {
				depth++
			}
			continue
		case "END":
			if strings.EqualFold(strings.TrimSpace(l.value), "VCARD") {
				depth--
			}
			continue
		}
		if depth == 1 && l.name == name && !l.drop {
			out = append(out, l)
		}
	}
	return out
}

// insertAfter — индекс, после которого добавлять новое свойство name: за
// последней строкой того же свойства, иначе перед END:VCARD.
func (c *card) insertAfter(name string) int {
	if ps := c.props(name); len(ps) > 0 {
		last := ps[len(ps)-1]
		for i, l := range c.lines {
			if l != last {
				continue
			}
			// Подпись группы (item2.X-ABLabel) идёт за своим адресом — новое
			// значение встаёт после неё, а не между ними.
			for last.group != "" && i+1 < c.end && c.lines[i+1].group == last.group {
				i++
			}
			return i
		}
	}
	return c.end - 1
}

// pending — новые строки, которые встанут после строки с индексом at.
type pending struct {
	at   int
	text []string
}

// Result — итог правки: новая карточка и имена полей Patch, которые в ней
// действительно поменялись (для очереди обратной синхронизации).
type Result struct {
	Data    string
	Changed []string
}

// Apply меняет в первой карточке data поля из p. Свойства, которых правка не
// касается, и сами строки затронутых свойств, значение которых совпало,
// остаются байт в байт. now — для REV (меняется, только если он уже был и
// что-то поменялось).
func Apply(data string, p Patch, now time.Time) (Result, error) {
	c, err := parse(data)
	if err != nil {
		return Result{Data: data}, err
	}
	var adds []pending
	var changed []string

	if p.FullName != nil {
		want := strings.TrimSpace(*p.FullName)
		if want != "" && c.setFullName(want, &adds) {
			changed = append(changed, "full_name")
		}
	}
	if p.Emails != nil && c.setList("EMAIL", *p.Emails, &adds) {
		changed = append(changed, "emails")
	}
	if p.Phones != nil && c.setList("TEL", *p.Phones, &adds) {
		changed = append(changed, "phones")
	}
	if p.Organization != nil && c.setSingle("ORG", strings.TrimSpace(*p.Organization), escapeStructured, &adds) {
		changed = append(changed, "organization")
	}
	if p.Title != nil && c.setSingle("TITLE", strings.TrimSpace(*p.Title), escapeText, &adds) {
		changed = append(changed, "title")
	}
	if len(changed) == 0 {
		return Result{Data: data}, nil
	}
	for _, l := range c.props("REV") {
		l.repl = []string{l.prefix + ":" + now.UTC().Format("20060102T150405Z")}
	}
	return Result{Data: c.render(adds), Changed: changed}, nil
}

func (c *card) render(adds []pending) string {
	var out []string
	emit := func(i int) {
		for _, a := range adds {
			if a.at == i {
				out = append(out, a.text...)
			}
		}
	}
	for i, l := range c.lines {
		switch {
		case l.drop:
		case l.repl != nil:
			out = append(out, l.repl...)
		default:
			out = append(out, l.raw...)
		}
		emit(i)
	}
	return strings.Join(out, c.eol)
}

// setFullName меняет FN и, если имя поменялось, компоненты имени в N.
func (c *card) setFullName(want string, adds *[]pending) bool {
	fns := c.props("FN")
	old := ""
	if len(fns) > 0 {
		old = strings.TrimSpace(clientValue(fns[0].value))
	}
	if old == want {
		return false
	}
	if len(fns) > 0 {
		replaceValue(fns[0], escapeText(want))
	} else {
		*adds = append(*adds, pending{at: c.afterVersion(), text: fold("FN:" + escapeText(want))})
	}

	ns := c.props("N")
	if len(ns) == 0 {
		// N обязателен в vCard 3.0; у карточки без него — из нового имени.
		given, family := splitName(want, false)
		*adds = append(*adds, pending{at: c.afterVersion(), text: fold("N:" + escapeComponent(family) + ";" + escapeComponent(given) + ";;;")})
		return true
	}
	n := ns[0]
	parts := splitComponents(n.value)
	for len(parts) < 5 {
		parts = append(parts, "")
	}
	oldFamily, oldGiven := unescape(parts[0]), unescape(parts[1])
	if strings.TrimSpace(oldFamily) == "" && strings.TrimSpace(oldGiven) == "" {
		// Пустой N — карточка организации (Apple: X-ABShowAs:COMPANY) или
		// сознательно без имени: не выдумываем человеку фамилию.
		return true
	}
	familyFirst := old != "" && old == strings.TrimSpace(oldFamily+" "+oldGiven) && old != strings.TrimSpace(oldGiven+" "+oldFamily)
	given, family := splitName(want, familyFirst)
	// Отчество/второе имя (компонент 2) уже вошло в новое FN — убираем, чтобы
	// не задвоить; приставку и суффикс (3, 4) оставляем.
	parts[0], parts[1], parts[2] = escapeComponent(family), escapeComponent(given), ""
	replaceValue(n, strings.Join(parts, ";"))
	return true
}

// afterVersion — индекс VERSION (новые FN/N встают сразу за ним), иначе
// BEGIN:VCARD.
func (c *card) afterVersion() int {
	for i := c.begin; i <= c.end; i++ {
		if c.lines[i].name == "VERSION" {
			return i
		}
	}
	return c.begin
}

// setSingle — свойство с одним значением (ORG, TITLE): пустое значение
// убирает свойство целиком.
func (c *card) setSingle(name, want string, esc func(string) string, adds *[]pending) bool {
	ps := c.props(name)
	old := ""
	if len(ps) > 0 {
		old = strings.TrimSpace(clientValue(ps[0].value))
	}
	if old == want {
		return false
	}
	switch {
	case want == "":
		for _, l := range ps {
			c.dropWithLabels(l)
		}
	case len(ps) > 0:
		replaceValue(ps[0], esc(want))
	default:
		*adds = append(*adds, pending{at: c.insertAfter(name), text: fold(name + ":" + esc(want))})
	}
	return true
}

// setList приводит значения многозначного свойства (EMAIL, TEL) к want.
//
// Совпавшие значения остаются своими строками — с типом, группой, подписью
// item1.X-ABLabel. Значения, которых больше нет, и новые сопоставляются по
// порядку: «исправил опечатку в адресе» меняет значение в той же строке, тип
// «рабочий» не теряется. Лишние старые строки уходят (вместе с подписью их
// группы), лишние новые добавляются после последней строки свойства.
// Порядок строк не меняется: перестановка в клиенте не переставляет vCard.
func (c *card) setList(name string, want []string, adds *[]pending) bool {
	var clean []string
	for _, v := range want {
		if v = strings.TrimSpace(v); v != "" {
			clean = append(clean, v)
		}
	}
	ps := c.props(name)
	cur := make([]string, len(ps))
	for i, l := range ps {
		cur[i] = strings.TrimSpace(clientValue(l.value))
	}
	if equalStrings(cur, clean) {
		return false
	}

	remaining := make(map[string]int, len(clean))
	for _, v := range clean {
		remaining[v]++
	}
	var unmatched []*line
	for i, l := range ps {
		if remaining[cur[i]] > 0 {
			remaining[cur[i]]--
			continue
		}
		unmatched = append(unmatched, l)
	}
	var fresh []string
	for _, v := range clean {
		if remaining[v] > 0 {
			remaining[v]--
			fresh = append(fresh, v)
		}
	}

	at := c.insertAfter(name)
	for i := 0; i < len(unmatched) || i < len(fresh); i++ {
		switch {
		case i < len(unmatched) && i < len(fresh):
			replaceValue(unmatched[i], escapeText(fresh[i]))
		case i < len(unmatched):
			c.dropWithLabels(unmatched[i])
		default:
			*adds = append(*adds, pending{at: at, text: fold(name + ":" + escapeText(fresh[i]))})
		}
	}
	return true
}

// dropWithLabels убирает строку, а если у неё есть группа и больше в группе
// ничего не остаётся, кроме подписей Apple (X-ABLabel и т.п.), — и их.
func (c *card) dropWithLabels(l *line) {
	l.drop = true
	if l.group == "" {
		return
	}
	var mates []*line
	for i := c.begin; i <= c.end; i++ {
		m := c.lines[i]
		if m.drop || m.group != l.group {
			continue
		}
		if !strings.HasPrefix(m.name, "X-AB") {
			return // группа ещё жива
		}
		mates = append(mates, m)
	}
	for _, m := range mates {
		m.drop = true
	}
}

// replaceValue переписывает значение строки, сохраняя группу и параметры.
// Параметры кодировки (ENCODING=QUOTED-PRINTABLE / b из vCard 2.1) к новому
// значению не подходят — у такой строки остаются только группа и имя.
func replaceValue(l *line, escaped string) {
	prefix := l.prefix
	if strings.Contains(strings.ToUpper(prefix), "ENCODING=") {
		prefix = l.name
		if l.group != "" {
			prefix = l.prefix[:len(l.group)] + "." + l.name
		}
	}
	l.repl = fold(prefix + ":" + escaped)
}

// splitName делит полное имя на имя и фамилию: фамилия — последнее слово
// (или первое, если карточка писала имя «Фамилия Имя»).
func splitName(full string, familyFirst bool) (given, family string) {
	words := strings.Fields(full)
	switch {
	case len(words) == 0:
		return "", ""
	case len(words) == 1:
		return words[0], ""
	case familyFirst:
		return strings.Join(words[1:], " "), words[0]
	default:
		return strings.Join(words[:len(words)-1], " "), words[len(words)-1]
	}
}

// splitComponents делит структурное значение (N, ORG) по неэкранированной
// точке с запятой; экранирование внутри компонентов сохраняется.
func splitComponents(v string) []string {
	var out []string
	start := 0
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '\\':
			i++
		case ';':
			out = append(out, v[start:i])
			start = i + 1
		}
	}
	return append(out, v[start:])
}

// clientValue — значение так, как его видит клиент: разобранное go-vcard
// (им заполнены поля контакта в базе). go-vcard снимает только \\, \n и
// \, — с тем же правилом и сравниваем, иначе нетронутое клиентом значение с
// \; выглядело бы изменённым и переписывалось бы при каждой правке.
func clientValue(s string) string {
	return clientUnescaper.Replace(s)
}

var clientUnescaper = strings.NewReplacer(`\\`, `\`, `\n`, "\n", `\,`, ",")

// unescape снимает экранирование полностью (RFC 6350 §3.4) — для
// компонентов N, которых клиент не видит.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n', 'N':
				b.WriteByte('\n')
				i++
				continue
			case '\\', ',', ';':
				b.WriteByte(s[i+1])
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// escapeText экранирует значение простого свойства (FN, TITLE, EMAIL, TEL):
// обратная косая, запятая, перевод строки. Точку с запятой в простом
// свойстве RFC 6350 §3.4 экранировать не требует, а go-vcard её экранирование
// не снимает — клиент увидел бы «\;».
func escapeText(s string) string {
	return textEscaper.Replace(s)
}

var textEscaper = strings.NewReplacer(`\`, `\\`, ",", `\,`, "\r\n", `\n`, "\n", `\n`, "\r", `\n`)

// escapeComponent — компонент составного свойства (N): ещё и точка с запятой.
func escapeComponent(s string) string {
	return strings.ReplaceAll(escapeText(s), ";", `\;`)
}

// escapeStructured — значение ORG: точка с запятой из клиента разделяет
// компоненты (организация;отдел) — так её и показывает разбор.
func escapeStructured(s string) string {
	parts := strings.Split(s, ";")
	for i, p := range parts {
		parts[i] = escapeText(p)
	}
	return strings.Join(parts, ";")
}

// maxOctets — предел строки vCard (RFC 6350 §3.2): 75 октетов без перевода
// строки.
const maxOctets = 75

// fold сворачивает строку по 75 октетов, не разрывая UTF-8, и отдаёт
// физические строки (продолжения начинаются с пробела).
func fold(s string) []string {
	if len(s) <= maxOctets {
		return []string{s}
	}
	var out []string
	limit := maxOctets
	prefix := ""
	for len(s) > limit {
		cut := limit
		for cut > 0 && s[cut]&0xC0 == 0x80 {
			cut--
		}
		if cut == 0 {
			cut = limit
		}
		out = append(out, prefix+s[:cut])
		s = s[cut:]
		prefix = " "
		limit = maxOctets - 1
	}
	return append(out, prefix+s)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
