package ru.letotam.ddmail.mail

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import ru.letotam.ddmail.api.ContactInfo
import ru.letotam.ddmail.compose.SentTarget
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

    private fun t(vararg addrs: String, account: String = "", from: String = "me@a.ru") =
        SentTarget(account, from, addrs.toSet())

    @Test
    fun matchesTargetForSelfChat() {
        val self = c("me@a.ru|self", "Я", "", "x").copy(counterparts = emptyList())
        assertTrue(matchesTarget(self, t("me@a.ru")))
        // Так её отдаёт сервер ddmail: собеседник — свой же адрес (замер 2026-11-13).
        val served = self.copy(counterparts = listOf(ContactInfo("", "me@a.ru")))
        assertTrue(matchesTarget(served, t("me@a.ru"), own = setOf("me@a.ru")))
    }

    @Test
    fun matchesTargetWithCcAndOwnAddress() {
        val pair = c("me@a.ru|bob@x.ru|c@y.ru", "Bob", "bob@x.ru", "x")
            .copy(counterparts = listOf(ContactInfo("", "bob@x.ru"), ContactInfo("", "c@y.ru")))
        assertTrue(matchesTarget(pair, t("bob@x.ru", "c@y.ru")))
        // Себе в копию — свой адрес в собеседники не входит.
        assertTrue(matchesTarget(pair, t("bob@x.ru", "c@y.ru", "me@a.ru"), own = setOf("me@a.ru")))
        assertTrue(!matchesTarget(pair, t("bob@x.ru")))
        assertTrue(!matchesTarget(pair.copy(spam = true), t("bob@x.ru", "c@y.ru")))
    }

    /**
     * Аудит 2026-11-14 №3: письмо info@ → info@ ушло через ddmail, а
     * открылась беседа IMAP Mail.ru «info@ → denis@» — тот же собеседник,
     * другая учётка. Нужна беседа той учётки, через которую письмо ушло.
     */
    @Test
    fun matchesTargetOnlyInSendingAccount() {
        val imap = c("denis@m.ru|info@h.ru", "info", "info@h.ru", "x")
            .copy(accountKey = "imap:denis@m.ru", receivedBy = "denis@m.ru")
        val ddmail = c("info@h.ru|self", "info", "info@h.ru", "x")
            .copy(accountKey = "ddmail:lucky", receivedBy = "info@h.ru")
        val target = t("info@h.ru", account = "ddmail:lucky", from = "info@h.ru")
        val own = setOf("info@h.ru", "denis@m.ru")
        assertTrue(!matchesTarget(imap, target, own))
        assertTrue(matchesTarget(ddmail, target, own))
        // Список — в любом порядке: выбирается беседа своей учётки.
        assertEquals(ddmail, listOf(imap, ddmail).first { matchesTarget(it, target, own) })
    }

    /** Аудит 2026-11-14 №11: адресаты своего пузыря — как `recipients_tip` десктопа. */
    @Test
    fun recipientsTipAsDesktop() {
        val b = ru.letotam.ddmail.api.MessageBody(uid = 1, isOutgoing = true, to = listOf("anna@x.ru", " olga@x.ru "))
        assertEquals(listOf("Кому: anna@x.ru, olga@x.ru"), recipientsTip(b))
        assertEquals(
            listOf("Кому: anna@x.ru, olga@x.ru", "Копия: Бух <buh@x.ru>"),
            recipientsTip(b.copy(cc = listOf("Бух <buh@x.ru>"))),
        )
        assertEquals(listOf("Кому: —"), recipientsTip(b.copy(to = emptyList())))
        assertEquals(emptyList<String>(), recipientsTip(b.copy(isOutgoing = false)))
    }

    /** Свой адрес — часть набора: та же учётка, другой свой адрес — другая беседа. */
    @Test
    fun matchesTargetOnlyFromSameOwnAddress() {
        val viaAlias = c("alias@a.ru|bob@x.ru", "Bob", "bob@x.ru", "x").copy(receivedBy = "alias@a.ru")
        val viaMe = viaAlias.copy(id = "me@a.ru|bob@x.ru", receivedBy = "me@a.ru")
        assertTrue(!matchesTarget(viaAlias, t("bob@x.ru")))
        assertTrue(matchesTarget(viaMe, t("bob@x.ru")))
        // Свой второй адрес среди адресатов (себе в копию) — беседа может числить им.
        assertTrue(matchesTarget(viaAlias, t("bob@x.ru", "alias@a.ru"), own = setOf("me@a.ru", "alias@a.ru")))
    }
}
