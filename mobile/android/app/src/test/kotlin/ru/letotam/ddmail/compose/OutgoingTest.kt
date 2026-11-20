package ru.letotam.ddmail.compose

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import ru.letotam.ddmail.api.ContactInfo
import ru.letotam.ddmail.api.Identity
import ru.letotam.ddmail.api.MessageBody
import ru.letotam.ddmail.sync.Conversation

/** Ветки отправки — §1 десктопного контракта. */
class OutgoingTest {
    private val ids = listOf(
        Identity("me@a.ru", "Я", isDefault = true),
        Identity("work@b.ru", "Я"),
    )

    private fun conv(group: Boolean = false, receivedBy: String = "work@b.ru") = Conversation(
        accountKey = "k", id = "c", label = "Bob", receivedBy = receivedBy,
        counterparts = listOf(ContactInfo("Bob", "bob@x.ru")) + if (group) listOf(ContactInfo("Carol", "carol@y.ru")) else emptyList(),
        isGroup = group, lastDateTs = 0, lastSubject = "Привет", unreadCount = 0, totalCount = 2, messages = emptyList(),
    )

    private fun body(subject: String, from: String, out: Boolean = false, to: List<String> = emptyList(), cc: List<String> = emptyList()) =
        MessageBody(uid = 1, subject = subject, fromAddr = from, isOutgoing = out, to = to, cc = cc, messageId = "<m1@x>", references = listOf("<m0@x>"))

    @Test
    fun implicitReplyGoesToCounterpartsFromReceivingIdentity() {
        val last = body("Вопрос", "bob@x.ru")
        val r = build(Branch.Implicit(conv(), last, last), Draft(text = "ок"), ids) as Built.Ok
        assertEquals(listOf("bob@x.ru"), r.request.to)
        assertEquals("Re: Вопрос", r.request.subject)
        assertEquals("<m1@x>", r.request.inReplyTo)
        assertEquals("<m0@x> <m1@x>", r.request.references)
        assertEquals("\"Я\" <work@b.ru>", r.request.from)
    }

    @Test
    fun explicitToAndSubjectBeatDerivation() {
        val r = build(Branch.Implicit(conv(), null, null), Draft(text = "x", to = "solo@z.ru", subject = "Своя"), ids) as Built.Ok
        assertEquals(listOf("solo@z.ru"), r.request.to)
        assertEquals("Своя", r.request.subject)
    }

    @Test
    fun reSubjectIsNotDoubled() {
        val last = body("RE: тема", "bob@x.ru")
        val r = build(Branch.Implicit(conv(), last, last), Draft(text = "x"), ids) as Built.Ok
        assertEquals("RE: тема", r.request.subject)
    }

    @Test
    fun explicitReplyInGroupIsReplyAllMinusOwn() {
        val o = body("Тема", "bob@x.ru", to = listOf("Я <me@a.ru>", "carol@y.ru"), cc = listOf("Dan <dan@z.ru>"))
        val r = build(Branch.Reply(o, isGroup = true), Draft(text = "x"), ids) as Built.Ok
        assertEquals(listOf("bob@x.ru", "carol@y.ru", "dan@z.ru"), r.request.to)
    }

    @Test
    fun explicitReplyOneToOneGoesToSenderOnly() {
        val o = body("Тема", "bob@x.ru", to = listOf("me@a.ru", "carol@y.ru"))
        val r = build(Branch.Reply(o, isGroup = false), Draft(text = "x"), ids) as Built.Ok
        assertEquals(listOf("bob@x.ru"), r.request.to)
    }

    @Test
    fun forwardNeedsExplicitTo() {
        val o = body("Отчёт", "bob@x.ru")
        assertTrue(build(Branch.Forward(o), Draft(), ids) is Built.Refused)
        val r = build(Branch.Forward(o), Draft(to = "x@y.ru"), ids) as Built.Ok
        assertEquals("Fwd: Отчёт", r.request.subject)
        assertEquals("", r.request.inReplyTo)
    }

    @Test
    fun attachmentOnlyIsValidEmptyIsNot() {
        val c = Branch.Implicit(conv(), null, null)
        assertTrue(build(c, Draft(), ids) is Built.Refused)
        val r = build(c, Draft(attachments = listOf(PendingAttachment("a.pdf", "application/pdf", byteArrayOf(1)))), ids)
        assertTrue(r is Built.Ok)
    }

    @Test
    fun selfChatRepliesToSender() {
        val c = conv().copy(counterparts = emptyList(), receivedBy = "me@a.ru")
        val last = body("Себе", "me@a.ru")
        val r = build(Branch.Implicit(c, last, last), Draft(text = "x"), ids) as Built.Ok
        assertEquals(listOf("me@a.ru"), r.request.to)
    }

    @Test
    fun textBecomesEscapedHtml() {
        assertEquals("<div>a &lt;b&gt;<br>c</div>", textToHtml("a <b>\nc"))
        assertEquals("bob@x.ru", bareAddress("Bob <Bob@X.ru>"))
    }

    /** Аудит 2026-11-14 №10: подсказки «Кому»/«Тема» — ровно то, что уйдёт. */
    @Test
    fun autoHintsShowWhatWillBeSent() {
        val last = body("Вопрос", "bob@x.ru")
        assertEquals(AutoHints("bob@x.ru, carol@y.ru", "Re: Вопрос"), autoHints(Branch.Implicit(conv(group = true), last, last), ids))
        // Явный ответ 1:1 — отправителю письма, «Re:» его темы.
        assertEquals(AutoHints("bob@x.ru", "Re: Тема"), autoHints(Branch.Reply(body("Тема", "bob@x.ru"), isGroup = false), ids))
        // Новое письмо: адресат — из поиска, автотемы нет (не «Re: …»).
        assertEquals(AutoHints("new@z.ru", ""), autoHints(Branch.New, ids, newTo = "new@z.ru"))
        // Пересылка: «Кому» не выводится, тема — «Fwd: …».
        assertEquals(AutoHints("", "Fwd: Тема"), autoHints(Branch.Forward(body("Тема", "bob@x.ru")), ids))
    }

    /** Документ групп (десктоп §1в): work@ и boss@ — одна строгая группа «Работа», отправитель boss@; me@ — по умолчанию. */
    private val groups = ru.letotam.ddmail.sync.IdentityGroups.parse(
        kotlinx.serialization.json.Json.parseToJsonElement(
            """{"default_group":"g2","groups":[
              {"id":"g1","name":"Работа","sender":"boss@b.ru","strict":true,"addresses":[{"email":"work@b.ru"},{"email":"boss@b.ru"}]},
              {"id":"g2","name":"Я","sender":"me@a.ru","addresses":[{"email":"me@a.ru"}]}]}""",
        ),
    )
    private val idsWithBoss = ids + Identity("boss@b.ru", "Шеф")

    @Test
    fun replyGoesFromGroupSenderOfReceivingAddress() {
        val last = body("Вопрос", "bob@x.ru")
        val r = build(Branch.Implicit(conv(), last, last), Draft(text = "ок"), idsWithBoss, groups = groups) as Built.Ok
        assertEquals("\"Шеф\" <boss@b.ru>", r.request.from)
        val e = build(Branch.Reply(last, false, receivedBy = "work@b.ru"), Draft(text = "ок"), idsWithBoss, groups = groups) as Built.Ok
        assertEquals("\"Шеф\" <boss@b.ru>", e.request.from)
    }

    @Test
    fun newMailAndForeignMailGoFromDefaultGroup() {
        val n = build(Branch.New, Draft(text = "x", to = "z@z.ru"), idsWithBoss, groups = groups) as Built.Ok
        assertEquals("\"Я\" <me@a.ru>", n.request.from)
        val last = body("Рассылка", "list@x.ru")
        val r = build(Branch.Implicit(conv(receivedBy = "list@x.ru"), last, last), Draft(text = "x"), idsWithBoss, groups = groups) as Built.Ok
        assertEquals("\"Я\" <me@a.ru>", r.request.from)
        // Сменили группу по умолчанию — новое письмо уходит с её отправителя.
        val w = build(Branch.New, Draft(text = "x", to = "z@z.ru"), idsWithBoss, groups = groups.withDefault("g1")) as Built.Ok
        assertEquals("\"Шеф\" <boss@b.ru>", w.request.from)
    }

    @Test
    fun strictGroupRefusesForeignContext() {
        val last = body("Личное", "bob@x.ru")
        val r = build(Branch.Implicit(conv(receivedBy = "me@a.ru"), last, last), Draft(text = "x", from = "work@b.ru"), idsWithBoss, groups = groups)
        assertTrue(r is Built.Refused)
        // Строгая группа по умолчанию и чужой адрес — первая нестрогая.
        val strictDefault = groups.withDefault("g1")
        val f = build(Branch.Implicit(conv(receivedBy = "list@x.ru"), last, last), Draft(text = "x"), idsWithBoss, groups = strictDefault) as Built.Ok
        assertEquals("\"Я\" <me@a.ru>", f.request.from)
        // В выпадашке строгой группы в чужом контексте нет.
        assertEquals(listOf("me@a.ru"), fromChoices(idsWithBoss, groups, "me@a.ru", null).map { it.email })
    }

    /** «Отправить без темы?»: у нового письма автотемы нет, у ответа на письмо без темы — тоже. */
    @Test
    fun subjectStaysEmptyWithoutBaseSubject() {
        val n = build(Branch.New, Draft(text = "x", to = "z@z.ru"), ids) as Built.Ok
        assertEquals("", n.request.subject)
        val last = body("", "bob@x.ru")
        val r = build(Branch.Implicit(conv().copy(lastSubject = ""), last, last), Draft(text = "x"), ids) as Built.Ok
        assertEquals("", r.request.subject)
        val ok = build(Branch.Implicit(conv(), body("Вопрос", "bob@x.ru"), null), Draft(text = "x"), ids) as Built.Ok
        assertEquals("Re: Вопрос", ok.request.subject)
    }
}
