package ru.letotam.ddmail.imap

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/** «Сообщения» поиска на IMAP: тема, отправитель или текст. */
class SearchTextTest {
    @Test
    fun asciiQueryIsQuoted() {
        FakeImapServer(
            "IMAP4rev1 LITERAL+",
            listOf("UID SEARCH OR OR SUBJECT \"invoice\" FROM \"invoice\" BODY \"invoice\"" to "* SEARCH 4 9\nTAG OK\n"),
        ).use { srv ->
            assertEquals(listOf(4L, 9L), srv.client().searchText("invoice"))
            assertTrue(srv.unexpected.toString(), srv.unexpected.isEmpty())
        }
    }

    @Test
    fun cyrillicGoesAsUtf8Literal() {
        FakeImapServer(
            "IMAP4rev1 LITERAL+",
            // Сервер склеивает литералы с остатком строки — «{n+}» и сами байты.
            listOf("UID SEARCH CHARSET UTF-8 OR OR SUBJECT {8+}счёт FROM {8+}счёт BODY {8+}счёт" to "* SEARCH 2\nTAG OK\n"),
        ).use { srv ->
            assertEquals(listOf(2L), srv.client().searchText("счёт"))
            assertTrue(srv.unexpected.toString(), srv.unexpected.isEmpty())
        }
    }
}
