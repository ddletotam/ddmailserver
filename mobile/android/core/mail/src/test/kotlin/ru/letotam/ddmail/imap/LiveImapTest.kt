package ru.letotam.ddmail.imap

import org.junit.Assume.assumeTrue
import org.junit.Test
import ru.letotam.ddmail.mime.MimeParse

/**
 * Проверка на живом ящике — только вручную, с переменными окружения
 * `DDMAIL_LIVE_HOST`, `DDMAIL_LIVE_USER`, `DDMAIL_LIVE_PASS` (пароль в
 * коде и в git не хранится). Без них тест пропускается. Печатает только
 * числа и роли папок — не содержимое писем.
 */
class LiveImapTest {
    @Test
    fun syncInboxOnRealServer() {
        val host = System.getenv("DDMAIL_LIVE_HOST")
        assumeTrue(host != null)
        val c = ImapClient.connect(host!!, 993)
        c.login(System.getenv("DDMAIL_LIVE_USER")!!, System.getenv("DDMAIL_LIVE_PASS")!!)
        println("caps: ${c.conn.capabilities.sorted()}")
        val roles = FolderRoles.detect(c.list())
        println("roles: ${roles.mapValues { ModifiedUtf7.decode(it.value) }}")
        val d = FolderSync.sync(c, roles.getValue(FolderRole.Inbox), 50, null, emptyList())
        println("inbox: added=${d.added.size} state=${d.state} undated=${d.added.count { it.dateTs == 0L }}")
        val again = FolderSync.sync(c, roles.getValue(FolderRole.Inbox), 50, d.state, d.added.map { it.uid })
        println("again: added=${again.added.size} flags=${again.flags.size} removed=${again.removed.size}")
        d.added.lastOrNull()?.let { h ->
            val raw = c.fetchRaw(listOf(h.uid))[h.uid]!!
            val b = MimeParse.body(raw, h.uid, "INBOX", emptyList())
            println("body: html=${b.html?.length} text=${b.text?.length} atts=${b.attachments.size} mid=${b.messageId == h.messageId}")
        }
        println("metadata: ${c.getMetadata("/shared/vendor/ddmail/identities")?.size}")
        c.logout()
        c.close()
    }
}
