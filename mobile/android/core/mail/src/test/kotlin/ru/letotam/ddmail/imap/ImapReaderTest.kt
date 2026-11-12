package ru.letotam.ddmail.imap

import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ImapReaderTest {
    private fun reader(s: String) = ImapReader(s.toByteArray(Charsets.UTF_8).inputStream())

    @Test
    fun fetchWithLiteralAndHeaderSection() {
        val hdr = "From: Bob <bob@x.ru>\r\n\r\n"
        val r = reader("* 12 FETCH (UID 7 FLAGS (\\Seen \\Flagged) BODY[HEADER.FIELDS (FROM TO)] {${hdr.length}}\r\n$hdr MODSEQ (42))\r\n")
        val d = r.read() as Resp.Data
        assertEquals("FETCH", d.kind)
        assertEquals(12L, d.number)
        val m = fetchItems(d)
        assertEquals(7L, m["UID"]?.long)
        assertEquals(setOf("\\SEEN", "\\FLAGGED"), m.flags())
        assertArrayEquals(hdr.toByteArray(), m.section("BODY[HEADER"))
        assertEquals(42L, m["MODSEQ"]?.list?.first()?.long)
    }

    @Test
    fun statusWithCodeAndFreeText() {
        val r = reader("* OK [UIDVALIDITY 3857529045] UIDs [valid\r\nA001 NO [AUTHENTICATIONFAILED] Invalid credentials (failure)\r\n")
        val st = r.read() as Resp.Status
        assertNull(st.tag)
        assertEquals("UIDVALIDITY", st.codeName)
        assertEquals(3857529045L, st.code[1].long)
        assertEquals("UIDs [valid", st.text)
        val tagged = r.read() as Resp.Status
        assertEquals("A001", tagged.tag)
        assertEquals("NO", tagged.status)
        assertEquals("AUTHENTICATIONFAILED", tagged.codeName)
    }

    @Test
    fun quotedEscapesNilAndContinuation() {
        val r = reader("* LIST (\\HasNoChildren \\Sent) \"/\" \"Sent \\\"x\\\"\"\r\n* LIST () NIL INBOX\r\n+ idling\r\n")
        val a = r.read() as Resp.Data
        assertEquals("LIST", a.kind)
        assertEquals("Sent \"x\"", a.items[3].str)
        val b = r.read() as Resp.Data
        assertTrue(b.items[2] is IVal.Nil)
        assertEquals("INBOX", b.items[3].str)
        assertTrue(r.read() is Resp.Continue)
    }

    @Test
    fun vanishedAndSearch() {
        val r = reader("* VANISHED (EARLIER) 300:310,405\r\n* SEARCH 2 84 882\r\n* SEARCH\r\n")
        val v = r.read() as Resp.Data
        assertEquals("VANISHED", v.kind)
        val ranges = UidSet.parse(v.items.last().str!!)
        assertTrue(UidSet.contains(ranges, 305))
        assertTrue(UidSet.contains(ranges, 405))
        val s = r.read() as Resp.Data
        assertEquals(listOf(2L, 84L, 882L), s.items.drop(1).map { it.long })
        assertEquals(1, (r.read() as Resp.Data).items.size)
    }

    @Test
    fun envelopeWithLiteralSubjectAndGroups() {
        val subj = "Тема"
        val n = subj.toByteArray().size
        val line = "* 1 FETCH (UID 5 ENVELOPE (\"Mon, 1 Jan 2024 10:00:00 +0300\" {$n}\r\n$subj " +
            "((\"Bob\" NIL \"bob\" \"x.ru\")) NIL NIL ((NIL NIL \"team\" NIL)(NIL NIL \"me\" \"a.ru\")(NIL NIL NIL NIL)) NIL NIL NIL \"<m1@x>\"))\r\n"
        val d = reader(line).read() as Resp.Data
        val env = fetchItems(d)["ENVELOPE"]!!.list
        assertEquals("Тема", env[1].str)
        assertEquals(10, env.size)
    }

    @Test
    fun uidSetFormat() {
        assertEquals("1:3,7,9:10", UidSet.format(listOf(10, 1, 2, 3, 7, 9)))
        assertEquals("", UidSet.format(emptyList()))
        assertEquals(listOf(5L..Long.MAX_VALUE), UidSet.parse("5:*"))
    }

    @Test
    fun modifiedUtf7() {
        val sent = "&BB4EQgQ,BEAEMAQyBDsENQQ9BD0ESwQ1-"
        assertEquals("Отправленные", ModifiedUtf7.decode(sent))
        assertEquals(sent, ModifiedUtf7.encode("Отправленные"))
        assertEquals("A&B", ModifiedUtf7.decode("A&-B"))
        assertEquals("A&-B", ModifiedUtf7.encode("A&B"))
        assertEquals("INBOX.Спам", ModifiedUtf7.decode(ModifiedUtf7.encode("INBOX.Спам")))
    }
}
