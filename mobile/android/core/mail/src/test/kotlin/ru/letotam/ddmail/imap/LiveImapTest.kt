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

    /**
     * Тот же ящик через IMAP и через сервер ddmail (`DDMAIL_LIVE_API`) —
     * id бесед обязаны совпасть (контракт §3). Печатает только счётчики.
     */
    @Test
    fun conversationIdsMatchServer(): Unit = kotlinx.coroutines.runBlocking {
        val api = System.getenv("DDMAIL_LIVE_API")
        assumeTrue(api != null && System.getenv("DDMAIL_LIVE_HOST") != null)
        val user = System.getenv("DDMAIL_LIVE_USER")!!
        val pass = System.getenv("DDMAIL_LIVE_PASS")!!
        val http = okhttp3.OkHttpClient.Builder().readTimeout(120, java.util.concurrent.TimeUnit.SECONDS).build()
        val token = ru.letotam.ddmail.api.DdmailApi(api!!, http).login(user, pass).token
        val tokens = object : ru.letotam.ddmail.api.TokenSource {
            override suspend fun current() = token
            override suspend fun update(token: String) {}
        }
        val session = ru.letotam.ddmail.api.SessionApi(api, http, tokens)
        val server = session.conversationsDelta(0).conversations
        val idents = session.identities().map { it.email.lowercase() }

        val c = ImapClient.connect(System.getenv("DDMAIL_LIVE_HOST")!!, 993)
        c.login(user, pass)
        val roles = FolderRoles.detect(c.list())
        val msgs = mutableListOf<ru.letotam.ddmail.conv.ConvMessage>()
        for (role in listOf(FolderRole.Inbox, FolderRole.Sent, FolderRole.Drafts)) {
            val name = roles[role] ?: continue
            val d = FolderSync.sync(c, name, role.window, null, emptyList())
            d.added.sortedBy { it.uid }.forEach { h ->
                msgs += ru.letotam.ddmail.conv.ConvMessage(
                    name, role == FolderRole.Sent, role.pack(h.uid), h.fromName, h.fromAddr, h.toAddrs, h.toNames,
                    h.ccAddrs, h.ccNames, h.subject, h.dateTs, h.seen, h.draft || role == FolderRole.Drafts, h.messageId,
                )
            }
        }
        c.logout()
        val serverIds = server.map { it.id }.toSet()
        println("server: ${server.size} convs, identities: ${idents.size}, imap messages: ${msgs.size}")
        // Своя учётка — какой адрес ни возьми из айдентик, ids с сервером сравниваются.
        for ((i, own) in idents.withIndex()) {
            val built = ru.letotam.ddmail.conv.Conversations.build(msgs, idents, own)
            val ids = built.map { it.id }.toSet()
            val both = ids.intersect(serverIds)
            // Сверка содержимого: число писем у совпавших по id.
            val sameCount = built.filter { it.id in both }.count { b -> server.first { it.id == b.id }.totalCount == b.totalCount }
            println("own#$i: imap=${ids.size} common=${both.size} onlyImap=${(ids - serverIds).size} onlyServer=${(serverIds - ids).size} sameCount=$sameCount")
            if (both.size > server.size * 9 / 10) {
                // Разбор расхождений без адресов: та же «чужая» часть, другая «своя»?
                val serverByOthers = server.groupBy { it.id.substringAfter('|') }
                for (id in ids - serverIds) {
                    val others = id.substringAfter('|')
                    val s = serverByOthers[others]
                    val mineN = id.substringBefore('|').split('+').size
                    val b = built.first { it.id == id }
                    println("  onlyImap: mine=$mineN group=${others.startsWith("group:")} serverSameOthers=${s?.size ?: 0} serverMineN=${s?.map { it.id.substringBefore('|').split('+').size }} msgs=${b.totalCount}/${s?.map { it.totalCount }}")
                }
            }
        }
    }

    /**
     * Отправка самому себе (`DDMAIL_LIVE_SELF` — адрес ящика, `DDMAIL_LIVE_SMTP`
     * — сервер:порт) и поиск копии в «Отправленных» по Message-ID; не нашлась —
     * APPEND, как в ImapEngine. Чужим адресам не пишет.
     */
    @Test
    fun sendToSelfAndCopyToSent() {
        val self = System.getenv("DDMAIL_LIVE_SELF")
        val smtp = System.getenv("DDMAIL_LIVE_SMTP")
        assumeTrue(self != null && smtp != null && System.getenv("DDMAIL_LIVE_HOST") != null)
        val user = System.getenv("DDMAIL_LIVE_USER")!!
        val pass = System.getenv("DDMAIL_LIVE_PASS")!!
        val req = ru.letotam.ddmail.api.SendRequest(
            from = self!!, to = listOf(self), subject = "ddmail Android: проверка SMTP",
            html = "<div>Проверка отправки с внешней учётки (Android, 0.0.24).</div>", text = "Проверка отправки с внешней учётки (Android, 0.0.24).",
        )
        val out = ru.letotam.ddmail.smtp.Smtp.build(req)
        val (host, port) = smtp!!.split(':')
        ru.letotam.ddmail.smtp.Smtp.send(ru.letotam.ddmail.smtp.SmtpConfig(host, port.toInt(), true, user, pass), out)
        println("sent: id-domain=${out.messageId.substringAfter('@') == self.substringAfter('@')}")
        val c = ImapClient.connect(System.getenv("DDMAIL_LIVE_HOST")!!, 993)
        c.login(user, pass)
        val sent = FolderRoles.detect(c.list())[FolderRole.Sent]!!
        var found = false
        for (i in 0 until 4) {
            Thread.sleep(5_000)
            c.select(sent)
            if (c.searchMessageId(out.messageId).isNotEmpty()) {
                found = true
                break
            }
        }
        println("copy in Sent by server: $found")
        if (!found) println("append uid: ${c.append(sent, "\\Seen", out.bytes())}")
        c.select("INBOX")
        println("in INBOX: ${c.searchMessageId(out.messageId).size}")
        c.logout()
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
