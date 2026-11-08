package ru.letotam.ddmail.mail

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import ru.letotam.ddmail.api.ContactInfo
import ru.letotam.ddmail.sync.Conversation

class SearchTest {
    private fun c(id: String, label: String, addr: String, subj: String) = Conversation(
        accountKey = "", id = id, label = label, receivedBy = "me@a.ru",
        counterparts = listOf(ContactInfo(label, addr)), isGroup = false,
        lastDateTs = 0, lastSubject = subj, unreadCount = 0, totalCount = 1, messages = emptyList(),
    )

    private val all = listOf(
        c("1", "Отчёт бот", "bot@x.ru", "Привет"),
        c("2", "Иван", "otchet@y.ru", "Тема"),
        c("3", "Пётр", "p@z.ru", "Квартальный ОТЧЁТ"),
    )

    @Test
    fun nameThenAddressThenSubjectCaseFolded() {
        assertEquals(listOf("1", "3"), searchConversations(all, "отчёт").map { it.id })
        assertEquals(listOf("2"), searchConversations(all, "OTCHET").map { it.id })
    }

    @Test
    fun matchesTargetForSelfChat() {
        val self = c("me@a.ru|self", "Я", "", "x").copy(counterparts = emptyList())
        assertTrue(matchesTarget(self, setOf("me@a.ru")))
    }
}
