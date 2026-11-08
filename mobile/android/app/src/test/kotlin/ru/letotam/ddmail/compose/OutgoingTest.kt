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
}
