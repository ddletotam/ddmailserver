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
    /** `DDMAIL_LIVE_DAV` — базовый адрес CalDAV/CardDAV; печатает только числа. */
    @Test
    fun davOnRealServer(): Unit = kotlinx.coroutines.runBlocking {
        val base = System.getenv("DDMAIL_LIVE_DAV")
        assumeTrue(base != null)
        val c = ru.letotam.ddmail.dav.DavClient(okhttp3.OkHttpClient(), System.getenv("DDMAIL_LIVE_USER")!!, System.getenv("DDMAIL_LIVE_PASS")!!)
        val cals = ru.letotam.ddmail.dav.DavPim.collections(c, base!!, "calendar")
        println("calendars: ${cals.size} writable=${cals.count { it.canWrite }}")
        val now = System.currentTimeMillis()
        cals.forEach { col ->
            val evs = ru.letotam.ddmail.dav.DavPim.events(c, col.url, now - 30L * 86_400_000, now + 30L * 86_400_000)
            println("  events: ${evs.size} rrule=${evs.count { it.event.rrule.isNotEmpty() }} etag=${evs.count { it.etag != null }}")
        }
        val books = runCatching { ru.letotam.ddmail.dav.DavPim.collections(c, System.getenv("DDMAIL_LIVE_CARDDAV") ?: base, "addressbook") }
        println("books: ${books.getOrNull()?.size} ${books.exceptionOrNull()?.message}")
        books.getOrNull()?.forEach { b -> println("  contacts: ${ru.letotam.ddmail.dav.DavPim.contacts(c, b.url).size}") }
    }

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
