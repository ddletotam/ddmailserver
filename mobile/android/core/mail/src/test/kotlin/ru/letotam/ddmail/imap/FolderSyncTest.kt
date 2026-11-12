package ru.letotam.ddmail.imap

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** Инкрементальный синк на записанных сессиях (контракт §11). */
class FolderSyncTest {
    private fun hdr(uid: Long, seen: Boolean = false, from: String = "bob@x.ru"): String {
        val h = "From: Bob <$from>\r\nSubject: s$uid\r\n\r\n"
        val flags = if (seen) "\\Seen" else ""
        val (mb, host) = from.split('@')
        return "* $uid FETCH (UID $uid FLAGS ($flags) INTERNALDATE \"01-Jan-2024 10:00:00 +0000\" " +
            "ENVELOPE (\"Mon, 1 Jan 2024 10:00:0$uid +0000\" \"s$uid\" ((\"Bob\" NIL \"$mb\" \"$host\")) NIL NIL ((NIL NIL \"me\" \"a.ru\")) NIL NIL NIL \"<m$uid@x>\") " +
            "BODY[HEADER.FIELDS (FROM TO CC DATE SUBJECT MESSAGE-ID IN-REPLY-TO REFERENCES CONTENT-TYPE)] {${h.length}}\n$h)\n"
    }

    private fun select(uv: Long, next: Long, modseq: Long?, exists: Int, extra: String = "") =
        "* $exists EXISTS\n* OK [UIDVALIDITY $uv] ok\n* OK [UIDNEXT $next] ok\n" +
            (if (modseq != null) "* OK [HIGHESTMODSEQ $modseq] ok\n" else "* OK [NOMODSEQ] none\n") + extra + "TAG OK [READ-WRITE] done\n"

    @Test
    fun firstSyncTakesWindowByUid() {
        FakeImapServer(
            "IMAP4rev1 LITERAL+ CONDSTORE",
            listOf(
                "SELECT \"INBOX\" (CONDSTORE)" to select(7, 10, 100, 4),
                "UID SEARCH ALL" to "* SEARCH 3 5 8 9\nTAG OK\n",
                "UID FETCH 8:9 " to hdr(8) + hdr(9, seen = true) + "TAG OK\n",
            ),
        ).use { srv ->
            val c = srv.client()
            val d = FolderSync.sync(c, "INBOX", window = 2, prev = null, cached = emptyList())
            assertTrue(d.reset)
            assertEquals(listOf(8L, 9L), d.added.map { it.uid })
            assertTrue(d.added[1].seen)
            assertEquals(FolderState(7, 10, 100, 9), d.state)
            assertTrue(srv.unexpected.toString(), srv.unexpected.isEmpty())
        }
    }

    @Test
    fun uidValidityChangeDropsFolder() {
        FakeImapServer(
            "IMAP4rev1 LITERAL+",
            listOf(
                "SELECT \"INBOX\"" to select(8, 3, null, 1),
                "UID SEARCH ALL" to "* SEARCH 2\nTAG OK\n",
                "UID FETCH 2 " to hdr(2) + "TAG OK\n",
            ),
        ).use { srv ->
            val d = FolderSync.sync(srv.client(), "INBOX", 10, FolderState(7, 10, 0, 9), cached = listOf(8L, 9L))
            assertTrue(d.reset)
            assertEquals(setOf(8L, 9L), d.removed)
            assertEquals(listOf(2L), d.added.map { it.uid })
        }
    }

    @Test
    fun condstoreFetchesOnlyChangedFlagsAndNewMail() {
        FakeImapServer(
            "IMAP4rev1 LITERAL+ CONDSTORE",
            listOf(
                "SELECT \"INBOX\" (CONDSTORE)" to select(7, 12, 120, 4),
                "UID FETCH 8:9 (UID FLAGS) (CHANGEDSINCE 100)" to "* 3 FETCH (UID 8 FLAGS (\\Seen) MODSEQ (110))\nTAG OK\n",
                "UID SEARCH UID 8:9" to "* SEARCH 8\nTAG OK\n",
                "UID FETCH 10:* " to hdr(11) + "TAG OK\n",
            ),
        ).use { srv ->
            val d = FolderSync.sync(srv.client(), "INBOX", 10, FolderState(7, 10, 100, 9), cached = listOf(8L, 9L))
            assertFalse(d.reset)
            assertEquals(listOf(FlagsItem(8, setOf("\\SEEN"), 110)), d.flags)
            assertEquals(setOf(9L), d.removed)
            assertEquals(listOf(11L), d.added.map { it.uid })
            assertEquals(FolderState(7, 12, 120, 11), d.state)
            assertTrue(srv.unexpected.toString(), srv.unexpected.isEmpty())
        }
    }

    @Test
    fun unchangedFolderWithCondstoreCostsSelectAndSearch() {
        FakeImapServer(
            "IMAP4rev1 LITERAL+ CONDSTORE",
            listOf(
                "SELECT \"INBOX\" (CONDSTORE)" to select(7, 10, 100, 2),
                "UID SEARCH UID 8:9" to "* SEARCH 8 9\nTAG OK\n",
            ),
        ).use { srv ->
            val d = FolderSync.sync(srv.client(), "INBOX", 10, FolderState(7, 10, 100, 9), cached = listOf(8L, 9L))
            assertTrue(d.added.isEmpty() && d.flags.isEmpty() && d.removed.isEmpty())
            assertTrue(srv.unexpected.toString(), srv.unexpected.isEmpty())
            // Только SELECT и SEARCH: без FETCH флагов и новых.
            assertEquals(2, srv.commands.count { it.startsWith("SELECT") || it.startsWith("UID") })
        }
    }

    @Test
    fun qresyncReportsVanishedInSelect() {
        FakeImapServer(
            "IMAP4rev1 LITERAL+ CONDSTORE QRESYNC ENABLE",
            listOf(
                "ENABLE QRESYNC" to "* ENABLED QRESYNC\nTAG OK\n",
                "LOGIN" to "TAG OK logged in\n",
                "CAPABILITY" to "* CAPABILITY IMAP4rev1 LITERAL+ CONDSTORE QRESYNC ENABLE\nTAG OK\n",
                "SELECT \"INBOX\" (QRESYNC (7 100))" to select(
                    7, 10, 130, 1,
                    "* VANISHED (EARLIER) 8\n* 1 FETCH (UID 9 FLAGS (\\Seen) MODSEQ (125))\n",
                ),
            ),
        ).use { srv ->
            val c = srv.client()
            c.login("me", "pw")
            assertTrue(c.qresync)
            val d = FolderSync.sync(c, "INBOX", 10, FolderState(7, 10, 100, 9), cached = listOf(8L, 9L))
            assertEquals(setOf(8L), d.removed)
            assertEquals(listOf(9L), d.flags.map { it.uid })
            assertTrue(srv.unexpected.toString(), srv.unexpected.isEmpty())
        }
    }

    @Test
    fun withoutCondstoreFlagsOfWholeWindow() {
        FakeImapServer(
            "IMAP4rev1 LITERAL+",
            listOf(
                "SELECT \"INBOX\"" to select(7, 10, null, 2),
                "UID FETCH 8:9 (UID FLAGS)" to "* 1 FETCH (UID 8 FLAGS ())\n* 2 FETCH (UID 9 FLAGS (\\Seen))\nTAG OK\n",
                "UID SEARCH UID 8:9" to "* SEARCH 8 9\nTAG OK\n",
            ),
        ).use { srv ->
            val d = FolderSync.sync(srv.client(), "INBOX", 10, FolderState(7, 10, 0, 9), cached = listOf(8L, 9L))
            assertEquals(2, d.flags.size)
            assertTrue(d.removed.isEmpty())
            assertTrue(srv.unexpected.toString(), srv.unexpected.isEmpty())
        }
    }

    @Test
    fun windowTrimsOldest() {
        FakeImapServer(
            "IMAP4rev1 LITERAL+ CONDSTORE",
            listOf(
                "SELECT \"INBOX\" (CONDSTORE)" to select(7, 12, 100, 3),
                "UID SEARCH UID 8:9" to "* SEARCH 8 9\nTAG OK\n",
                "UID FETCH 10:* " to hdr(11) + "TAG OK\n",
            ),
        ).use { srv ->
            val d = FolderSync.sync(srv.client(), "INBOX", 2, FolderState(7, 10, 100, 9), cached = listOf(8L, 9L))
            assertEquals(setOf(8L), d.removed)
            assertEquals(listOf(11L), d.added.map { it.uid })
        }
    }

    @Test
    fun loginFailureIsAuth() {
        FakeImapServer("IMAP4rev1", listOf("LOGIN" to "TAG NO [AUTHENTICATIONFAILED] Invalid credentials\n")).use { srv ->
            val c = srv.client()
            val e = runCatching { c.login("me", "bad") }.exceptionOrNull()
            assertTrue(e is MailException.Auth)
        }
    }

    @Test
    fun appendSendsLiteralAndReadsAppendUid() {
        FakeImapServer(
            "IMAP4rev1 LITERAL+ UIDPLUS",
            listOf("APPEND \"Sent\" (\\Seen) {5+}hello" to "TAG OK [APPENDUID 38505 3955] done\n"),
        ).use { srv ->
            assertEquals(3955L, srv.client().append("Sent", "\\Seen", "hello".toByteArray()))
            assertTrue(srv.unexpected.toString(), srv.unexpected.isEmpty())
        }
    }

    @Test
    fun headersFromFetch() {
        FakeImapServer("IMAP4rev1 LITERAL+", listOf("UID FETCH 5 " to hdr(5, from = "Bob@X.ru") + "TAG OK\n")).use { srv ->
            val h = srv.client().fetchHeaders(listOf(5L)).single()
            assertEquals("bob@x.ru", h.fromAddr)
            assertEquals("Bob", h.fromName)
            assertEquals(listOf("me@a.ru"), h.toAddrs)
            assertEquals("m5@x", h.messageId)
            assertEquals(1704103205L, h.dateTs)
        }
    }

    @Test
    fun folderRolesBySpecialUseAndNames() {
        val boxes = listOf(
            Mailbox("INBOX", "/", emptySet()),
            Mailbox("&BB4EQgQ,BEAEMAQyBDsENQQ9BD0ESwQ1-", "/", emptySet()),
            Mailbox("Bulk", "/", setOf("\\JUNK")),
            Mailbox("Черновики", "/", emptySet()),
        )
        val r = FolderRoles.detect(boxes)
        assertEquals("&BB4EQgQ,BEAEMAQyBDsENQQ9BD0ESwQ1-", r[FolderRole.Sent])
        assertEquals("Bulk", r[FolderRole.Junk])
        assertEquals("Черновики", r[FolderRole.Drafts])
        assertEquals(FolderRole.Junk, FolderRole.unpackRole(FolderRole.Junk.pack(77)))
        assertEquals(77L, FolderRole.unpackUid(FolderRole.Junk.pack(77)))
    }
}
