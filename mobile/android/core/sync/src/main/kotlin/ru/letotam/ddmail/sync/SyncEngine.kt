package ru.letotam.ddmail.sync

import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.json.Json
import ru.letotam.ddmail.api.ChangeEntry
import ru.letotam.ddmail.api.ContactInfo
import ru.letotam.ddmail.api.ConversationDto
import ru.letotam.ddmail.api.MessageRef
import ru.letotam.ddmail.api.SessionApi
import ru.letotam.ddmail.db.CacheDao
import ru.letotam.ddmail.db.ConversationEntity
import ru.letotam.ddmail.db.MessageRefEntity

/**
 * Синхронизация учётки ddmail (контракт §4) — модель десктопного
 * native-провайдера (`engine.rs`, `FetchConversations`):
 *
 * 1. журнал `/changes?since=<seq>` — надгробия удалений применяются к кэшу
 *    ДО дельты: дельта бесед сообщает только изменившиеся, удалённые — никогда;
 *    `reset` (курсор негоден) — следующий шаг полный;
 * 2. `/conversations?since=<watermark>` — дельта; полная выдача (`since=0`)
 *    заменяет набор целиком: на первом запуске, по `reset`, раз в сутки и при
 *    смене [CONV_ID_EPOCH];
 * 3. документ папок диалогов `/dialog-folders` — в кэш (табы видны на
 *    старте, до ответа сервера).
 *
 * Синхронизации одной учётки не идут параллельно ([lock]).
 */
class SyncEngine(
    val accountKey: String,
    private val api: SessionApi,
    private val dao: CacheDao,
    private val now: () -> Long = { System.currentTimeMillis() / 1000 },
) {
    private val lock = Mutex()

    val conversations: Flow<List<Conversation>> =
        combine(dao.observeConversations(accountKey), dao.observeRefs(accountKey)) { convs, refs ->
            val byConv = refs.groupBy { it.conversationId }
            convs.map { it.toDomain(byConv[it.id].orEmpty()) }
        }

    val folders: Flow<FoldersDoc> = dao.observeMeta(K_FOLDERS).map { s ->
        s?.let { runCatching { FoldersDoc.parse(json.parseToJsonElement(it)) }.getOrNull() } ?: FoldersDoc()
    }

    /** Один проход. Ошибки API уходят наверх ([ru.letotam.ddmail.api.ApiException]). */
    suspend fun sync() = lock.withLock {
        var forceFull = false

        val seq = dao.meta(K_SEQ)?.toLongOrNull() ?: 0
        val ch = api.changes(seq)
        if (ch.reset && seq > 0) forceFull = true
        val deletes = ch.entries.filter { it.kind == ChangeEntry.DELETE }.map { it.messageId }
        if (deletes.isNotEmpty()) dao.applyDeletions(accountKey, deletes)

        val lastFull = dao.meta(K_FULL_TS)?.toLongOrNull() ?: 0
        val epochStale = dao.meta(K_EPOCH) != CONV_ID_EPOCH
        val since = if (forceFull || epochStale || now() - lastFull > FULL_EVERY_S) {
            0
        } else {
            dao.meta(K_SINCE)?.toLongOrNull() ?: 0
        }
        val delta = api.conversationsDelta(since)
        val convs = delta.conversations.map { it.toEntity() }
        val refs = delta.conversations.flatMap { c -> c.messages.map { it.toEntity(c.id) } }
        if (since > 0) {
            dao.upsert(accountKey, convs, refs)
        } else {
            dao.replaceAll(accountKey, convs, refs)
            dao.setMeta(K_FULL_TS, now().toString())
            dao.setMeta(K_EPOCH, CONV_ID_EPOCH)
        }
        if (delta.serverNowMs > 0) dao.setMeta(K_SINCE, delta.serverNowMs.toString())
        // Курсор журнала — только после того, как дельта легла: упади она,
        // надгробия этого прохода не потеряются, их отдадут ещё раз.
        if (ch.latestSeq > 0) dao.setMeta(K_SEQ, ch.latestSeq.toString())

        val f = api.dialogFolders()
        f.doc?.let {
            dao.setMeta(K_FOLDERS, it.toString())
            dao.setMeta(K_FOLDERS_VERSION, f.version.toString())
        }
    }

    private fun ConversationDto.toEntity() = ConversationEntity(
        accountKey = accountKey,
        id = id,
        label = label,
        avatarHash = avatarHash,
        receivedBy = receivedBy,
        counterpartsJson = json.encodeToString(contacts, counterparts),
        isGroup = isGroup,
        lastDateTs = lastDateTs,
        lastSubject = lastSubject,
        unreadCount = unreadCount,
        totalCount = totalCount,
        updatedAt = updatedAt,
    )

    private fun MessageRef.toEntity(conversationId: String) =
        MessageRefEntity(accountKey, conversationId, folder, uid, messageId, seen)

    private fun ConversationEntity.toDomain(refs: List<MessageRefEntity>) = Conversation(
        accountKey = accountKey,
        id = id,
        label = label,
        receivedBy = receivedBy,
        counterparts = runCatching { json.decodeFromString(contacts, counterpartsJson) }.getOrDefault(emptyList()),
        isGroup = isGroup,
        lastDateTs = lastDateTs,
        lastSubject = lastSubject,
        unreadCount = unreadCount,
        totalCount = totalCount,
        messages = refs.map { MessageRef(it.folder, it.uid, it.messageId, it.seen) },
    )

    companion object {
        /**
         * Версия формата id бесед (сервер). Сменилась — следующий синк полный,
         * иначе старые ключи висели бы рядом с новыми (§4 десктопа).
         */
        const val CONV_ID_EPOCH = "2"
        const val FULL_EVERY_S = 24 * 3600L

        private const val K_SEQ = "journal_seq"
        private const val K_SINCE = "conv_since"
        private const val K_FULL_TS = "conv_full_ts"
        private const val K_EPOCH = "conv_id_epoch"
        private const val K_FOLDERS = "dialog_folders"
        private const val K_FOLDERS_VERSION = "dialog_folders_version"

        private val json = Json { ignoreUnknownKeys = true }
        private val contacts = ListSerializer(ContactInfo.serializer())
    }
}
