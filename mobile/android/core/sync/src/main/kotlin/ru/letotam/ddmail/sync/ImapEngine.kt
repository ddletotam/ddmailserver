package ru.letotam.ddmail.sync

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.awaitCancellation
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.isActive
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import kotlinx.serialization.builtins.ListSerializer
import ru.letotam.ddmail.account.ImapAccount
import ru.letotam.ddmail.api.Binary
import ru.letotam.ddmail.api.ConversationDto
import ru.letotam.ddmail.api.Identity
import ru.letotam.ddmail.api.MessageBody
import ru.letotam.ddmail.api.MessageRef
import ru.letotam.ddmail.api.SendRequest
import ru.letotam.ddmail.conv.ConvMessage
import ru.letotam.ddmail.conv.Conversations
import ru.letotam.ddmail.db.CacheDao
import ru.letotam.ddmail.db.ImapFolderEntity
import ru.letotam.ddmail.db.ImapHeaderEntity
import ru.letotam.ddmail.db.MessageRefEntity
import ru.letotam.ddmail.imap.FolderRole
import ru.letotam.ddmail.imap.FolderRoles
import ru.letotam.ddmail.imap.FolderState
import ru.letotam.ddmail.imap.FolderSync
import ru.letotam.ddmail.imap.ImapClient
import ru.letotam.ddmail.imap.MailException
import ru.letotam.ddmail.imap.MessageHeader
import ru.letotam.ddmail.imap.Resp
import ru.letotam.ddmail.mime.MimeParse
import ru.letotam.ddmail.smtp.Smtp
import ru.letotam.ddmail.smtp.SmtpConfig
import ru.letotam.ddmail.sync.CacheRows.toDomain
import ru.letotam.ddmail.sync.CacheRows.toEntity
import java.io.File
import java.security.MessageDigest

/**
 * Внешняя учётка IMAP + SMTP (mobile-external-accounts §3–§6) — второй
 * [MailSource] рядом с сервером ddmail.
 *
 * - Синк по папкам INBOX, Sent, Drafts, Junk ([FolderSync]: UIDVALIDITY,
 *   UIDNEXT, CONDSTORE/QRESYNC с запасными путями) в кэш заголовков Room;
 *   беседы строятся на устройстве из всего кэша ([Conversations]) и кладутся в
 *   те же таблицы, что у ddmail; Junk — в пространство `<учётка>#spam` с id
 *   `spam:…`, как спам сервера.
 * - Письма в кэше — под упакованным UID (`роль shl 32 or uid`, [FolderRole.pack]):
 *   у разных папок UID пересекаются, а ключ тел — (учётка, uid).
 * - Правки оптимистичны в кэше беседы; кэш заголовков меняется только после
 *   ответа сервера; отказ — беседы пересобираются из заголовков (откат) и
 *   ошибка уходит наверх (у десктопа она глоталась).
 * - Одно командное соединение на учётку, переиспользуется ~1 мин; IDLE —
 *   своё ([watch]).
 */
class ImapEngine(
    val account: ImapAccount,
    private val password: suspend () -> String?,
    private val dao: CacheDao,
    private val scope: CoroutineScope,
    /** Каталог кэша сырых писем (вложения и `cid:` без повторной закачки). */
    rawDir: File,
) : MailSource {
    override val accountKey: String = account.key
    override val title: String get() = account.email
    private val spamKey = "$accountKey#spam"
    private val kIdentities = "$accountKey:identities"
    private val kMetaTs = "$accountKey:identities_ts"

    private val syncLock = Mutex()
    private val poolLock = Mutex()
    private var client: ImapClient? = null
    private var lastUse = 0L
    private var closer: Job? = null
    private val raw = RawCache(File(rawDir, md5(accountKey)))

    /** Папки по ролям — с прошлого синка (из кэша до первого). */
    @Volatile private var roles: Map<FolderRole, String> = emptyMap()

    override val conversations: Flow<List<Conversation>> = CacheRows.conversations(dao, accountKey, spamKey)

    override val identities: Flow<List<Identity>> = dao.observeMeta(kIdentities).map { s -> mergeIdentities(s) }

    override val spamRules: Boolean get() = false

    override suspend fun canSpam(): Boolean = roles().containsKey(FolderRole.Junk)

    private fun mergeIdentities(serverJson: String?): List<Identity> {
        val base = account.ownAddresses.map { Identity(it, account.name, "", isDefault = it == account.email.lowercase()) }
        val server = serverJson?.let { runCatching { CacheRows.json.decodeFromString(identSer, it) }.getOrNull() }.orEmpty()
            .filter { it.email.contains('@') }
            .map { it.copy(email = it.email.lowercase(), isDefault = false) }
        return (base + server).distinctBy { it.email }
    }

    private suspend fun ourAddrs(): List<String> = identities.first().map { it.email }

    private suspend fun roles(): Map<FolderRole, String> {
        if (roles.isEmpty()) {
            roles = dao.imapFolders(accountKey).mapNotNull { f -> FolderRole.ofCode(f.role)?.let { it to f.name } }.toMap()
        }
        return roles
    }

    // ── Соединение ──

    private fun openClient(pw: String): ImapClient {
        val c = ImapClient.connect(account.imapHost, account.imapPort)
        try {
            c.login(account.login, pw)
        } catch (e: Exception) {
            c.close()
            throw e
        }
        return c
    }

    /**
     * Командное соединение: живое переиспользуется, после минуты простоя
     * закрывается. Обрыв на переиспользованном — один повтор на новом.
     */
    internal suspend fun <T> withClient(block: suspend (ImapClient) -> T): T = poolLock.withLock {
        withContext(Dispatchers.IO) {
            closer?.cancel()
            val pw = password() ?: throw MailException.Auth("нет пароля")
            val fresh = client == null || System.currentTimeMillis() - lastUse > REUSE_MS
            if (fresh) {
                client?.let { old -> runCatching { old.logout(); old.close() } }
                client = openClient(pw)
            }
            try {
                block(client!!).also { lastUse = System.currentTimeMillis() }
            } catch (e: MailException.Network) {
                runCatching { client?.close() }
                client = null
                if (fresh) throw e
                client = openClient(pw)
                block(client!!).also { lastUse = System.currentTimeMillis() }
            } catch (e: Exception) {
                runCatching { client?.close() }
                client = null
                throw e
            } finally {
                closer = scope.launch {
                    delay(REUSE_MS)
                    poolLock.withLock {
                        withContext(Dispatchers.IO) {
                            client?.let { runCatching { it.logout(); it.close() } }
                            client = null
                        }
                    }
                }
            }
        }
    }

    override fun close() {
        closer?.cancel()
        val c = client
        client = null
        if (c != null) scope.launch(Dispatchers.IO) { runCatching { c.logout(); c.close() } }
    }

    // ── Синк ──

    override suspend fun sync() = syncLock.withLock {
        if (closed) return@withLock
        var changed = false
        withClient { c ->
            val found = FolderRoles.detect(c.list())
            val states = dao.imapFolders(accountKey).associateBy { it.role }
            for (role in SYNCED) {
                val name = found[role]
                val prev = states[role.code]
                if (name == null) {
                    if (prev != null) {
                        dao.dropImapFolderRole(accountKey, role.code, role.pack(0))
                        changed = true
                    }
                    continue
                }
                // Папку роли переименовали или подменили — это другая папка.
                val prevState = prev?.takeIf { it.name == name }?.let { FolderState(it.uidValidity, it.uidNext, it.highestModSeq, it.lastUid) }
                val cachedFlags = dao.imapFlags(accountKey, role.code).associateBy { it.uid }
                val d = FolderSync.sync(c, name, role.window, prevState, cachedFlags.keys)
                val reset = d.reset || prevState == null
                // Без CONDSTORE приходят флаги всего окна — в кэш и в пересборку
                // идут только действительно изменившиеся.
                val flags = d.flags.map { CacheDao.ImapFlags(it.uid, "\\SEEN" in it.flags, "\\FLAGGED" in it.flags, "\\DRAFT" in it.flags) }
                    .filter { f -> cachedFlags[f.uid] != f }
                if (reset || d.added.isNotEmpty() || flags.isNotEmpty() || d.removed.isNotEmpty()) changed = true
                if (closed) return@withClient
                dao.applyImapFolder(
                    accountKey, role.code, reset,
                    d.added.map { it.toEntity(role) },
                    flags,
                    d.removed,
                    ImapFolderEntity(accountKey, role.code, name, d.state.uidValidity, d.state.uidNext, d.state.highestModSeq, d.state.lastUid),
                    role.pack(0),
                )
            }
            roles = found
            // Айдентики сервера ddmail по IMAP (GETMETADATA, как у десктопа) — раз в 6 ч.
            val ts = dao.meta(kMetaTs)?.toLongOrNull() ?: 0
            if (System.currentTimeMillis() - ts > 6 * 3600_000L) {
                c.getMetadata(METADATA_IDENTITIES)?.let { bytes ->
                    extractJsonArray(String(bytes, Charsets.UTF_8))?.let {
                        if (it != dao.meta(kIdentities)) changed = true
                        dao.setMeta(kIdentities, it)
                    }
                }
                dao.setMeta(kMetaTs, System.currentTimeMillis().toString())
            }
        }
        if (!closed && (changed || dao.meta("$accountKey:built") == null)) rebuild()
    }

    /** Беседы заново из кэша заголовков (после синка и для отката правок). */
    suspend fun rebuild() {
        val headers = dao.imapHeaders(accountKey)
        val names = dao.imapFolders(accountKey).associate { it.role to it.name }
        val ours = ourAddrs()
        val own = account.email.lowercase()
        fun msgs(filter: (Int) -> Boolean) = headers.filter { filter(it.role) }
            // Порядок входа как у десктопа: INBOX, Sent, Drafts; внутри — по UID.
            .sortedWith(compareBy({ it.role }, { it.uid }))
            .map { it.toConv(names[it.role].orEmpty()) }
        val ham = Conversations.build(msgs { it != FolderRole.Junk.code }, ours, own)
        val spam = Conversations.build(msgs { it == FolderRole.Junk.code }, ours, own).map { it.copy(id = SPAM_PREFIX + it.id) }
        dao.replaceAll(accountKey, ham.map { it.toEntity(accountKey) }, refs(accountKey, ham))
        dao.replaceAll(spamKey, spam.map { it.toEntity(spamKey) }, refs(spamKey, spam))
        dao.setMeta("$accountKey:built", "1")
    }

    private fun refs(key: String, convs: List<ConversationDto>) =
        convs.flatMap { c -> c.messages.map { MessageRefEntity(key, c.id, it.folder, it.uid, it.messageId, it.seen) } }

    private fun ImapHeaderEntity.toConv(folder: String): ConvMessage {
        val s = CacheRows.strings
        fun list(j: String) = runCatching { CacheRows.json.decodeFromString(s, j) }.getOrDefault(emptyList())
        return ConvMessage(
            folder = folder,
            isSent = role == FolderRole.Sent.code,
            uid = (role.toLong() shl 32) or uid,
            fromName = fromName,
            fromAddr = fromAddr,
            toAddrs = list(toAddrsJson),
            toNames = list(toNamesJson),
            ccAddrs = list(ccAddrsJson),
            ccNames = list(ccNamesJson),
            subject = subject,
            dateTs = dateTs,
            seen = seen,
            // Всё в «Черновиках» — черновик, даже без флага \Draft.
            draft = draft || role == FolderRole.Drafts.code,
            messageId = messageId,
            updatedAt = updatedAt,
        )
    }

    private fun MessageHeader.toEntity(role: FolderRole): ImapHeaderEntity {
        val s = CacheRows.strings
        val j = CacheRows.json
        return ImapHeaderEntity(
            accountKey, role.code, uid, seen, flagged, draft, subject, fromName, fromAddr,
            j.encodeToString(s, toAddrs), j.encodeToString(s, toNames), j.encodeToString(s, ccAddrs), j.encodeToString(s, ccNames),
            dateTs, messageId, inReplyTo, j.encodeToString(s, references), hasAttachments, System.currentTimeMillis(),
        )
    }

    // ── Тела ──

    override suspend fun bodies(refs: List<MessageRef>): List<MessageBody> {
        if (refs.isEmpty()) return emptyList()
        val cached = dao.bodies(accountKey, refs.map { it.uid }).associateBy { it.uid }
            .mapValues { it.value.toDomain() }.filterValues { !it.isBlank }
        val missing = refs.filter { it.uid !in cached }
        val fetched = if (missing.isEmpty()) emptyMap() else try {
            fetchBodies(missing).associateBy { it.uid }
        } catch (e: MailException.Auth) {
            throw e
        } catch (e: MailException) {
            emptyMap()
        }
        return refs.mapNotNull { cached[it.uid] ?: fetched[it.uid] }
    }

    private suspend fun fetchBodies(refs: List<MessageRef>, maxSize: Long = 0): List<MessageBody> {
        val ours = ourAddrs()
        val r = roles()
        val out = mutableListOf<MessageBody>()
        withClient { c ->
            for ((role, list) in refs.groupBy { FolderRole.unpackRole(it.uid) }) {
                val name = role?.let { r[it] } ?: continue
                c.select(name)
                var uids = list.map { FolderRole.unpackUid(it.uid) }
                if (maxSize > 0) {
                    val sizes = c.sizes(uids)
                    uids = uids.filter { (sizes[it] ?: 0) <= maxSize }
                }
                val raws = c.fetchRaw(uids)
                for (ref in list) {
                    val bytes = raws[FolderRole.unpackUid(ref.uid)] ?: continue
                    raw.put(ref.uid, bytes)
                    out += runCatching { MimeParse.body(bytes, ref.uid, name, ours) }.getOrNull() ?: continue
                }
            }
        }
        if (out.isNotEmpty()) dao.putBodies(out.map { it.toEntity(accountKey) })
        return out
    }

    override suspend fun prefetchBodies(allowed: suspend () -> Boolean, batch: Int): Int {
        val have = dao.cachedBodyUids(accountKey).toHashSet()
        val convs = dao.observeConversations(accountKey).first()
        val refs = dao.observeRefs(accountKey).first().groupBy { it.conversationId }
        val queue = convs.flatMap { c -> refs[c.id].orEmpty().reversed() }
            .filter { it.uid !in have }
            .map { MessageRef(it.folder, it.uid, it.messageId, it.seen) }
        var n = 0
        // Письма целиком (с вложениями) — порции маленькие, большие письма
        // заранее не качаются: откроют — скачаются (память телефона не резиновая).
        for (chunk in queue.chunked(PREFETCH_BATCH)) {
            if (!allowed()) break
            n += try {
                fetchBodies(chunk, maxSize = PREFETCH_MAX_SIZE).size
            } catch (e: MailException.Auth) {
                throw e
            } catch (e: MailException) {
                break
            }
        }
        return n
    }

    /** Сырое письмо: из кэша или повторным `BODY.PEEK[]`. */
    private suspend fun rawOf(packed: Long): ByteArray {
        raw.get(packed)?.let { return it }
        val role = FolderRole.unpackRole(packed) ?: throw MailException.Rejected("Письмо не найдено")
        val name = roles()[role] ?: throw MailException.Rejected("Папка письма не найдена")
        val bytes = withClient { c ->
            c.select(name)
            c.fetchRaw(listOf(FolderRole.unpackUid(packed)))[FolderRole.unpackUid(packed)]
        } ?: throw MailException.Rejected("Письмо удалено на сервере")
        raw.put(packed, bytes)
        return bytes
    }

    override suspend fun part(uid: Long, cid: String): Binary {
        val bytes = rawOf(uid)
        val (b, type) = mime { MimeParse.part(bytes, cid) } ?: throw MailException.Rejected("Часть письма не найдена")
        return Binary(b, type)
    }

    override suspend fun attachment(uid: Long, index: Int): Binary {
        val bytes = rawOf(uid)
        val (b, type, _) = mime { MimeParse.attachment(bytes, index) } ?: throw MailException.Rejected("Вложение не найдено")
        return Binary(b, type)
    }

    /** Разбор MIME: битое письмо — ошибка действия, а не падение приложения. */
    private suspend fun <T> mime(block: () -> T): T = withContext(Dispatchers.Default) {
        try {
            block()
        } catch (e: Exception) {
            throw MailException.Rejected("Письмо повреждено: ${e.message ?: e.javaClass.simpleName}")
        }
    }

    // ── Правки ──

    private fun cacheKey(c: Conversation) = if (c.spam) spamKey else accountKey

    /** Ссылки по ролям папок. */
    private fun byRole(refs: Collection<MessageRef>): Map<FolderRole, List<Long>> =
        refs.groupBy { FolderRole.unpackRole(it.uid) }.filterKeys { it != null }
            .mapKeys { it.key!! }.mapValues { e -> e.value.map { FolderRole.unpackUid(it.uid) } }

    /** Правка на сервере; отказ — беседы из заголовков (откат оптимистичного) и ошибка наверх. */
    private suspend fun <T> edit(block: suspend (ImapClient, Map<FolderRole, String>) -> T): T {
        val r = roles()
        return try {
            withClient { c -> block(c, r) }
        } catch (e: Exception) {
            runCatching { rebuild() }
            throw e
        }
    }

    private suspend fun setSeen(c: Conversation, refs: List<MessageRef>, seen: Boolean) {
        if (refs.isEmpty()) return
        val groups = byRole(refs)
        edit { client, r ->
            for ((role, uids) in groups) {
                val name = r[role] ?: continue
                client.select(name)
                client.store(uids, "\\Seen", add = seen)
            }
        }
        groups.forEach { (role, uids) -> dao.markImapSeen(accountKey, role.code, uids, seen) }
        if (!seen) rebuild()
    }

    override suspend fun markRead(c: Conversation) {
        val unseen = c.messages.filter { !it.seen }
        dao.markRead(cacheKey(c), c.id)
        setSeen(c, unseen, true)
    }

    override suspend fun markUnread(c: Conversation) {
        // Последнее входящее (ссылки идут по дате), а не своё из «Отправленных».
        val last = c.messages.lastOrNull { FolderRole.unpackRole(it.uid) == FolderRole.Inbox } ?: c.messages.lastOrNull() ?: return
        dao.markUnread(cacheKey(c), c.id, last.uid)
        setSeen(c, listOf(last), false)
    }

    /** Удаление необратимо: `\Deleted` + `UID EXPUNGE` (без UIDPLUS — `EXPUNGE`). */
    override suspend fun deleteConversations(convs: Collection<Conversation>) {
        convs.groupBy { cacheKey(it) }.forEach { (k, list) -> dao.deleteConversations(k, list.map { it.id }) }
        val groups = byRole(convs.flatMap { it.messages })
        edit { client, r ->
            for ((role, uids) in groups) {
                val name = r[role] ?: continue
                client.select(name)
                client.delete(uids)
            }
        }
        groups.forEach { (role, uids) -> dao.dropImapMessages(accountKey, role.code, uids, role.pack(0)) }
        rebuild()
    }

    /** «В спам» — перенос в Junk (`UID MOVE`, без MOVE — копия и удаление). Правил нет. */
    override suspend fun spam(convs: Collection<Conversation>, scope: String): SpamOutcome {
        val ham = convs.filter { !it.spam }
        val junk = roles()[FolderRole.Junk] ?: throw MailException.Rejected("В ящике нет папки «Спам»")
        dao.deleteConversations(accountKey, ham.map { it.id })
        val groups = byRole(ham.flatMap { it.messages }).filterKeys { it != FolderRole.Junk }
        edit { client, r ->
            for ((role, uids) in groups) {
                val name = r[role] ?: continue
                client.select(name)
                client.moveTo(uids, junk)
            }
        }
        groups.forEach { (role, uids) -> dao.dropImapMessages(accountKey, role.code, uids, role.pack(0)) }
        rebuild()
        this.scope.launch { runCatching { sync() } }
        return SpamOutcome.Moved(groups.values.sumOf { it.size })
    }

    /** «Не спам» — из Junk во «Входящие». */
    override suspend fun restoreSpam(convs: Collection<Conversation>, trust: Boolean): Int {
        val spam = convs.filter { it.spam }
        val r = roles()
        val junk = r[FolderRole.Junk] ?: return 0
        val inbox = r[FolderRole.Inbox] ?: "INBOX"
        dao.deleteConversations(spamKey, spam.map { it.id })
        val uids = byRole(spam.flatMap { it.messages })[FolderRole.Junk].orEmpty()
        edit { client, _ ->
            client.select(junk)
            client.moveTo(uids, inbox)
        }
        dao.dropImapMessages(accountKey, FolderRole.Junk.code, uids, FolderRole.Junk.pack(0))
        rebuild()
        scope.launch { runCatching { sync() } }
        return uids.size
    }

    // ── Отправка ──

    private suspend fun smtp(): SmtpConfig {
        val pw = password() ?: throw MailException.Auth("нет пароля")
        return SmtpConfig(account.smtpHost, account.smtpPort, account.smtpAuth, account.login, pw)
    }

    /**
     * SMTP (контракт §5), затем копия в «Отправленные»: через ~5 с ищем письмо
     * по Message-ID (многие серверы кладут копию сами), не нашлось за ~15 с —
     * `APPEND` с `\Seen`. У Gmail не делаем — он кладёт сам.
     */
    /**
     * IMAP `SEARCH` во «Входящих» и «Отправленных» — как `search_impl`
     * десктопа. Из каждой папки — последние [limit] совпадений.
     */
    override suspend fun searchMessages(query: String, limit: Int): List<MessageHit> {
        val r = roles()
        val wanted = listOf(FolderRole.Inbox, FolderRole.Sent).mapNotNull { role -> r[role]?.let { role to it } }
        if (wanted.isEmpty()) return emptyList()
        return withClient { c ->
            wanted.flatMap { (role, name) ->
                c.select(name)
                val uids = c.searchText(query).sorted().takeLast(limit)
                if (uids.isEmpty()) emptyList()
                else c.fetchHeaders(uids).map { h ->
                    MessageHit(accountKey, name, role.pack(h.uid), h.messageId, h.fromName.ifBlank { h.fromAddr }, h.subject, h.dateTs)
                }
            }
        }
    }

    override suspend fun send(req: SendRequest) {
        val cfg = smtp()
        val out = mime { Smtp.build(req) }
        withContext(Dispatchers.IO) { Smtp.send(cfg, out) }
        if (account.imapHost.endsWith("gmail.com") || account.imapHost.endsWith("googlemail.com")) {
            scope.launch { delay(5_000); runCatching { sync() } }
            return
        }
        scope.launch {
            runCatching { copyToSent(out.messageId, out.bytes()) }
            runCatching { sync() }
        }
    }

    private suspend fun copyToSent(messageId: String, bytes: ByteArray) {
        delay(5_000)
        val deadline = System.currentTimeMillis() + 15_000
        while (true) {
            val sent = roles()[FolderRole.Sent] ?: return
            val found = withClient { c ->
                c.select(sent)
                c.searchMessageId(messageId).isNotEmpty()
            }
            if (found) return
            if (System.currentTimeMillis() >= deadline) {
                withClient { c -> c.append(sent, "\\Seen", bytes) }
                return
            }
            delay(5_000)
        }
    }

    /** Проверка входа: IMAP и SMTP (форма подключения и «Войти»). */
    suspend fun checkLogin() {
        withContext(Dispatchers.IO) {
            val pw = password() ?: throw MailException.Auth("нет пароля")
            openClient(pw).use { it.logout() }
            Smtp.check(SmtpConfig(account.smtpHost, account.smtpPort, account.smtpAuth, account.login, pw))
        }
    }

    // ── Новая почта на экране: IDLE ──

    /**
     * IDLE на INBOX (контракт §6) своим соединением: событие — [onChange];
     * IDLE перевыдаётся каждые 25 мин. Без IDLE у сервера — опрос раз в 2
     * мин. Возвращается при обрыве (ошибкой) — переподключение решает
     * вызывающий, с бэкоффом.
     */
    suspend fun watch(onChange: () -> Unit) {
        if (closed) return
        val pw = password() ?: throw MailException.Auth("нет пароля")
        val inbox = roles()[FolderRole.Inbox] ?: "INBOX"
        val c = withContext(Dispatchers.IO) { openClient(pw) }
        if (!c.idleSupported) {
            // Без IDLE соединение не держим: опрос — обычным синком.
            withContext(Dispatchers.IO) { runCatching { c.logout() }; c.close() }
            while (true) {
                delay(POLL_MS)
                onChange()
            }
        }
        coroutineScope {
            // Отмена (приложение ушло с экрана) закрывает сокет сразу: блокирующее
            // чтение IDLE падает, а не ждёт события до 25 минут.
            val guard = launch {
                try {
                    awaitCancellation()
                } finally {
                    c.conn.stopIdle()
                    c.close()
                }
            }
            try {
                withContext(Dispatchers.IO) {
                    c.select(inbox)
                    while (isActive) {
                        var fired = false
                        c.conn.resetIdleStop()
                        c.idle(IDLE_MS) { d: Resp.Data ->
                            if (d.kind in setOf("EXISTS", "EXPUNGE", "FETCH", "VANISHED")) fired = true
                            fired
                        }
                        if (fired) onChange()
                    }
                }
            } finally {
                guard.cancel()
                withContext(NonCancellable + Dispatchers.IO) {
                    runCatching { c.logout() }
                    c.close()
                }
            }
        }
    }

    /** Учётку удаляют: ни синк, ни соединения больше не пишут в кэш. */
    @Volatile private var closed = false

    override suspend fun forget() {
        closed = true
        close()
        // Под замком синка: идущий проход не допишет удалённое обратно.
        syncLock.withLock {
            dao.clearAccount(accountKey)
            raw.clear()
        }
    }

    companion object {
        const val SPAM_PREFIX = "spam:"
        const val REUSE_MS = 60_000L
        const val IDLE_MS = 25 * 60_000L
        const val POLL_MS = 2 * 60_000L
        const val PREFETCH_BATCH = 10
        const val PREFETCH_MAX_SIZE = 5L shl 20
        const val METADATA_IDENTITIES = "/shared/vendor/ddmail/identities"
        val SYNCED = listOf(FolderRole.Inbox, FolderRole.Sent, FolderRole.Drafts, FolderRole.Junk)
        private val identSer = ListSerializer(Identity.serializer())

        /**
         * Проверка входа перед сохранением (контракт §1): IMAP, затем SMTP,
         * у каждого шага таймаут 25 с. Ошибка — с префиксом шага.
         */
        suspend fun check(account: ImapAccount, password: String) = withContext(Dispatchers.IO) {
            try {
                ImapClient.connect(account.imapHost, account.imapPort).use { c ->
                    c.login(account.login, password)
                    c.logout()
                }
            } catch (e: MailException) {
                throw prefixed("IMAP", e)
            }
            try {
                Smtp.check(SmtpConfig(account.smtpHost, account.smtpPort, account.smtpAuth, account.login, password))
            } catch (e: MailException) {
                throw prefixed("SMTP", e)
            }
        }

        private fun prefixed(step: String, e: MailException): MailException = when (e) {
            is MailException.Auth -> MailException.Auth("$step: неверный логин или пароль")
            is MailException.Network -> MailException.Network("$step: ${e.message}", e)
            is MailException.Rejected -> MailException.Rejected("$step: ${e.message}")
        }

        fun md5(s: String): String = MessageDigest.getInstance("MD5").digest(s.toByteArray()).joinToString("") { "%02x".format(it) }

        /** Первый JSON-массив верхнего уровня (`extract_top_level_array` десктопа). */
        fun extractJsonArray(s: String): String? {
            val start = s.indexOf('[').takeIf { it >= 0 } ?: return null
            var depth = 0
            var inStr = false
            var esc = false
            for (i in start until s.length) {
                val b = s[i]
                if (inStr) {
                    if (esc) esc = false else if (b == '\\') esc = true else if (b == '"') inStr = false
                    continue
                }
                when (b) {
                    '"' -> inStr = true
                    '[' -> depth++
                    ']' -> {
                        depth--
                        if (depth == 0) return s.substring(start, i + 1)
                    }
                }
            }
            return null
        }
    }
}

/** Кэш сырых писем на диске (в cacheDir — ОС может его чистить), не больше 64 МБ. */
internal class RawCache(private val dir: File) {
    fun get(packed: Long): ByteArray? = File(dir, "$packed.eml").takeIf { it.exists() }?.let { f ->
        runCatching { f.setLastModified(System.currentTimeMillis()); f.readBytes() }.getOrNull()
    }

    fun put(packed: Long, bytes: ByteArray) {
        runCatching {
            dir.mkdirs()
            File(dir, "$packed.eml").writeBytes(bytes)
            val files = dir.listFiles().orEmpty().sortedByDescending { it.lastModified() }
            var total = 0L
            for (f in files) {
                total += f.length()
                if (total > LIMIT) f.delete()
            }
        }
    }

    fun clear() {
        dir.deleteRecursively()
    }

    private companion object {
        const val LIMIT = 64L shl 20
    }
}
