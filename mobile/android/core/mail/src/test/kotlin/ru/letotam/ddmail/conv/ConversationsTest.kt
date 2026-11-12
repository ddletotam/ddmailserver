package ru.letotam.ddmail.conv

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Test
import ru.letotam.ddmail.conv.Conversations.conversationId
import ru.letotam.ddmail.conv.Conversations.participants

/** Порт `conversation_key_tests` десктопа (`imap.rs`) и сценарии склейки. */
class ConversationsTest {
    private fun v(vararg xs: String) = xs.toList()

    @Test
    fun participantsIgnoreRoles() {
        val a = participants("bob@x.ru", v("me@a.ru"), v("carol@x.ru"))
        val b = participants("me@a.ru", v("carol@x.ru"), v("bob@x.ru"))
        assertEquals(a, b)
        assertEquals(v("bob@x.ru", "carol@x.ru", "me@a.ru"), a)
    }

    @Test
    fun participantsNormaliseCaseAndDedup() {
        assertEquals(v("bob@x.ru", "me@a.ru"), participants("  Bob@X.RU ", v("me@a.ru", "bob@x.ru"), v("")))
    }

    @Test
    fun differentSetIsADifferentConversation() {
        assertNotEquals(participants("bob@x.ru", v("me@a.ru"), v()), participants("bob@x.ru", v("me@a.ru", "carol@x.ru"), v()))
    }

    @Test
    fun idMatchesTheOldFormatForASingleIdentity() {
        assertEquals("me@a.ru|bob@x.ru", conversationId(v("me@a.ru"), v("bob@x.ru"), "me@a.ru"))
        assertEquals("me@a.ru|group:bob@x.ru,carol@x.ru", conversationId(v("me@a.ru"), v("bob@x.ru", "carol@x.ru"), "me@a.ru"))
    }

    @Test
    fun idSeparatesConversationsThatDifferOnlyByMySecondIdentity() {
        val one = conversationId(v("me@a.ru"), v("bob@x.ru"), "me@a.ru")
        val two = conversationId(v("me@a.ru", "me@b.ru"), v("bob@x.ru"), "me@a.ru")
        assertNotEquals(one, two)
        assertEquals("me@a.ru+me@b.ru|bob@x.ru", two)
    }

    @Test
    fun idForMailBetweenMyOwnIdentities() {
        assertEquals("me@a.ru+me@b.ru|self", conversationId(v("me@a.ru", "me@b.ru"), v(), "me@a.ru"))
    }

    @Test
    fun idWhenIAmNotAmongTheVisibleAddresses() {
        assertEquals("me@a.ru|bob@x.ru", conversationId(v(), v("bob@x.ru"), "me@a.ru"))
    }

    @Test
    fun idKeepsTheEmptyFormWhenNoIdentityIsKnown() {
        assertEquals("|bob@x.ru", conversationId(v(), v("bob@x.ru"), ""))
    }

    // ── Сценарии склейки ──

    private var uid = 0L
    private fun msg(
        from: String,
        to: List<String>,
        cc: List<String> = emptyList(),
        date: Long,
        folder: String = "INBOX",
        mid: String = "",
        seen: Boolean = true,
        draft: Boolean = false,
        subject: String = "s",
        fromName: String = "",
        toNames: List<String> = to,
    ) = ConvMessage(folder, folder == "Sent", ++uid, fromName, from, to, toNames, cc, cc, subject, date, seen, draft, mid)

    @Test
    fun bccIncomingAndMyReplyLandInOneConversation() {
        // Входящее по bcc ({bob}) и наш ответ ({me, bob}) — одна беседа, а не
        // две строки с одним id (ошибка десктопа).
        val convs = Conversations.build(
            listOf(
                msg("bob@x.ru", v("list@x.ru"), date = 1, seen = false, fromName = "Боб"),
                msg("me@a.ru", v("bob@x.ru"), date = 2, folder = "Sent"),
            ).let { listOf(it[0].copy(toAddrs = v(), toNames = v()), it[1]) },
            ourAddrs = v("me@a.ru"),
            own = "me@a.ru",
        )
        assertEquals(1, convs.size)
        val c = convs[0]
        assertEquals("me@a.ru|bob@x.ru", c.id)
        assertEquals(2, c.totalCount)
        assertEquals(1, c.unreadCount)
        assertEquals("Боб", c.label)
        assertEquals("me@a.ru", c.receivedBy)
    }

    @Test
    fun duplicateByMessageIdPrefersSentCopy() {
        val convs = Conversations.build(
            listOf(
                msg("me@a.ru", v("bob@x.ru"), date = 5, folder = "INBOX", mid = "m1"),
                msg("me@a.ru", v("bob@x.ru"), date = 5, folder = "Sent", mid = "m1"),
            ),
            v("me@a.ru"), "me@a.ru",
        )
        assertEquals(1, convs[0].totalCount)
        assertEquals("Sent", convs[0].messages[0].folder)
    }

    @Test
    fun draftsAloneDoNotStartAConversation() {
        val convs = Conversations.build(
            listOf(msg("me@a.ru", v("zed@x.ru"), date = 1, folder = "Drafts", draft = true)),
            v("me@a.ru"), "me@a.ru",
        )
        assertEquals(0, convs.size)
    }

    @Test
    fun groupLabelCountsEveryoneAndTrimsSubject() {
        val convs = Conversations.build(
            listOf(msg("bob@x.ru", v("me@a.ru", "carol@y.ru"), date = 1, subject = "  План  ")),
            v("me@a.ru"), "me@a.ru",
        )
        val c = convs.single()
        assertEquals("me@a.ru|group:bob@x.ru,carol@y.ru", c.id)
        assertEquals("План (3 чел)", c.label)
        assertEquals(true, c.isGroup)
        assertEquals(Conversations.md5("bob@x.ru"), c.avatarHash)
    }

    @Test
    fun selfMailCounterpartIsSenderOfFirst() {
        val convs = Conversations.build(
            listOf(msg("me@a.ru", v("me@b.ru"), date = 1)),
            v("me@a.ru", "me@b.ru"), "me@a.ru",
        )
        val c = convs.single()
        assertEquals("me@a.ru+me@b.ru|self", c.id)
        assertEquals("me@a.ru", c.counterparts.single().addr)
        assertEquals("me@b.ru", c.receivedBy)
    }

    @Test
    fun myIdIsIdentityOfFirstMessage() {
        val convs = Conversations.build(
            listOf(
                msg("bob@x.ru", v("me@b.ru"), date = 1),
                msg("me@a.ru", v("bob@x.ru", "me@b.ru"), date = 2),
            ),
            v("me@a.ru", "me@b.ru"), "me@a.ru",
        )
        // Разные наборы — разные беседы; у первой айдентика — me@b.ru.
        assertEquals(2, convs.size)
        assertEquals("me@b.ru", convs.first { it.id == "me@b.ru|bob@x.ru" }.receivedBy)
    }

    @Test
    fun nameComesFromToWhenWeWroteFirst() {
        val convs = Conversations.build(
            listOf(msg("me@a.ru", v("bob@x.ru"), date = 1, toNames = v("Bob Smith <bob@x.ru>"))),
            v("me@a.ru"), "me@a.ru",
        )
        assertEquals("Bob Smith", convs.single().label)
    }

    @Test
    fun cleanDisplayName() {
        assertEquals("John Doe", Conversations.cleanDisplayName("John Doe <john@example.com>"))
        assertEquals("John Doe", Conversations.cleanDisplayName("John Doe (john@example.com)"))
        assertEquals("", Conversations.cleanDisplayName("john@example.com"))
    }

    @Test
    fun lastSubjectAndOrderByDate() {
        val convs = Conversations.build(
            listOf(
                msg("bob@x.ru", v("me@a.ru"), date = 10, subject = "new"),
                msg("bob@x.ru", v("me@a.ru"), date = 3, subject = "old"),
                msg("eve@x.ru", v("me@a.ru"), date = 5),
            ),
            v("me@a.ru"), "me@a.ru",
        )
        assertEquals(v("me@a.ru|bob@x.ru", "me@a.ru|eve@x.ru"), convs.map { it.id })
        assertEquals("new", convs[0].lastSubject)
        assertEquals(10L, convs[0].lastDateTs)
    }
}
