package caldav

import (
	"strconv"
	"strings"
	"time"
)

// TodoFields — то, что пользователь задаёт задаче из клиента: заголовок,
// заметка, срок, приоритет. Всё остальное (STATUS, UID, DTSTAMP) решает сервер.
type TodoFields struct {
	Summary     string
	Description string
	// Due — срок в миллисекундах; nil — задача без срока.
	Due *int64
	// AllDay — срок датой (DUE;VALUE=DATE), а не моментом. Дата хранится
	// полуночью UTC, как у событий «на весь день» (importer/timezone.go).
	AllDay bool
	// Priority — 1 (высший) … 9 (низший); nil или 0 — не указан.
	Priority *int16
}

// TodoPatch — правка задачи: nil-поле не трогается.
type TodoPatch struct {
	Summary     *string
	Description *string
	// SetDue — срок меняется: на Due (AllDay — датой), а при Due == nil
	// снимается вовсе.
	SetDue bool
	Due    *int64
	AllDay bool
	// Priority: 0 снимает приоритет.
	Priority *int16
}

const (
	stampLayout = "20060102T150405Z"
	dateLayout  = "20060102"
)

// BuildTodo собирает VCALENDAR с одной новой задачей.
//
// DTSTAMP обязателен по RFC 5545: без него iCloud отвечает 403 на PUT, и
// задача навсегда застревает в очереди обратной синхронизации. CREATED и
// LAST-MODIFIED не обязательны, но Напоминания Apple по ним упорядочивают
// изменения.
func BuildTodo(uid string, f TodoFields, nowMs int64) string {
	stamp := time.UnixMilli(nowMs).UTC().Format(stampLayout)

	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:-//DDMailServer//Tasks//EN\r\n")
	b.WriteString("BEGIN:VTODO\r\n")
	b.WriteString(ContentLine("UID", uid))
	b.WriteString("DTSTAMP:" + stamp + "\r\n")
	b.WriteString("CREATED:" + stamp + "\r\n")
	b.WriteString("LAST-MODIFIED:" + stamp + "\r\n")
	b.WriteString(ContentLine("SUMMARY", f.Summary))
	if f.Description != "" {
		b.WriteString(ContentLine("DESCRIPTION", f.Description))
	}
	if f.Due != nil {
		b.WriteString(dueLine("DUE", *f.Due, f.AllDay))
	}
	if f.Priority != nil && *f.Priority > 0 {
		b.WriteString("PRIORITY:" + strconv.Itoa(int(*f.Priority)) + "\r\n")
	}
	b.WriteString("STATUS:NEEDS-ACTION\r\n")
	b.WriteString("END:VTODO\r\n")
	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}

// dueLine форматирует DUE или DTSTART задачи: датой для «на весь день»,
// иначе моментом в UTC. TZID не пишем: момент в UTC однозначен, а
// VTIMEZONE пришлось бы собирать к нему.
func dueLine(name string, ms int64, allDay bool) string {
	t := time.UnixMilli(ms).UTC()
	if allDay {
		return name + ";VALUE=DATE:" + t.Format(dateLayout) + "\r\n"
	}
	return name + ":" + t.Format(stampLayout) + "\r\n"
}

// TruncateToUTCDate приводит срок «на весь день» к полуночи UTC его даты —
// так такие даты хранятся во всей базе.
func TruncateToUTCDate(ms int64) int64 {
	t := time.UnixMilli(ms).UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
}

// icalLine — логическая строка iCalendar: первая физическая строка и её
// продолжения (свёрнутые, начинаются с пробела или табуляции).
type icalLine struct {
	raw []string
	// name — имя свойства заглавными, без параметров; для BEGIN/END — "BEGIN"
	// и "END", значение — в value.
	name  string
	value string
}

func splitICalLines(icalData string) ([]icalLine, string) {
	eol := "\n"
	if strings.Contains(icalData, "\r\n") {
		eol = "\r\n"
	}
	physical := strings.Split(strings.ReplaceAll(icalData, "\r\n", "\n"), "\n")
	var out []icalLine
	for _, p := range physical {
		if (strings.HasPrefix(p, " ") || strings.HasPrefix(p, "\t")) && len(out) > 0 {
			last := &out[len(out)-1]
			last.raw = append(last.raw, p)
			continue
		}
		out = append(out, icalLine{raw: []string{p}})
	}
	for i := range out {
		unfolded := out[i].raw[0]
		for _, c := range out[i].raw[1:] {
			unfolded += c[1:]
		}
		name := unfolded
		value := ""
		if j := strings.IndexAny(unfolded, ":;"); j != -1 {
			name = unfolded[:j]
			if k := strings.Index(unfolded, ":"); k != -1 {
				value = unfolded[k+1:]
			}
		}
		out[i].name = strings.ToUpper(strings.TrimSpace(name))
		out[i].value = strings.ToUpper(strings.TrimSpace(value))
	}
	return out, eol
}

// SetTodoFields переписывает в теле задачи то, что задаёт пользователь.
//
// Хирургия по строкам, а не разбор и пересборка — по той же причине, что и у
// SetTodoCompletion: задача из Напоминаний iOS несёт свойства, которых мы не
// моделируем (X-APPLE-SORT-ORDER, геозоны, вложенные VALARM), и правка
// заголовка не должна их молча стирать. Трогаются только свойства самой
// VTODO: DESCRIPTION внутри вложенного VALARM — это текст будильника, а не
// заметка задачи.
//
// Смена срока двигает и DTSTART, если он был: RFC 5545 требует, чтобы DTSTART
// не был позже DUE и имел тот же тип значения (дата или момент), а будильники
// Apple считаются от DTSTART — выбросить его значило бы сломать их.
// Снятие срока снимает и DTSTART: у Напоминаний «без даты» нет ни того, ни
// другого.
func SetTodoFields(icalData string, p TodoPatch, nowMs int64) string {
	if icalData == "" || !strings.Contains(strings.ToUpper(icalData), "BEGIN:VTODO") {
		return icalData
	}
	lines, eol := splitICalLines(icalData)
	stamp := time.UnixMilli(nowMs).UTC().Format(stampLayout)

	drop := map[string]bool{"DTSTAMP": true, "LAST-MODIFIED": true}
	if p.Summary != nil {
		drop["SUMMARY"] = true
	}
	if p.Description != nil {
		drop["DESCRIPTION"] = true
	}
	if p.SetDue {
		drop["DUE"] = true
		drop["DTSTART"] = true
	}
	if p.Priority != nil {
		drop["PRIORITY"] = true
	}

	// Первый проход: был ли DTSTART у самой задачи.
	hadStart := false
	walkTodo(lines, func(l icalLine) {
		if l.name == "DTSTART" {
			hadStart = true
		}
	})

	var add []string
	add = append(add, "DTSTAMP:"+stamp, "LAST-MODIFIED:"+stamp)
	if p.Summary != nil {
		add = append(add, strings.TrimSuffix(ContentLine("SUMMARY", *p.Summary), "\r\n"))
	}
	if p.Description != nil && *p.Description != "" {
		add = append(add, strings.TrimSuffix(ContentLine("DESCRIPTION", *p.Description), "\r\n"))
	}
	if p.SetDue && p.Due != nil {
		add = append(add, strings.TrimSuffix(dueLine("DUE", *p.Due, p.AllDay), "\r\n"))
		if hadStart {
			add = append(add, strings.TrimSuffix(dueLine("DTSTART", *p.Due, p.AllDay), "\r\n"))
		}
	}
	if p.Priority != nil && *p.Priority > 0 {
		add = append(add, "PRIORITY:"+strconv.Itoa(int(*p.Priority)))
	}

	var out []string
	depth := 0
	inTodo := false
	todoDepth := 0
	inserted := false
	for _, l := range lines {
		switch l.name {
		case "BEGIN":
			depth++
			out = append(out, l.raw...)
			if l.value == "VTODO" && !inserted {
				inTodo = true
				todoDepth = depth
				for _, a := range add {
					// ContentLine уже свернул длинное значение через "\r\n ";
					// приводим перевод строки к принятому в этом теле.
					out = append(out, strings.Split(strings.ReplaceAll(a, "\r\n", "\n"), "\n")...)
				}
				inserted = true
			}
			continue
		case "END":
			if inTodo && depth == todoDepth && l.value == "VTODO" {
				inTodo = false
			}
			depth--
			out = append(out, l.raw...)
			continue
		}
		if inTodo && depth == todoDepth && drop[l.name] {
			continue
		}
		out = append(out, l.raw...)
	}
	return strings.Join(out, eol)
}

// walkTodo вызывает fn для свойств первой VTODO (без вложенных компонентов).
func walkTodo(lines []icalLine, fn func(icalLine)) {
	depth := 0
	todoDepth := -1
	for _, l := range lines {
		switch l.name {
		case "BEGIN":
			depth++
			if l.value == "VTODO" && todoDepth == -1 {
				todoDepth = depth
			}
			continue
		case "END":
			if depth == todoDepth && l.value == "VTODO" {
				return
			}
			depth--
			continue
		}
		if depth == todoDepth {
			fn(l)
		}
	}
}
