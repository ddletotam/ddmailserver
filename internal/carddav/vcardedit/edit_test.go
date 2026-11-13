package vcardedit

import (
	"strings"
	"testing"
	"time"
)

// Карточка в духе iCloud: фото свёрнутыми строками, день рождения, адрес,
// заметка, группы, X-свойства Apple, подписи item1.X-ABLabel и четыре адреса.
var icloudCard = strings.Join([]string{
	"BEGIN:VCARD",
	"VERSION:3.0",
	"PRODID:-//Apple Inc.//iOS 18.4//EN",
	"N:Иванов;Пётр;Сергеевич;Др.;",
	"FN:Пётр Сергеевич Иванов",
	"ORG:Рога и копыта;Отдел продаж",
	"TITLE:Директор",
	"EMAIL;type=INTERNET;type=HOME;type=pref:petr@home.example",
	"EMAIL;type=INTERNET;type=WORK:petr@work.example",
	"item1.EMAIL;type=INTERNET:petr@old.example",
	"item1.X-ABLabel:_$!<Other>!$_",
	"item2.EMAIL;type=INTERNET:p.ivanov@fourth.example",
	"item2.X-ABLabel:Личный",
	"TEL;type=CELL;type=VOICE;type=pref:+7 900 000-00-01",
	"TEL;type=WORK;type=VOICE:+7 495 000-00-02",
	"item3.ADR;type=HOME;type=pref:;;ул. Ленина\\, 1;Москва;;101000;Россия",
	"item3.X-ABADR:ru",
	"NOTE:Звонить после 18:00\\nне по пятницам",
	"BDAY;value=date:1980-05-17",
	"X-SOCIALPROFILE;type=telegram:x-apple:petr_iv",
	"X-ABRELATEDNAMES;type=pref:Мария",
	"X-ABShowAs:PERSON",
	"CATEGORIES:Друзья,Работа",
	"PHOTO;ENCODING=b;TYPE=JPEG:/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAMCAgICAgMCAgIDAwMDBAYEBA",
	" QFBQQEBAQEBQcFBQcIDAoHBwcKDQoMDA0MCxMUExEREhEVFxIUFxQRDw0SFBMHBwcI",
	" CAgICQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQn/wAAR",
	"UID:6D9C5A40-1B7B-4E0F-9A3D-2F0A9A1C0B11",
	"REV:2026-10-01T10:00:00Z",
	"END:VCARD",
	"",
}, "\r\n")

var fixedNow = time.Date(2026, 11, 14, 3, 4, 5, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

// keepLines — строки, которые обязаны остаться байт в байт.
func mustContainAll(t *testing.T, got string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(got, l) {
			t.Errorf("пропала строка %q\n--- карточка ---\n%s", l, got)
		}
	}
}

func TestApply_RenameKeepsEverythingElse(t *testing.T) {
	res, err := Apply(icloudCard, Patch{FullName: ptr("Пётр Иванов-Петров")}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 1 || res.Changed[0] != "full_name" {
		t.Fatalf("Changed = %v", res.Changed)
	}
	got := res.Data
	mustContainAll(t, got,
		"FN:Пётр Иванов-Петров\r\n",
		// Имя «Имя Фамилия»: фамилия — последнее слово; отчество (компонент
		// 2) вошло в FN и убрано, приставка «Др.» осталась.
		"N:Иванов-Петров;Пётр;;Др.;\r\n",
		"REV:20261114T030405Z\r\n",
	)
	// Всё прочее — те же байты, что пришли: фото с его свёрткой, день
	// рождения, адрес, заметка, X-свойства, все четыре адреса с подписями.
	for _, l := range strings.Split(icloudCard, "\r\n") {
		if strings.HasPrefix(l, "FN:") || strings.HasPrefix(l, "N:") || strings.HasPrefix(l, "REV:") {
			continue
		}
		if !strings.Contains(got, l) {
			t.Errorf("строка изменилась или пропала: %q", l)
		}
	}
	photo := "PHOTO;ENCODING=b;TYPE=JPEG:/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAMCAgICAgMCAgIDAwMDBAYEBA\r\n QFBQQ"
	if !strings.Contains(got, photo) {
		t.Errorf("фото потеряло свёртку")
	}
	if strings.Count(got, "EMAIL") != 4 {
		t.Errorf("адресов не 4:\n%s", got)
	}
	if len(got) != len(icloudCard)-len("N:Иванов;Пётр;Сергеевич;Др.;")+len("N:Иванов-Петров;Пётр;;Др.;")-
		len("FN:Пётр Сергеевич Иванов")+len("FN:Пётр Иванов-Петров")-len("REV:2026-10-01T10:00:00Z")+len("REV:20261114T030405Z") {
		t.Errorf("длина карточки не сходится: что-то ещё изменилось\n%s", got)
	}

	f, err := Read(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Emails) != 4 || f.Emails[3] != "p.ivanov@fourth.example" {
		t.Errorf("после правки адреса: %v", f.Emails)
	}
}

func TestApply_SameValuesChangeNothing(t *testing.T) {
	// Клиенты шлют форму целиком: значения, совпавшие с карточкой, — не
	// правка. Карточка возвращается как есть, REV не трогается.
	p := Patch{
		FullName:     ptr("Пётр Сергеевич Иванов"),
		Emails:       &[]string{"petr@home.example", "petr@work.example", "petr@old.example", "p.ivanov@fourth.example"},
		Phones:       &[]string{"+7 900 000-00-01", "+7 495 000-00-02"},
		Organization: ptr("Рога и копыта;Отдел продаж"),
		Title:        ptr("Директор"),
	}
	res, err := Apply(icloudCard, p, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if res.Data != icloudCard || len(res.Changed) != 0 {
		t.Fatalf("неизменённая форма поменяла карточку: %v\n%s", res.Changed, res.Data)
	}
}

func TestApply_EmailsEditInPlace(t *testing.T) {
	// Два адреса заменены: несовпавшие старые и новые сопоставляются по
	// порядку и меняют значение в своей строке — тип WORK и группа item1 с
	// подписью остаются. item2 не тронут.
	p := Patch{Emails: &[]string{"petr@home.example", "petr@work2.example", "new@fifth.example", "p.ivanov@fourth.example"}}
	res, err := Apply(icloudCard, p, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	mustContainAll(t, res.Data,
		"EMAIL;type=INTERNET;type=HOME;type=pref:petr@home.example\r\n",
		"EMAIL;type=INTERNET;type=WORK:petr@work2.example\r\n",
		"item1.EMAIL;type=INTERNET:new@fifth.example\r\nitem1.X-ABLabel:_$!<Other>!$_\r\n",
		"item2.EMAIL;type=INTERNET:p.ivanov@fourth.example\r\nitem2.X-ABLabel:Личный\r\n",
	)
}

func TestApply_EmailsRemoveAndAdd(t *testing.T) {
	// Удалён третий адрес (item1) — уходит и его подпись; добавлен пятый —
	// встаёт за последним адресом.
	p := Patch{Emails: &[]string{"petr@home.example", "petr@work.example", "p.ivanov@fourth.example"}}
	res, err := Apply(icloudCard, p, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Data, "petr@old.example") || strings.Contains(res.Data, "_$!<Other>!$_") {
		t.Errorf("удалённый адрес или его подпись остались:\n%s", res.Data)
	}
	mustContainAll(t, res.Data, "item2.X-ABLabel:Личный\r\n", "item3.X-ABADR:ru\r\n")

	add := append(*p.Emails, "new@fifth.example")
	res, err = Apply(res.Data, Patch{Emails: &add}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	mustContainAll(t, res.Data, "item2.EMAIL;type=INTERNET:p.ivanov@fourth.example\r\nitem2.X-ABLabel:Личный\r\nEMAIL:new@fifth.example\r\n")
	f, _ := Read(res.Data)
	if len(f.Emails) != 4 || f.Emails[3] != "new@fifth.example" {
		t.Errorf("emails = %v", f.Emails)
	}
}

func TestApply_PhonesAllAndNoLimit(t *testing.T) {
	want := []string{"+7 900 000-00-01", "+7 495 000-00-02", "+1 1", "+1 2", "+1 3"}
	res, err := Apply(icloudCard, Patch{Phones: &want}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := Read(res.Data)
	if strings.Join(f.Phones, "|") != strings.Join(want, "|") {
		t.Errorf("phones = %v", f.Phones)
	}
}

func TestApply_ClearTitleAndOrg(t *testing.T) {
	res, err := Apply(icloudCard, Patch{Title: ptr(""), Organization: ptr("")}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Data, "TITLE") || strings.Contains(res.Data, "ORG:") {
		t.Errorf("TITLE/ORG не убраны:\n%s", res.Data)
	}
}

func TestApply_EmptyFullNameDoesNotEraseName(t *testing.T) {
	res, err := Apply(icloudCard, Patch{FullName: ptr("  ")}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if res.Data != icloudCard {
		t.Errorf("пустое имя стёрло FN")
	}
}

func TestApply_FamilyFirstOrderAndEscaping(t *testing.T) {
	card := "BEGIN:VCARD\nVERSION:3.0\nN:Сидоров;Иван;;;\nFN:Сидоров Иван\nEND:VCARD\n"
	res, err := Apply(card, Patch{FullName: ptr("Сидоров Иван, мл."), Organization: ptr("ООО «A, B»")}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	// Перевод строки — как в исходной карточке (LF); новая ORG — перед END.
	want := "BEGIN:VCARD\nVERSION:3.0\nN:Сидоров;Иван\\, мл.;;;\nFN:Сидоров Иван\\, мл.\nORG:ООО «A\\, B»\nEND:VCARD\n"
	if res.Data != want {
		t.Errorf("got\n%q\nwant\n%q", res.Data, want)
	}
	f, _ := Read(res.Data)
	if f.FullName != "Сидоров Иван, мл." || f.Organization != "ООО «A, B»" {
		t.Errorf("Read: %+v", f)
	}
}

func TestApply_CompanyCardKeepsEmptyN(t *testing.T) {
	card := "BEGIN:VCARD\r\nVERSION:3.0\r\nN:;;;;\r\nFN:Рога и копыта\r\nX-ABShowAs:COMPANY\r\nEND:VCARD\r\n"
	res, err := Apply(card, Patch{FullName: ptr("Рога и копыта 2")}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Data, "N:;;;;\r\n") || !strings.Contains(res.Data, "FN:Рога и копыта 2\r\n") {
		t.Errorf("got %q", res.Data)
	}
}

func TestApply_MissingFNAndN(t *testing.T) {
	card := "BEGIN:VCARD\r\nVERSION:4.0\r\nEMAIL:a@b.c\r\nEND:VCARD\r\n"
	res, err := Apply(card, Patch{FullName: ptr("Анна Каренина")}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	want := "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Анна Каренина\r\nN:Каренина;Анна;;;\r\nEMAIL:a@b.c\r\nEND:VCARD\r\n"
	if res.Data != want {
		t.Errorf("got %q", res.Data)
	}
}

func TestApply_QuotedPrintableValueLosesEncodingParam(t *testing.T) {
	card := "BEGIN:VCARD\r\nVERSION:2.1\r\nN:;;;;\r\nFN;CHARSET=UTF-8;ENCODING=QUOTED-PRINTABLE:=D0=90\r\nEND:VCARD\r\n"
	res, err := Apply(card, Patch{FullName: ptr("Б")}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Data, "\r\nFN:Б\r\n") {
		t.Errorf("got %q", res.Data)
	}
}

func TestApply_LongValueIsFolded(t *testing.T) {
	long := strings.Repeat("Очень длинное название организации ", 4)
	res, err := Apply("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:x\r\nEND:VCARD\r\n", Patch{Organization: ptr(long)}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(res.Data, "\r\n") {
		if len(l) > 75 {
			t.Errorf("строка длиннее 75 октетов: %q", l)
		}
	}
	f, _ := Read(res.Data)
	if f.Organization != strings.TrimSpace(long) {
		t.Errorf("после свёртки: %q", f.Organization)
	}
}

func TestApply_NoVCard(t *testing.T) {
	if _, err := Apply("garbage", Patch{FullName: ptr("x")}, fixedNow); err != ErrNoVCard {
		t.Errorf("err = %v", err)
	}
}

func TestApply_SemicolonInFNIsStable(t *testing.T) {
	// go-vcard не снимает «\;» — клиент видит и присылает обратно ровно это.
	// Такое значение — не правка, иначе экранирование росло бы с каждой.
	card := "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:A\\;B\r\nEND:VCARD\r\n"
	f, _ := Read(card)
	res, err := Apply(card, Patch{FullName: ptr(f.FullName)}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if res.Data != card {
		t.Errorf("got %q", res.Data)
	}
}
