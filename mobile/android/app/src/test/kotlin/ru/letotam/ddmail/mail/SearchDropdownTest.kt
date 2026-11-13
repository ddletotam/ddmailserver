package ru.letotam.ddmail.mail

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import ru.letotam.ddmail.api.ContactDto
import ru.letotam.ddmail.api.ContactInfo
import ru.letotam.ddmail.api.MessageRef
import ru.letotam.ddmail.sync.Conversation
import ru.letotam.ddmail.sync.MessageHit

/** Выпадашка поиска (§4 «Поиск» десктопа, docs/mobile-layout.md). */
class SearchDropdownTest {
    private fun c(
        id: String,
        label: String,
        addr: String,
        subj: String = "",
        account: String = "a",
        refs: List<MessageRef> = emptyList(),
        spam: Boolean = false,
    ) = Conversation(
        accountKey = account, id = id, label = label, receivedBy = "me@a.ru",
        counterparts = listOf(ContactInfo(label, addr)), isGroup = false,
        lastDateTs = 0, lastSubject = subj, unreadCount = 0, totalCount = refs.size, messages = refs, spam = spam,
    )

    @Test
    fun emailLikeAsDesktop() {
        assertEquals("ivan@x.ru", parseEmailLike("  Ivan@X.ru "))
        assertNull(parseEmailLike("ivan"))
        assertNull(parseEmailLike("ivan@x"))
        assertNull(parseEmailLike("ivan@x."))
        assertNull(parseEmailLike("@x.ru"))
        assertNull(parseEmailLike("Иван <ivan@x.ru>"))
        assertNull(parseEmailLike("a@b.ru, c@d.ru"))
        // EMUI вставляет пробел после точки — такой «адрес» адресом не считается.
        assertNull(parseEmailLike("ivan@x. ru"))
    }

    @Test
    fun dialogsSayWhatMatched() {
        val all = listOf(
            c("1", "Отчёт бот", "bot@x.ru", "Привет"),
            c("2", "Иван", "otchet@y.ru", "Тема"),
            c("3", "Пётр", "p@z.ru", "Квартальный отчёт"),
        )
        val hits = searchConvHits(all, "отчёт")
        assertEquals(listOf("1", "3"), hits.map { it.conv.id })
        assertEquals("bot@x.ru", hits[0].detail)
        assertEquals("Квартальный отчёт", hits[1].detail)
        assertEquals("otchet@y.ru", searchConvHits(all, "OTCHET").single().detail)
        assertTrue(searchConvHits(all, "  ").isEmpty())
    }

    @Test
    fun contactsSkipAddressesOfFoundDialogs() {
        val book = listOf(
            ContactDto(1, "Иван Петров", listOf("ivan@x.ru", "IVAN@work.ru")),
            ContactDto(2, "Мария", listOf("maria@x.ru")),
            ContactDto(3, "", listOf("ivanov@y.ru")),
        )
        val hits = searchContacts(book, skip = setOf("ivan@x.ru"), q = "иван")
        assertEquals(listOf("ivan@work.ru"), hits.map { it.email })
        assertEquals(listOf("ivan@x.ru", "ivan@work.ru", "ivanov@y.ru"), searchContacts(book, emptySet(), "ivan").map { it.email })
        assertEquals(listOf("maria@x.ru"), searchContacts(book, emptySet(), "MARIA").map { it.email })
    }

    @Test
    fun messagesMapToTheirConversation() {
        val convs = listOf(
            c("d1", "Иван", "ivan@x.ru", account = "dd", refs = listOf(MessageRef("INBOX", 42, "<m42@x>"))),
            c("i1", "Пётр", "p@z.ru", account = "imap", refs = listOf(MessageRef("Sent", (2L shl 32) or 7, "<s7@z>"))),
            c("s1", "Спамер", "s@spam.ru", account = "dd", refs = listOf(MessageRef("INBOX", 99, "<spam@x>")), spam = true),
        )
        val hits = listOf(
            MessageHit("dd", "", 42, "m42@x", "Иван Иванов <ivan@x.ru>", "Счёт", 30),
            MessageHit("imap", "Sent", (2L shl 32) or 7, "", "p@z.ru", "Ответ", 20),
            // Нет uid в беседах, но совпал Message-ID.
            MessageHit("imap", "INBOX", 1, "<S7@z>", "p@z.ru", "Копия", 10),
            // Спам в поиск не попадает, чужие письма — тоже.
            MessageHit("dd", "", 99, "spam@x", "s@spam.ru", "Купи", 5),
            MessageHit("dd", "", 1000, "", "x@x.ru", "Нет беседы", 1),
        )
        val out = mapMessageHits(hits, convs)
        assertEquals(listOf(refOf("dd", "d1"), refOf("imap", "i1"), refOf("imap", "i1")), out.map { it.ref })
        assertEquals("Иван Иванов", out[0].from)
        assertEquals("p@z.ru", out[1].from)
    }

    private fun refOf(account: String, id: String) = c(id, "", "", account = account).ref
}
