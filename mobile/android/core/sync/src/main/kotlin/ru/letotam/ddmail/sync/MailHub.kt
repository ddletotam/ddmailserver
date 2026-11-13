package ru.letotam.ddmail.sync

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.flatMapLatest
import kotlinx.coroutines.flow.flowOf
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.update
import kotlinx.serialization.json.Json
import okhttp3.OkHttpClient
import okhttp3.Request
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.api.Binary
import ru.letotam.ddmail.api.Identity
import ru.letotam.ddmail.api.MessageBody
import ru.letotam.ddmail.api.MessageRef
import ru.letotam.ddmail.api.SendRequest
import ru.letotam.ddmail.db.CacheDao
import ru.letotam.ddmail.imap.MailException

/** Состояние учётки для интерфейса (контракт §7): «нужен вход» — у каждой своё. */
sealed interface AccountStatus {
    data object Ok : AccountStatus
    data class Offline(val reason: String) : AccountStatus

    /** Ретраи не лечат: пароль отвергнут или сессия ddmail мертва. */
    data object NeedsLogin : AccountStatus
}

/**
 * Все учётки разом (mobile-external-accounts §1): одна сводная лента бесед,
 * правки — источнику беседы по `accountKey`. Ошибка одной учётки другим не
 * мешает: у каждой свой [AccountStatus].
 *
 * Папки диалогов — документ сервера ddmail, если такая учётка есть; иначе —
 * локальный документ в том же формате (контракт §4). Цвета строк — группы
 * айдентик ddmail.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class MailHub(
    private val dao: CacheDao,
    private val http: OkHttpClient,
) {
    private val _sources = MutableStateFlow<List<MailSource>>(emptyList())
    val sources: StateFlow<List<MailSource>> = _sources

    private val _statuses = MutableStateFlow<Map<String, AccountStatus>>(emptyMap())
    val statuses: StateFlow<Map<String, AccountStatus>> = _statuses

    /** Учётка ddmail (не больше одной). */
    val ddmail: SyncEngine? get() = _sources.value.filterIsInstance<SyncEngine>().firstOrNull()

    fun source(accountKey: String): MailSource? = _sources.value.firstOrNull { it.accountKey == accountKey }

    /** Набор учёток сменился: новые подключаются, ушедшие закрывают соединения. */
    fun setSources(list: List<MailSource>) {
        val keep = list.map { it.accountKey }.toSet()
        // Закрываются и подменённые экземпляры той же учётки (правка настроек).
        _sources.value.filter { old -> list.none { it === old } }.forEach { it.close() }
        _sources.value = list
        _statuses.update { m -> m.filterKeys { it in keep } }
    }

    val conversations: Flow<List<Conversation>> = _sources.flatMapLatest { list ->
        if (list.isEmpty()) flowOf(emptyList())
        else combine(list.map { it.conversations }) { arr -> arr.flatMap { it }.sortedByDescending { it.lastDateTs } }
    }

    /** Все свои адреса: учётка — владелец адреса (`owner_for` десктопа). */
    val identities: Flow<List<Pair<String, Identity>>> = _sources.flatMapLatest { list ->
        if (list.isEmpty()) flowOf(emptyList())
        else combine(list.map { s -> s.identities.map { ids -> ids.map { s.accountKey to it } } }) { arr -> arr.flatMap { it } }
    }

    val folders: Flow<FoldersDoc> = _sources.flatMapLatest { list ->
        list.filterIsInstance<SyncEngine>().firstOrNull()?.folders
            ?: dao.observeMeta(K_LOCAL_FOLDERS).map { s -> parseDoc(s).withSpam() }
    }

    val identityColors: Flow<IdentityColors> = _sources.flatMapLatest { list ->
        list.filterIsInstance<SyncEngine>().firstOrNull()?.identityColors ?: flowOf(IdentityColors())
    }

    fun setStatus(accountKey: String, s: AccountStatus) = _statuses.update { it + (accountKey to s) }

    /**
     * Итог действия для статуса учётки: отказ входа — «нужен вход»; удача
     * синка снимает и его.
     */
    fun noteError(accountKey: String, e: Throwable) {
        if (e.isAuthError()) setStatus(accountKey, AccountStatus.NeedsLogin)
    }

    /** Синк всех учёток разом; ошибки — в статусы, наружу не летят. */
    suspend fun syncAll(): Map<String, AccountStatus> = coroutineScope {
        _sources.value.map { s ->
            async {
                val st = try {
                    s.sync()
                    AccountStatus.Ok
                } catch (e: Exception) {
                    if (e is kotlinx.coroutines.CancellationException) throw e
                    when {
                        e.isAuthError() -> AccountStatus.NeedsLogin
                        e.isMailError() -> AccountStatus.Offline(e.errorText())
                        else -> AccountStatus.Offline(e.message ?: e.javaClass.simpleName)
                    }
                }
                setStatus(s.accountKey, st)
                s.accountKey to st
            }
        }.awaitAll().toMap()
    }

    private fun of(c: Conversation): MailSource = source(c.accountKey) ?: throw MailException.Rejected("Учётка беседы удалена")

    private suspend fun <T> tracked(key: String, block: suspend () -> T): T = try {
        block()
    } catch (e: Exception) {
        noteError(key, e)
        throw e
    }

    suspend fun bodies(accountKey: String, refs: List<MessageRef>): List<MessageBody> =
        source(accountKey)?.let { s -> tracked(accountKey) { s.bodies(refs) } }.orEmpty()

    suspend fun part(accountKey: String, uid: Long, cid: String): Binary =
        (source(accountKey) ?: throw MailException.Rejected("нет учётки")).part(uid, cid)

    suspend fun attachment(accountKey: String, uid: Long, index: Int): Binary =
        tracked(accountKey) { (source(accountKey) ?: throw MailException.Rejected("нет учётки")).attachment(uid, index) }

    suspend fun markRead(c: Conversation) = tracked(c.accountKey) { of(c).markRead(c) }
    suspend fun markUnread(c: Conversation) = tracked(c.accountKey) { of(c).markUnread(c) }

    suspend fun deleteConversations(convs: Collection<Conversation>) =
        convs.groupBy { it.accountKey }.forEach { (k, list) -> tracked(k) { of(list[0]).deleteConversations(list) } }

    suspend fun restoreSpam(convs: Collection<Conversation>, trust: Boolean): Int =
        convs.groupBy { it.accountKey }.entries.sumOf { (k, list) -> tracked(k) { of(list[0]).restoreSpam(list, trust) } }

    suspend fun spam(convs: Collection<Conversation>, scope: String): List<SpamOutcome> =
        convs.groupBy { it.accountKey }.map { (k, list) -> tracked(k) { of(list[0]).spam(list, scope) } }

    /** Можно ли «В спам» для этих бесед; с правилами «адрес/домен» — только ddmail. */
    suspend fun spamAbility(convs: Collection<Conversation>): Pair<Boolean, Boolean> {
        val srcs = convs.map { it.accountKey }.distinct().mapNotNull(::source)
        if (srcs.isEmpty()) return false to false
        return srcs.all { it.canSpam() } to srcs.all { it.spamRules }
    }

    /**
     * «Сообщения» поиска — со всех учёток разом, новые сверху. Учётка, у
     * которой поиск не удался (нет сети, нет поиска), просто ничего не даёт:
     * выпадашка не должна ломаться из-за одной учётки.
     */
    suspend fun searchMessages(query: String, limit: Int): List<MessageHit> = coroutineScope {
        _sources.value.map { s ->
            async {
                try {
                    s.searchMessages(query, limit)
                } catch (e: Exception) {
                    if (e is kotlinx.coroutines.CancellationException || !e.isMailError()) throw e
                    emptyList()
                }
            }
        }.awaitAll().flatten().sortedByDescending { it.dateTs }
    }

    /**
     * Отправка — учёткой-владельцем адреса From (`owner_for` десктопа); нет
     * владельца — «Не через что отправить» (контракт §5).
     *
     * `true` — письмо принял сервер ddmail: копия в «Отправленных» уже есть,
     * доставку он сообщит сам (`delivery_status`). `false` — ушло через SMTP
     * IMAP-учётки: доставлено. Отказ SMTP — [SmtpFailed].
     */
    suspend fun send(req: SendRequest, owners: List<Pair<String, Identity>>, prefer: String? = null): Boolean {
        val from = bareAddr(req.from)
        val candidates = owners.filter { it.second.email.equals(from, ignoreCase = true) }.map { it.first }
        // Адрес есть у нескольких учёток (ящик подключён и через ddmail, и по
        // IMAP) — отвечаем той, где беседа.
        val key = (candidates.firstOrNull { it == prefer } ?: candidates.firstOrNull())
            ?: throw MailException.Rejected("Не через что отправить: адрес $from не принадлежит ни одной учётке")
        val src = source(key) ?: throw MailException.Rejected("Не через что отправить")
        try {
            tracked(key) { src.send(req) }
        } catch (e: Exception) {
            if (src !is SyncEngine && e.isMailError()) throw SmtpFailed(e)
            throw e
        }
        return src is SyncEngine
    }

    /** Документ папок в кэше (до ответа сервера) — для правок. */
    suspend fun assignFolder(convs: Collection<Conversation>, folderId: String?) {
        ddmail?.let { return it.assignFolder(convs, folderId) }
        val doc = parseDoc(dao.meta(K_LOCAL_FOLDERS)).withSpam()
        val target = folderId?.let { id -> doc.folders.indexOfFirst { it.id == id }.takeIf { it >= 0 } }
        if (folderId != null && target == null) return
        dao.setMeta(K_LOCAL_FOLDERS, doc.assign(convs, target).toJson().toString())
    }

    /**
     * Аватар: через сервер ddmail (его цепочка: контакты, Gravatar, BIMI,
     * favicon), если такая учётка есть; иначе Gravatar напрямую, как IMAP-режим
     * десктопа. `null` — нет.
     */
    suspend fun avatar(email: String): Binary? {
        ddmail?.let { return it.avatar(email) }
        val hash = ru.letotam.ddmail.conv.Conversations.md5(email)
        val req = Request.Builder().url("https://www.gravatar.com/avatar/$hash?d=404&s=96").build()
        return try {
            val r = ru.letotam.ddmail.api.fetchBinary(http, req)
            r?.takeIf { it.bytes.isNotEmpty() }
        } catch (e: ApiException) {
            null
        }
    }

    /**
     * Правка документа папок (контракт §5, десктоп §4): на сервере ddmail, если
     * такая учётка есть, иначе — локальный документ телефона.
     */
    suspend fun editFolders(transform: (FoldersDoc) -> FoldersDoc) {
        ddmail?.let { return it.editFolders(transform) }
        val doc = parseDoc(dao.meta(K_LOCAL_FOLDERS)).withSpam()
        dao.setMeta(K_LOCAL_FOLDERS, transform(doc).withSpam().toJson().toString())
    }

    private fun parseDoc(s: String?): FoldersDoc =
        s?.let { runCatching { FoldersDoc.parse(Json.parseToJsonElement(it)) }.getOrNull() } ?: FoldersDoc()

    companion object {
        const val K_LOCAL_FOLDERS = "local:dialog_folders"

        fun bareAddr(raw: String): String {
            val lt = raw.lastIndexOf('<')
            val gt = raw.lastIndexOf('>')
            return (if (lt >= 0 && gt > lt) raw.substring(lt + 1, gt) else raw).trim().lowercase()
        }
    }
}
