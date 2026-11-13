package ru.letotam.ddmail.dav

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.time.Instant

/** Порт тестов `internal/carddav/vcardedit` сервера: правка по строкам без потерь. */
class VCardEditTest {
    /** Карточка в духе iCloud: фото свёрнутыми строками, адрес, заметка, группы, четыре адреса. */
    private val icloud = listOf(
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
        "item1.X-ABLabel:_\$!<Other>!\$_",
        "item2.EMAIL;type=INTERNET:p.ivanov@fourth.example",
        "item2.X-ABLabel:Личный",
        "TEL;type=CELL;type=VOICE;type=pref:+7 900 000-00-01",
        "TEL;type=WORK;type=VOICE:+7 495 000-00-02",
        "item3.ADR;type=HOME;type=pref:;;ул. Ленина\\, 1;Москва;;101000;Россия",
        "item3.X-ABADR:ru",
        "NOTE:Звонить после 18:00\\nне по пятницам",
        "BDAY;value=date:1980-05-17",
        "X-SOCIALPROFILE;type=telegram:x-apple:petr_iv",
        "CATEGORIES:Друзья,Работа",
        "PHOTO;ENCODING=b;TYPE=JPEG:/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAMCAgICAgMCAgIDAwMDBAYEBA",
        " QFBQQEBAQEBQcFBQcIDAoHBwcKDQoMDA0MCxMUExEREhEVFxIUFxQRDw0SFBMHBwcI",
        " CAgICQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQn/wAAR",
        "UID:6D9C5A40-1B7B-4E0F-9A3D-2F0A9A1C0B11",
        "REV:2026-10-01T10:00:00Z",
        "END:VCARD",
        "",
    ).joinToString("\r\n")

    private val now = Instant.parse("2026-11-14T03:04:05Z")

    private fun read(card: String) = VCard.parse(card)!!.second

    @Test
    fun renameKeepsEverythingElse() {
        val r = VCardEdit.apply(icloud, VCardEdit.Patch(fullName = "Пётр Иванов-Петров"), now)
        assertEquals(listOf("full_name"), r.changed)
        assertTrue(r.data.contains("FN:Пётр Иванов-Петров\r\n"))
        // Отчество вошло в FN и убрано, приставка «Др.» осталась.
        assertTrue(r.data.contains("N:Иванов-Петров;Пётр;;Др.;\r\n"))
        assertTrue(r.data.contains("REV:20261114T030405Z\r\n"))
        for (l in icloud.split("\r\n")) {
            if (l.startsWith("FN:") || l.startsWith("N:") || l.startsWith("REV:")) continue
            assertTrue("строка изменилась или пропала: $l", r.data.contains(l))
        }
        assertTrue("фото потеряло свёртку", r.data.contains("TYPE=JPEG:/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAMCAgICAgMCAgIDAwMDBAYEBA\r\n QFBQQ"))
        assertEquals(4, read(r.data).emails.size)
    }

    @Test
    fun sameValuesChangeNothing() {
        // Форма шлёт всё: совпавшее с карточкой — не правка, REV не трогается.
        val p = VCardEdit.Patch(
            fullName = "Пётр Сергеевич Иванов",
            emails = listOf("petr@home.example", "petr@work.example", "petr@old.example", "p.ivanov@fourth.example"),
            phones = listOf("+7 900 000-00-01", "+7 495 000-00-02"),
            organization = "Рога и копыта",
            title = "Директор",
        )
        val r = VCardEdit.apply(icloud, p, now)
        assertEquals(icloud, r.data)
        assertTrue(r.changed.isEmpty())
        // Ровно то, что показал разбор Android, — тоже не правка.
        val c = read(icloud)
        val again = VCardEdit.apply(icloud, VCardEdit.Patch(c.fullName, c.emails, c.phones, c.organization, c.title), now)
        assertEquals(icloud, again.data)
    }

    @Test
    fun emailsEditInPlace() {
        val p = VCardEdit.Patch(emails = listOf("petr@home.example", "petr@work2.example", "new@fifth.example", "p.ivanov@fourth.example"))
        val d = VCardEdit.apply(icloud, p, now).data
        assertTrue(d.contains("EMAIL;type=INTERNET;type=HOME;type=pref:petr@home.example\r\n"))
        assertTrue(d.contains("EMAIL;type=INTERNET;type=WORK:petr@work2.example\r\n"))
        assertTrue(d.contains("item1.EMAIL;type=INTERNET:new@fifth.example\r\nitem1.X-ABLabel:_\$!<Other>!\$_\r\n"))
        assertTrue(d.contains("item2.EMAIL;type=INTERNET:p.ivanov@fourth.example\r\nitem2.X-ABLabel:Личный\r\n"))
    }

    @Test
    fun emailsRemoveAndAdd() {
        val three = listOf("petr@home.example", "petr@work.example", "p.ivanov@fourth.example")
        val d = VCardEdit.apply(icloud, VCardEdit.Patch(emails = three), now).data
        assertFalse(d.contains("petr@old.example"))
        assertFalse("подпись удалённого адреса осталась", d.contains("<Other>"))
        assertTrue(d.contains("item2.X-ABLabel:Личный\r\n"))
        assertTrue(d.contains("item3.X-ABADR:ru\r\n"))
        val d2 = VCardEdit.apply(d, VCardEdit.Patch(emails = three + "new@fifth.example"), now).data
        assertTrue(d2.contains("item2.EMAIL;type=INTERNET:p.ivanov@fourth.example\r\nitem2.X-ABLabel:Личный\r\nEMAIL:new@fifth.example\r\n"))
        assertEquals(listOf("petr@home.example", "petr@work.example", "p.ivanov@fourth.example", "new@fifth.example"), read(d2).emails)
    }

    @Test
    fun phonesWithoutLimit() {
        val want = listOf("+7 900 000-00-01", "+7 495 000-00-02", "+1 1", "+1 2", "+1 3")
        assertEquals(want, read(VCardEdit.apply(icloud, VCardEdit.Patch(phones = want), now).data).phones)
    }

    @Test
    fun orgKeepsDepartmentAndClears() {
        val d = VCardEdit.apply(icloud, VCardEdit.Patch(organization = "Копыта и рога"), now).data
        assertTrue(d.contains("ORG:Копыта и рога;Отдел продаж\r\n"))
        val cleared = VCardEdit.apply(icloud, VCardEdit.Patch(title = "", organization = ""), now).data
        assertFalse(cleared.contains("TITLE"))
        assertFalse(cleared.contains("ORG:"))
    }

    @Test
    fun emptyFullNameDoesNotEraseName() {
        assertEquals(icloud, VCardEdit.apply(icloud, VCardEdit.Patch(fullName = "  "), now).data)
    }

    @Test
    fun familyFirstOrderAndEscaping() {
        val card = "BEGIN:VCARD\nVERSION:3.0\nN:Сидоров;Иван;;;\nFN:Сидоров Иван\nEND:VCARD\n"
        val d = VCardEdit.apply(card, VCardEdit.Patch(fullName = "Сидоров Иван, мл.", organization = "ООО «A, B»"), now).data
        assertEquals("BEGIN:VCARD\nVERSION:3.0\nN:Сидоров;Иван\\, мл.;;;\nFN:Сидоров Иван\\, мл.\nORG:ООО «A\\, B»\nEND:VCARD\n", d)
        val c = read(d)
        assertEquals("Сидоров Иван, мл.", c.fullName)
        assertEquals("ООО «A, B»", c.organization)
    }

    @Test
    fun companyCardKeepsEmptyN() {
        val card = "BEGIN:VCARD\r\nVERSION:3.0\r\nN:;;;;\r\nFN:Рога и копыта\r\nX-ABShowAs:COMPANY\r\nEND:VCARD\r\n"
        val d = VCardEdit.apply(card, VCardEdit.Patch(fullName = "Рога и копыта 2"), now).data
        assertTrue(d.contains("N:;;;;\r\n"))
        assertTrue(d.contains("FN:Рога и копыта 2\r\n"))
    }

    @Test
    fun missingFnAndN() {
        val card = "BEGIN:VCARD\r\nVERSION:4.0\r\nEMAIL:a@b.c\r\nEND:VCARD\r\n"
        val d = VCardEdit.apply(card, VCardEdit.Patch(fullName = "Анна Каренина"), now).data
        assertEquals("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Анна Каренина\r\nN:Каренина;Анна;;;\r\nEMAIL:a@b.c\r\nEND:VCARD\r\n", d)
    }

    @Test
    fun quotedPrintableLosesEncodingParam() {
        val card = "BEGIN:VCARD\r\nVERSION:2.1\r\nN:;;;;\r\nFN;CHARSET=UTF-8;ENCODING=QUOTED-PRINTABLE:=D0=90\r\nEND:VCARD\r\n"
        assertTrue(VCardEdit.apply(card, VCardEdit.Patch(fullName = "Б"), now).data.contains("\r\nFN:Б\r\n"))
    }

    @Test
    fun longValueIsFolded() {
        val long = "Очень длинное название организации ".repeat(4)
        val d = VCardEdit.apply("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:x\r\nEND:VCARD\r\n", VCardEdit.Patch(organization = long), now).data
        for (l in d.split("\r\n")) assertTrue("длиннее 75 октетов: $l", l.toByteArray().size <= 75)
        assertEquals(long.trim(), read(d).organization)
    }

    @Test(expected = VCardEdit.NoVCard::class)
    fun noVCard() {
        VCardEdit.apply("garbage", VCardEdit.Patch(fullName = "x"), now)
    }
}
