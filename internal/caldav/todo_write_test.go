package caldav

import (
	"strings"
	"testing"

	"github.com/emersion/go-ical"
)

const (
	// 2026-11-13T09:30:00Z
	testDueMs = int64(1794562200000)
	// 2026-01-01T00:00:00Z
	testNowMs = int64(1767225600000)
)

func ptr[T any](v T) *T { return &v }

// decodeTodo разбирает тело настоящим парсером: то, что уходит в iCloud,
// должно читаться не только нашими строковыми проверками.
func decodeTodo(t *testing.T, body string) *ical.Component {
	t.Helper()
	cal, err := ical.NewDecoder(strings.NewReader(body)).Decode()
	if err != nil {
		t.Fatalf("тело не разбирается: %v\n%s", err, body)
	}
	for _, c := range cal.Children {
		if c.Name == ical.CompToDo {
			return c
		}
	}
	t.Fatalf("в теле нет VTODO:\n%s", body)
	return nil
}

func TestBuildTodoTimedDue(t *testing.T) {
	body := BuildTodo("abc@ddmailserver", TodoFields{
		Summary:     "Позвонить, договориться; потом — отчёт",
		Description: "Строка 1\nСтрока 2",
		Due:         ptr(testDueMs),
		Priority:    ptr(int16(1)),
	}, testNowMs)

	todo := decodeTodo(t, body)
	if got := todo.Props.Get("UID").Value; got != "abc@ddmailserver" {
		t.Errorf("UID = %q", got)
	}
	if got, _ := todo.Props.Text("SUMMARY"); got != "Позвонить, договориться; потом — отчёт" {
		t.Errorf("SUMMARY = %q", got)
	}
	if got, _ := todo.Props.Text("DESCRIPTION"); got != "Строка 1\nСтрока 2" {
		t.Errorf("DESCRIPTION = %q", got)
	}
	if got := todo.Props.Get("DUE").Value; got != "20261113T093000Z" {
		t.Errorf("DUE = %q", got)
	}
	if todo.Props.Get("DTSTAMP") == nil {
		t.Error("нет DTSTAMP — iCloud ответит 403")
	}
	if got := todo.Props.Get("STATUS").Value; got != "NEEDS-ACTION" {
		t.Errorf("STATUS = %q", got)
	}
	if got := todo.Props.Get("PRIORITY").Value; got != "1" {
		t.Errorf("PRIORITY = %q", got)
	}
	if todo.Props.Get("DTSTART") != nil {
		t.Error("DTSTART не просили")
	}
}

func TestBuildTodoAllDayAndBare(t *testing.T) {
	body := BuildTodo("u1", TodoFields{Summary: "Купить хлеб", Due: ptr(TruncateToUTCDate(testDueMs)), AllDay: true}, testNowMs)
	if !strings.Contains(body, "DUE;VALUE=DATE:20261113\r\n") {
		t.Errorf("срок датой не записан:\n%s", body)
	}

	bare := BuildTodo("u2", TodoFields{Summary: "Без срока", Priority: ptr(int16(0))}, testNowMs)
	todo := decodeTodo(t, bare)
	if todo.Props.Get("DUE") != nil || todo.Props.Get("DESCRIPTION") != nil || todo.Props.Get("PRIORITY") != nil {
		t.Errorf("лишние свойства у задачи без срока:\n%s", bare)
	}
}

func TestTruncateToUTCDate(t *testing.T) {
	if got := TruncateToUTCDate(testDueMs); got != 1794528000000 {
		t.Errorf("TruncateToUTCDate = %d, ждали полночь UTC 2026-11-13", got)
	}
}

// Задача из Напоминаний: со вложенным VALARM, свёрнутой строкой и
// свойствами Apple, которых мы не моделируем.
const appleReminder = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//Apple Inc.//iOS 26.6//EN\r\n" +
	"BEGIN:VTODO\r\n" +
	"CREATED:20260825T143426Z\r\n" +
	"DTSTAMP:20260825T143443Z\r\n" +
	"DTSTART;TZID=Europe/Moscow:20260919T090000\r\n" +
	"DUE;TZID=Europe/Moscow:20260919T090000\r\n" +
	"STATUS:NEEDS-ACTION\r\n" +
	"SUMMARY:Очень длинный заголовок напоминания, который не влезает в семьдесят\r\n" +
	"  пять октетов\r\n" +
	"DESCRIPTION:Старая заметка\r\n" +
	"PRIORITY:5\r\n" +
	"UID:FCC55C22-6C52-432C-8684-D82A49F77590\r\n" +
	"X-APPLE-SORT-ORDER:12\r\n" +
	"BEGIN:VALARM\r\n" +
	"ACTION:DISPLAY\r\n" +
	"DESCRIPTION:Напоминание\r\n" +
	"TRIGGER:-PT15M\r\n" +
	"END:VALARM\r\n" +
	"END:VTODO\r\n" +
	"END:VCALENDAR\r\n"

func TestSetTodoFieldsKeepsAppleProperties(t *testing.T) {
	out := SetTodoFields(appleReminder, TodoPatch{
		Summary:     ptr("Новый заголовок"),
		Description: ptr(""),
		SetDue:      true,
		Due:         ptr(TruncateToUTCDate(testDueMs)),
		AllDay:      true,
	}, testNowMs)

	todo := decodeTodo(t, out)
	if got, _ := todo.Props.Text("SUMMARY"); got != "Новый заголовок" {
		t.Errorf("SUMMARY = %q", got)
	}
	if n := len(todo.Props.Values("SUMMARY")); n != 1 {
		t.Errorf("SUMMARY %d раз — продолжение свёрнутой строки осталось сиротой?\n%s", n, out)
	}
	if todo.Props.Get("DESCRIPTION") != nil {
		t.Error("пустая заметка должна снимать DESCRIPTION")
	}
	if got := todo.Props.Get("DUE").Value; got != "20261113" {
		t.Errorf("DUE = %q", got)
	}
	// DTSTART был — он идёт за сроком и с тем же типом значения.
	if got := todo.Props.Get("DTSTART").Value; got != "20261113" {
		t.Errorf("DTSTART = %q, должен совпасть с DUE", got)
	}
	if got := todo.Props.Get("DTSTAMP").Value; got != "20260101T000000Z" {
		t.Errorf("DTSTAMP = %q", got)
	}
	// Не тронуто: приоритет, свойство Apple, будильник с его DESCRIPTION.
	if got := todo.Props.Get("PRIORITY").Value; got != "5" {
		t.Errorf("PRIORITY = %q, его не меняли", got)
	}
	if todo.Props.Get("X-APPLE-SORT-ORDER") == nil {
		t.Error("X-APPLE-SORT-ORDER потерян")
	}
	if len(todo.Children) != 1 || todo.Children[0].Name != "VALARM" {
		t.Fatalf("VALARM потерян:\n%s", out)
	}
	if got, _ := todo.Children[0].Props.Text("DESCRIPTION"); got != "Напоминание" {
		t.Errorf("текст будильника = %q — правка задачи задела VALARM", got)
	}
	if !strings.HasSuffix(out, "END:VCALENDAR\r\n") || strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") {
		t.Error("переводы строк тела должны остаться CRLF")
	}
}

func TestSetTodoFieldsClearsDueAndPriority(t *testing.T) {
	out := SetTodoFields(appleReminder, TodoPatch{SetDue: true, Priority: ptr(int16(0))}, testNowMs)
	todo := decodeTodo(t, out)
	if todo.Props.Get("DUE") != nil || todo.Props.Get("DTSTART") != nil {
		t.Errorf("срок не снят:\n%s", out)
	}
	if todo.Props.Get("PRIORITY") != nil {
		t.Error("приоритет не снят")
	}
	if got, _ := todo.Props.Text("DESCRIPTION"); got != "Старая заметка" {
		t.Errorf("заметку не трогали, а она %q", got)
	}
}

func TestSetTodoFieldsTimedDueWithoutStart(t *testing.T) {
	body := BuildTodo("u3", TodoFields{Summary: "a"}, testNowMs)
	out := SetTodoFields(body, TodoPatch{SetDue: true, Due: ptr(testDueMs)}, testNowMs+1000)
	todo := decodeTodo(t, out)
	if got := todo.Props.Get("DUE").Value; got != "20261113T093000Z" {
		t.Errorf("DUE = %q", got)
	}
	if todo.Props.Get("DTSTART") != nil {
		t.Error("DTSTART не было — и не должно появиться")
	}
	if n := len(todo.Props.Values("DTSTAMP")); n != 1 {
		t.Errorf("DTSTAMP %d раз", n)
	}
}

func TestSetTodoFieldsLeavesEventsAlone(t *testing.T) {
	ev := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nSUMMARY:x\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if got := SetTodoFields(ev, TodoPatch{Summary: ptr("y")}, testNowMs); got != ev {
		t.Errorf("событие изменено:\n%s", got)
	}
}
