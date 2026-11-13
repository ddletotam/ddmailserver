package ru.letotam.ddmail.imap

import java.io.Closeable

/** Папка из LIST: имя как на сервере (modified UTF-7) и для людей. */
data class Mailbox(val name: String, val delimiter: String?, val attrs: Set<String>) {
    val display: String get() = ModifiedUtf7.decode(name)
    val selectable: Boolean get() = "\\NOSELECT" !in attrs && "\\NONEXISTENT" !in attrs
}

/** Флаги письма из FETCH (и его MODSEQ, если сервер CONDSTORE). */
data class FlagsItem(val uid: Long, val flags: Set<String>, val modseq: Long)

/** Состояние папки после SELECT и то, что сообщил QRESYNC. */
data class SelectInfo(
    val uidValidity: Long,
    val uidNext: Long,
    /** 0 — сервер без CONDSTORE для этой папки (`NOMODSEQ`). */
    val highestModSeq: Long,
    val exists: Long,
    /** QRESYNC: `VANISHED (EARLIER)` — удалённые с прошлого раза. */
    val vanished: List<LongRange>,
    /** QRESYNC: изменившиеся флаги с прошлого раза. */
    val changed: List<FlagsItem>,
)

/** QRESYNC-параметры SELECT: что клиент знает о папке. */
data class Qresync(val uidValidity: Long, val modseq: Long)

/**
 * Команды IMAP, которые нужны синхронизации и правкам (контракт §3–§4).
 * Только UID-команды: номера сообщений не используются нигде (сервер ddmail
 * их считал неверно — комментарий к `search_impl` десктопа).
 */
class ImapClient(val conn: ImapConnection) : Closeable {
    var qresyncEnabled = false
        private set

    val condstore get() = conn.has("CONDSTORE") || conn.has("QRESYNC")
    val qresync get() = qresyncEnabled
    val idleSupported get() = conn.has("IDLE")
    val uidplus get() = conn.has("UIDPLUS")
    val move get() = conn.has("MOVE")

    /**
     * LOGIN. Любой NO на LOGIN — «неверный логин или пароль» (контракт §7),
     * кроме `[UNAVAILABLE]`: сервер временно не пускает — это сеть.
     */
    fun login(user: String, password: String) {
        if (conn.has("LOGINDISABLED")) throw MailException.Rejected("Сервер запрещает вход по паролю")
        val st = try {
            conn.command(Arg.Raw("LOGIN"), Arg.Str(user), Arg.Str(password))
        } catch (e: MailException.Rejected) {
            if (conn.lastRejectCode == "UNAVAILABLE") throw MailException.Network(e.message ?: "Сервер временно недоступен")
            throw MailException.Auth(e.message ?: "")
        }
        if (st.codeName != "CAPABILITY") conn.refreshCapabilities()
        if (conn.has("QRESYNC")) {
            runCatching { conn.command(Arg.Raw("ENABLE QRESYNC")) }.onSuccess { qresyncEnabled = true }
        }
    }

    fun list(): List<Mailbox> {
        fun run(cmd: String): List<Mailbox> {
            val out = mutableListOf<Mailbox>()
            conn.command(Arg.Raw(cmd)) { d ->
                if (d.kind != "LIST") return@command
                val attrs = d.items.getOrNull(1)?.list?.mapNotNull { it.str?.uppercase() }?.toSet().orEmpty()
                val delim = d.items.getOrNull(2)?.str
                val name = d.items.getOrNull(3)?.str ?: return@command
                out += Mailbox(name, delim, attrs)
            }
            return out
        }
        val plain = run("LIST \"\" \"*\"")
        val hasSpecial = plain.any { m -> m.attrs.any { it in SPECIAL } }
        if (!hasSpecial && conn.has("SPECIAL-USE")) {
            runCatching { run("LIST (SPECIAL-USE) \"\" \"*\"") }.getOrNull()?.let { special ->
                val byName = special.associateBy { it.name }
                return plain.map { m -> byName[m.name]?.let { s -> m.copy(attrs = m.attrs + s.attrs) } ?: m }
            }
        }
        return plain
    }

    fun select(name: String, qresync: Qresync? = null): SelectInfo {
        var uidValidity = 0L
        var uidNext = 0L
        var modseq = 0L
        var exists = 0L
        val vanished = mutableListOf<LongRange>()
        val changed = mutableListOf<FlagsItem>()
        conn.untaggedStatus = { st ->
            when (st.codeName) {
                "UIDVALIDITY" -> uidValidity = st.code.getOrNull(1)?.long ?: 0
                "UIDNEXT" -> uidNext = st.code.getOrNull(1)?.long ?: 0
                "HIGHESTMODSEQ" -> modseq = st.code.getOrNull(1)?.long ?: 0
                "NOMODSEQ" -> modseq = 0
            }
        }
        val param = when {
            qresync != null && qresyncEnabled && qresync.modseq > 0 ->
                " (QRESYNC (${qresync.uidValidity} ${qresync.modseq}))"
            condstore -> " (CONDSTORE)"
            else -> ""
        }
        try {
            val args = listOfNotNull(Arg.Raw("SELECT"), Arg.Str(name), param.takeIf { it.isNotEmpty() }?.let { Arg.Raw(it.trim()) })
            conn.command(*args.toTypedArray()) { d ->
                when (d.kind) {
                    "EXISTS" -> exists = d.number ?: exists
                    "VANISHED" -> vanished += vanishedRanges(d)
                    "FETCH" -> flagsItem(fetchItems(d))?.let { changed += it }
                }
            }
        } finally {
            conn.untaggedStatus = null
        }
        return SelectInfo(uidValidity, uidNext, modseq, exists, vanished, changed)
    }

    /** Заголовки писем по UID (для бесед), порциями. */
    fun fetchHeaders(uids: Collection<Long>): List<MessageHeader> {
        val out = mutableListOf<MessageHeader>()
        for (set in UidSet.chunks(uids)) {
            conn.command(Arg.Raw("UID FETCH $set ${Headers.FETCH_ITEMS}")) { d ->
                if (d.kind == "FETCH") Headers.parse(fetchItems(d))?.let { out += it }
            }
        }
        return out
    }

    /** Новые письма: `UID FETCH <from>:*` (сервер отдаёт и последнее, если оно старше — отсекаем). */
    fun fetchHeadersFrom(fromUid: Long): List<MessageHeader> {
        val out = mutableListOf<MessageHeader>()
        conn.command(Arg.Raw("UID FETCH $fromUid:* ${Headers.FETCH_ITEMS}")) { d ->
            if (d.kind == "FETCH") Headers.parse(fetchItems(d))?.takeIf { it.uid >= fromUid }?.let { out += it }
        }
        return out
    }

    /**
     * Флаги в диапазоне: с CONDSTORE — только изменившиеся после [changedSince]
     * (и `VANISHED`, если включён QRESYNC); без — все.
     */
    fun fetchFlags(set: String, changedSince: Long = 0): Pair<List<FlagsItem>, List<LongRange>> {
        val items = mutableListOf<FlagsItem>()
        val vanished = mutableListOf<LongRange>()
        val mod = when {
            changedSince > 0 && qresyncEnabled -> " (CHANGEDSINCE $changedSince VANISHED)"
            changedSince > 0 && condstore -> " (CHANGEDSINCE $changedSince)"
            else -> ""
        }
        conn.command(Arg.Raw("UID FETCH $set (UID FLAGS)$mod")) { d ->
            when (d.kind) {
                "FETCH" -> flagsItem(fetchItems(d))?.let { items += it }
                "VANISHED" -> vanished += vanishedRanges(d)
            }
        }
        return items to vanished
    }

    fun search(criteria: String): List<Long> {
        val out = mutableListOf<Long>()
        conn.command(Arg.Raw("UID SEARCH $criteria")) { d ->
            if (d.kind == "SEARCH") d.items.drop(1).forEach { v -> v.long?.let { out += it } }
        }
        return out
    }

    /**
     * Письма, где [query] есть в теме, отправителе или тексте (секция
     * «Сообщения» поиска). Не-ASCII — с `CHARSET UTF-8`, строка уходит
     * литералом.
     */
    fun searchText(query: String): List<Long> {
        val out = mutableListOf<Long>()
        val head = if (query.all { it.code < 128 }) "UID SEARCH OR OR SUBJECT" else "UID SEARCH CHARSET UTF-8 OR OR SUBJECT"
        conn.command(Arg.Raw(head), Arg.Str(query), Arg.Raw("FROM"), Arg.Str(query), Arg.Raw("BODY"), Arg.Str(query)) { d ->
            if (d.kind == "SEARCH") d.items.drop(1).forEach { v -> v.long?.let { out += it } }
        }
        return out
    }

    /** Письмо по Message-ID (копия в «Отправленных», §5). */
    fun searchMessageId(messageId: String): List<Long> {
        val out = mutableListOf<Long>()
        conn.command(Arg.Raw("UID SEARCH HEADER Message-ID"), Arg.Str("<${Headers.normalizeMsgId(messageId)}>")) { d ->
            if (d.kind == "SEARCH") d.items.drop(1).forEach { v -> v.long?.let { out += it } }
        }
        return out
    }

    fun store(uids: Collection<Long>, flags: String, add: Boolean) {
        val op = if (add) "+FLAGS.SILENT" else "-FLAGS.SILENT"
        for (set in UidSet.chunks(uids)) conn.command(Arg.Raw("UID STORE $set $op ($flags)"))
    }

    /** Удалить: `\Deleted` + `UID EXPUNGE` (UIDPLUS), иначе `EXPUNGE`. */
    fun delete(uids: Collection<Long>) {
        if (uids.isEmpty()) return
        store(uids, "\\Deleted", add = true)
        if (uidplus) {
            for (set in UidSet.chunks(uids)) conn.command(Arg.Raw("UID EXPUNGE $set"))
        } else {
            conn.command(Arg.Raw("EXPUNGE"))
        }
    }

    /** Перенести: `UID MOVE`, без MOVE — `UID COPY` и удаление. */
    fun moveTo(uids: Collection<Long>, dest: String) {
        if (uids.isEmpty()) return
        if (move) {
            for (set in UidSet.chunks(uids)) conn.command(Arg.Raw("UID MOVE $set"), Arg.Str(dest))
        } else {
            for (set in UidSet.chunks(uids)) conn.command(Arg.Raw("UID COPY $set"), Arg.Str(dest))
            delete(uids)
        }
    }

    /** Размеры писем (`RFC822.SIZE`) — чтобы не качать заранее гигантские. */
    fun sizes(uids: Collection<Long>): Map<Long, Long> {
        val out = mutableMapOf<Long, Long>()
        for (set in UidSet.chunks(uids)) {
            conn.command(Arg.Raw("UID FETCH $set (UID RFC822.SIZE)")) { d ->
                if (d.kind != "FETCH") return@command
                val it = fetchItems(d)
                val uid = it["UID"]?.long ?: return@command
                it["RFC822.SIZE"]?.long?.let { s -> out[uid] = s }
            }
        }
        return out
    }

    /** Письмо целиком без `\Seen` (`BODY.PEEK[]`). */
    fun fetchRaw(uids: Collection<Long>): Map<Long, ByteArray> {
        val out = mutableMapOf<Long, ByteArray>()
        for (set in UidSet.chunks(uids, 10)) {
            conn.command(Arg.Raw("UID FETCH $set (UID BODY.PEEK[])")) { d ->
                if (d.kind != "FETCH") return@command
                val it = fetchItems(d)
                val uid = it["UID"]?.long ?: return@command
                it.section("BODY[]")?.let { b -> out[uid] = b }
            }
        }
        return out
    }

    /** APPEND; UID нового письма, если сервер сказал (UIDPLUS `APPENDUID`). */
    fun append(mailbox: String, flags: String, message: ByteArray): Long? {
        val st = conn.command(Arg.Raw("APPEND"), Arg.Str(mailbox), Arg.Raw("($flags)"), Arg.Literal(message))
        return if (st.codeName == "APPENDUID") st.code.getOrNull(2)?.long else null
    }

    /** `GETMETADATA` (RFC 5464): значение записи или `null`. Сервер ddmail так отдаёт айдентики. */
    fun getMetadata(entry: String): ByteArray? {
        if (!conn.has("METADATA") && !conn.has("METADATA-SERVER")) return null
        var value: ByteArray? = null
        runCatching {
            conn.command(Arg.Raw("GETMETADATA"), Arg.Str(""), Arg.Raw(entry)) { d ->
                if (d.kind != "METADATA") return@command
                val pairs = d.items.getOrNull(2)?.list.orEmpty()
                var i = 0
                while (i + 1 < pairs.size) {
                    if (pairs[i].str.equals(entry, ignoreCase = true)) value = pairs[i + 1].bytes
                    i += 2
                }
            }
        }
        return value
    }

    fun idle(maxMs: Long, onData: (Resp.Data) -> Boolean) = conn.idle(maxMs, onData)

    fun noop() {
        conn.command(Arg.Raw("NOOP"))
    }

    fun logout() {
        runCatching { conn.command(Arg.Raw("LOGOUT")) }
    }

    override fun close() {
        conn.close()
    }

    companion object {
        val SPECIAL = setOf("\\SENT", "\\DRAFTS", "\\JUNK", "\\TRASH", "\\ARCHIVE", "\\ALL", "\\FLAGGED")

        fun connect(host: String, port: Int, timeoutMs: Int = 25_000): ImapClient = ImapClient(ImapConnection.open(host, port, timeoutMs))

        private fun vanishedRanges(d: Resp.Data): List<LongRange> {
            val last = d.items.lastOrNull()?.str ?: return emptyList()
            return UidSet.parse(last)
        }

        private fun flagsItem(m: Map<String, IVal>): FlagsItem? {
            val uid = m["UID"]?.long ?: return null
            if ("FLAGS" !in m) return null
            val modseq = m["MODSEQ"]?.list?.firstOrNull()?.long ?: 0
            return FlagsItem(uid, m.flags(), modseq)
        }
    }
}
