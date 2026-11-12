package ru.letotam.ddmail.db

import android.content.Context
import androidx.room.AutoMigration
import androidx.room.Dao
import androidx.room.Database
import androidx.room.Entity
import androidx.room.Insert
import androidx.room.OnConflictStrategy
import androidx.room.Query
import androidx.room.Room
import androidx.room.RoomDatabase
import androidx.room.Transaction
import kotlinx.coroutines.flow.Flow

/**
 * Беседа как её отдал сервер. Ключ — (учётка, id): id задаёт сервер (набор
 * адресов), учётка — `логин@хост`, как `account_key` десктопа, чтобы ключи
 * папок диалогов совпадали.
 */
@Entity(tableName = "conversations", primaryKeys = ["accountKey", "id"])
data class ConversationEntity(
    val accountKey: String,
    val id: String,
    val label: String,
    val avatarHash: String,
    val receivedBy: String,
    /** JSON-массив `{name, addr}` — разбирает слой синхронизации. */
    val counterpartsJson: String,
    val isGroup: Boolean,
    /** Секунды. */
    val lastDateTs: Long,
    val lastSubject: String,
    val unreadCount: Int,
    val totalCount: Int,
    /** Миллисекунды, часы сервера. */
    val updatedAt: Long,
)

/** Ссылка на письмо беседы. Тела — отдельно (следующий шаг). */
@Entity(tableName = "conversation_messages", primaryKeys = ["accountKey", "conversationId", "folder", "uid"])
data class MessageRefEntity(
    val accountKey: String,
    val conversationId: String,
    val folder: String,
    val uid: Long,
    val messageId: String,
    val seen: Boolean,
)

/**
 * Тело письма. Тела в кэше неизменны (по письму не перезапрашиваются) — кроме
 * пустых: ни HTML, ни текста, ни вложений значит сбойный фетч (§4а десктопа).
 */
@Entity(tableName = "message_bodies", primaryKeys = ["accountKey", "uid"])
data class MessageBodyEntity(
    val accountKey: String,
    val uid: Long,
    val messageId: String,
    val folder: String,
    val subject: String,
    val fromName: String,
    val fromAddr: String,
    val toJson: String,
    val ccJson: String,
    /** Секунды. */
    val dateTs: Long,
    val html: String?,
    val text: String?,
    val attachmentsJson: String,
    val isOutgoing: Boolean,
    val inReplyTo: String,
    val referencesJson: String,
)

/**
 * Напоминание об одном вхождении события (контракт §9, как `reminders2`
 * десктопа). Каскад [leads] — минуты до начала по убыванию; [next] — индекс
 * следующего звена. [snoozeAt] — «напомнить позже» заменяет каскад одним
 * сроком. [state]: armed | done | cancelled (✕ — тишина навсегда).
 */
@Entity(tableName = "reminders", primaryKeys = ["eventId", "occStart"])
data class ReminderEntity(
    val eventId: Long,
    val occStart: Long,
    /** 0 — у события нет конца. */
    val occEnd: Long,
    val calendarId: Long,
    val summary: String,
    /** Минуты через запятую, по убыванию: «15,5,0». */
    val leads: String,
    /** Всё, от чего зависит напоминание: сменилось — каскад заново. */
    val signature: String,
    val next: Int,
    val snoozeAt: Long?,
    val state: String,
)

/**
 * Состояние папки внешней IMAP-учётки (mobile-external-accounts §3): по нему
 * синк инкрементальный. [role] — код роли (`FolderRole.code`), [name] — имя
 * на сервере (modified UTF-7).
 */
@Entity(tableName = "imap_folders", primaryKeys = ["accountKey", "role"])
data class ImapFolderEntity(
    val accountKey: String,
    val role: Int,
    val name: String,
    val uidValidity: Long,
    val uidNext: Long,
    val highestModSeq: Long,
    val lastUid: Long,
)

/**
 * Заголовки письма IMAP-учётки — из них на устройстве строятся беседы.
 * [uid] — UID в папке (не упакованный).
 */
@Entity(tableName = "imap_headers", primaryKeys = ["accountKey", "role", "uid"])
data class ImapHeaderEntity(
    val accountKey: String,
    val role: Int,
    val uid: Long,
    val seen: Boolean,
    val flagged: Boolean,
    val draft: Boolean,
    val subject: String,
    val fromName: String,
    val fromAddr: String,
    val toAddrsJson: String,
    val toNamesJson: String,
    val ccAddrsJson: String,
    val ccNamesJson: String,
    /** Секунды. */
    val dateTs: Long,
    val messageId: String,
    val inReplyTo: String,
    val referencesJson: String,
    val hasAttachments: Boolean,
    /** Миллисекунды: когда строка последний раз менялась. */
    val updatedAt: Long,
)

/** Служебные значения синхронизации: курсоры, водяные знаки, кэш документов. */
@Entity(tableName = "meta")
data class MetaEntity(@androidx.room.PrimaryKey val key: String, val value: String)

@Dao
abstract class CacheDao {
    @Query("SELECT * FROM conversations WHERE accountKey = :account ORDER BY lastDateTs DESC")
    abstract fun observeConversations(account: String): Flow<List<ConversationEntity>>

    @Query("SELECT * FROM conversation_messages WHERE accountKey = :account")
    abstract fun observeRefs(account: String): Flow<List<MessageRefEntity>>

    @Insert(onConflict = OnConflictStrategy.REPLACE)
    protected abstract suspend fun insertConversations(c: List<ConversationEntity>)

    /** OR IGNORE: дубль ссылки в выдаче не должен ронять транзакцию (§4 десктопа). */
    @Insert(onConflict = OnConflictStrategy.IGNORE)
    protected abstract suspend fun insertRefs(r: List<MessageRefEntity>)

    @Query("DELETE FROM conversations WHERE accountKey = :account")
    protected abstract suspend fun deleteAllConversations(account: String)

    @Query("DELETE FROM conversation_messages WHERE accountKey = :account")
    protected abstract suspend fun deleteAllRefs(account: String)

    @Query("DELETE FROM conversation_messages WHERE accountKey = :account AND conversationId IN (:ids)")
    protected abstract suspend fun deleteRefsOf(account: String, ids: List<String>)

    @Query("DELETE FROM conversation_messages WHERE accountKey = :account AND messageId IN (:messageIds)")
    protected abstract suspend fun deleteRefsByMessageId(account: String, messageIds: List<String>): Int

    @Query("DELETE FROM message_bodies WHERE accountKey = :account AND messageId IN (:messageIds)")
    protected abstract suspend fun deleteBodiesByMessageId(account: String, messageIds: List<String>)

    @Query(
        "DELETE FROM conversations WHERE accountKey = :account AND id NOT IN " +
            "(SELECT DISTINCT conversationId FROM conversation_messages WHERE accountKey = :account)",
    )
    protected abstract suspend fun deleteEmptyConversations(account: String)

    /** Полная выдача заменяет набор целиком. */
    @Transaction
    open suspend fun replaceAll(account: String, convs: List<ConversationEntity>, refs: List<MessageRefEntity>) {
        deleteAllRefs(account)
        deleteAllConversations(account)
        insertConversations(convs)
        insertRefs(refs)
    }

    /** Дельта: изменившиеся беседы целиком заменяют свои прежние версии. */
    @Transaction
    open suspend fun upsert(account: String, convs: List<ConversationEntity>, refs: List<MessageRefEntity>) {
        convs.map { it.id }.chunked(500).forEach { deleteRefsOf(account, it) }
        insertConversations(convs)
        insertRefs(refs)
    }

    /**
     * Надгробия журнала: письма уходят из бесед, беседы без писем — целиком
     * (как `apply_deletions` десктопа).
     */
    @Transaction
    open suspend fun applyDeletions(account: String, messageIds: List<String>): Int {
        val ids = messageIds.filter { it.isNotEmpty() }
        if (ids.isEmpty()) return 0
        var n = 0
        ids.chunked(500).forEach {
            n += deleteRefsByMessageId(account, it)
            deleteBodiesByMessageId(account, it)
        }
        deleteEmptyConversations(account)
        return n
    }

    @Query("SELECT * FROM message_bodies WHERE accountKey = :account AND uid IN (:uids)")
    abstract suspend fun bodies(account: String, uids: List<Long>): List<MessageBodyEntity>

    @Insert(onConflict = OnConflictStrategy.REPLACE)
    abstract suspend fun putBodies(b: List<MessageBodyEntity>)

    /** Какие тела уже в кэше (непустые) — для докачки; сами тела не читаем. */
    @Query("SELECT uid FROM message_bodies WHERE accountKey = :account AND (html IS NOT NULL OR text IS NOT NULL OR attachmentsJson != '[]')")
    abstract suspend fun cachedBodyUids(account: String): List<Long>

    @Query("UPDATE conversation_messages SET seen = 1 WHERE accountKey = :account AND conversationId = :conv")
    protected abstract suspend fun seeRefs(account: String, conv: String)

    @Query("UPDATE conversations SET unreadCount = 0 WHERE accountKey = :account AND id = :conv")
    protected abstract suspend fun zeroUnread(account: String, conv: String)

    @Query("DELETE FROM conversation_messages WHERE accountKey = :account AND conversationId IN (:ids)")
    protected abstract suspend fun dropRefs(account: String, ids: List<String>)

    @Query("DELETE FROM conversations WHERE accountKey = :account AND id IN (:ids)")
    protected abstract suspend fun dropConversations(account: String, ids: List<String>)

    /** Беседы удалены целиком (оптимистично, до ответа журнала). */
    @Transaction
    open suspend fun deleteConversations(account: String, ids: List<String>) {
        ids.chunked(500).forEach {
            dropRefs(account, it)
            dropConversations(account, it)
        }
    }

    @Query("UPDATE conversation_messages SET seen = 0 WHERE accountKey = :account AND conversationId = :conv AND uid = :uid")
    protected abstract suspend fun unseeRef(account: String, conv: String, uid: Long)

    @Query("UPDATE conversations SET unreadCount = MAX(unreadCount, 1) WHERE accountKey = :account AND id = :conv")
    protected abstract suspend fun bumpUnread(account: String, conv: String)

    /** «Непрочитано»: последнее письмо снова непрочитано. */
    @Transaction
    open suspend fun markUnread(account: String, conv: String, uid: Long) {
        unseeRef(account, conv, uid)
        bumpUnread(account, conv)
    }

    /** Беседу открыли — прочитана локально сразу, сервер — следом. */
    @Transaction
    open suspend fun markRead(account: String, conv: String) {
        seeRefs(account, conv)
        zeroUnread(account, conv)
    }

    @Query("SELECT value FROM meta WHERE `key` = :key")
    abstract suspend fun meta(key: String): String?

    @Query("SELECT value FROM meta WHERE `key` = :key")
    abstract fun observeMeta(key: String): Flow<String?>

    @Insert(onConflict = OnConflictStrategy.REPLACE)
    protected abstract suspend fun putMeta(m: MetaEntity)

    suspend fun setMeta(key: String, value: String) = putMeta(MetaEntity(key, value))

    @Query("DELETE FROM meta WHERE `key` = :key")
    abstract suspend fun deleteMeta(key: String)

    @Query("SELECT * FROM reminders")
    abstract suspend fun reminders(): List<ReminderEntity>

    @Query("SELECT * FROM reminders WHERE eventId = :eventId AND occStart = :occStart")
    abstract suspend fun reminder(eventId: Long, occStart: Long): ReminderEntity?

    @Insert(onConflict = OnConflictStrategy.REPLACE)
    abstract suspend fun putReminders(r: List<ReminderEntity>)

    @Query("DELETE FROM reminders WHERE eventId = :eventId AND occStart = :occStart")
    protected abstract suspend fun deleteReminder(eventId: Long, occStart: Long)

    @Query("DELETE FROM reminders WHERE calendarId = :calendarId")
    abstract suspend fun deleteCalendarReminders(calendarId: Long)

    /** Посев: удалить и записать одной транзакцией. */
    @Transaction
    open suspend fun applyReminders(delete: List<Pair<Long, Long>>, put: List<ReminderEntity>) {
        delete.forEach { (e, s) -> deleteReminder(e, s) }
        putReminders(put)
    }

    @Query("DELETE FROM reminders")
    protected abstract suspend fun clearReminders()

    @Query("DELETE FROM meta")
    protected abstract suspend fun clearMeta()

    @Query("DELETE FROM conversations")
    protected abstract suspend fun clearConversations()

    @Query("DELETE FROM conversation_messages")
    protected abstract suspend fun clearRefs()

    @Query("DELETE FROM message_bodies")
    protected abstract suspend fun clearBodies()

    // ── Внешние IMAP-учётки ──

    @Query("SELECT * FROM imap_folders WHERE accountKey = :account")
    abstract suspend fun imapFolders(account: String): List<ImapFolderEntity>

    @Query("SELECT * FROM imap_headers WHERE accountKey = :account")
    abstract suspend fun imapHeaders(account: String): List<ImapHeaderEntity>

    @Query("SELECT uid FROM imap_headers WHERE accountKey = :account AND role = :role")
    abstract suspend fun imapUids(account: String, role: Int): List<Long>

    @Insert(onConflict = OnConflictStrategy.REPLACE)
    protected abstract suspend fun putImapHeaders(h: List<ImapHeaderEntity>)

    @Insert(onConflict = OnConflictStrategy.REPLACE)
    protected abstract suspend fun putImapFolder(f: ImapFolderEntity)

    @Query("DELETE FROM imap_headers WHERE accountKey = :account AND role = :role")
    protected abstract suspend fun dropImapRole(account: String, role: Int)

    @Query("DELETE FROM imap_headers WHERE accountKey = :account AND role = :role AND uid IN (:uids)")
    protected abstract suspend fun dropImapHeaders(account: String, role: Int, uids: List<Long>)

    @Query("DELETE FROM message_bodies WHERE accountKey = :account AND uid IN (:uids)")
    protected abstract suspend fun dropBodies(account: String, uids: List<Long>)

    @Query("DELETE FROM message_bodies WHERE accountKey = :account AND uid >= :from AND uid < :to")
    protected abstract suspend fun dropBodiesRange(account: String, from: Long, to: Long)

    @Query("UPDATE imap_headers SET seen = :seen, flagged = :flagged, draft = :draft, updatedAt = :at WHERE accountKey = :account AND role = :role AND uid = :uid")
    protected abstract suspend fun setImapFlags(account: String, role: Int, uid: Long, seen: Boolean, flagged: Boolean, draft: Boolean, at: Long)

    @Query("UPDATE imap_headers SET seen = :seen, updatedAt = :at WHERE accountKey = :account AND role = :role AND uid IN (:uids)")
    protected abstract suspend fun setImapSeen(account: String, role: Int, uids: List<Long>, seen: Boolean, at: Long)

    @Query("DELETE FROM imap_folders WHERE accountKey = :account AND role = :role")
    protected abstract suspend fun dropImapFolder(account: String, role: Int)

    /** Флаги и строки, изменённые одним проходом по папке, — одной транзакцией. */
    data class ImapFlags(val uid: Long, val seen: Boolean, val flagged: Boolean, val draft: Boolean)

    /**
     * Итог прохода по папке. [reset] — сменилась UIDVALIDITY: заголовки и тела
     * папки уходят целиком. [bodyBase] — упакованный UID ноль этой роли
     * (`role shl 32`): тела в кэше лежат под упакованными UID.
     */
    @Transaction
    open suspend fun applyImapFolder(
        account: String,
        role: Int,
        reset: Boolean,
        added: List<ImapHeaderEntity>,
        flags: List<ImapFlags>,
        removed: Collection<Long>,
        state: ImapFolderEntity,
        bodyBase: Long,
    ) {
        if (reset) {
            dropImapRole(account, role)
            dropBodiesRange(account, bodyBase, bodyBase + (1L shl 32))
        }
        removed.chunked(500).forEach { chunk ->
            dropImapHeaders(account, role, chunk)
            dropBodies(account, chunk.map { bodyBase or it })
        }
        val now = System.currentTimeMillis()
        flags.forEach { setImapFlags(account, role, it.uid, it.seen, it.flagged, it.draft, now) }
        putImapHeaders(added)
        putImapFolder(state)
    }

    /** Папка роли пропала на сервере — её кэш уходит. */
    @Transaction
    open suspend fun dropImapFolderRole(account: String, role: Int, bodyBase: Long) {
        dropImapRole(account, role)
        dropBodiesRange(account, bodyBase, bodyBase + (1L shl 32))
        dropImapFolder(account, role)
    }

    /** Флаг `\Seen` после ответа сервера. */
    suspend fun markImapSeen(account: String, role: Int, uids: List<Long>, seen: Boolean) =
        uids.chunked(500).forEach { setImapSeen(account, role, it, seen, System.currentTimeMillis()) }

    /** Письма ушли с сервера по нашей правке (удаление, перенос). */
    @Transaction
    open suspend fun dropImapMessages(account: String, role: Int, uids: List<Long>, bodyBase: Long) {
        uids.chunked(500).forEach { chunk ->
            dropImapHeaders(account, role, chunk)
            dropBodies(account, chunk.map { bodyBase or it })
        }
    }

    @Query("DELETE FROM imap_headers WHERE accountKey = :account")
    protected abstract suspend fun clearImapHeaders(account: String)

    @Query("DELETE FROM imap_folders WHERE accountKey = :account")
    protected abstract suspend fun clearImapFolders(account: String)

    @Query("DELETE FROM message_bodies WHERE accountKey = :account")
    protected abstract suspend fun clearAccountBodies(account: String)

    @Query("DELETE FROM meta WHERE `key` LIKE :prefix || '%'")
    protected abstract suspend fun clearMetaPrefix(prefix: String)

    /**
     * Учётку удалили: её беседы, заголовки, тела и служебные значения
     * (ключи meta с префиксом `<учётка>:`) не остаются на телефоне.
     */
    @Transaction
    open suspend fun clearAccount(account: String) {
        for (k in listOf(account, "$account#spam")) {
            deleteAllRefs(k)
            deleteAllConversations(k)
            clearAccountBodies(k)
        }
        clearImapHeaders(account)
        clearImapFolders(account)
        clearMetaPrefix("$account:")
    }

    @Query("DELETE FROM imap_headers")
    protected abstract suspend fun clearAllImapHeaders()

    @Query("DELETE FROM imap_folders")
    protected abstract suspend fun clearAllImapFolders()

    /** Удалена последняя учётка: на телефоне не остаётся ничего из кэша. */
    @Transaction
    open suspend fun clearAll() {
        clearRefs()
        clearConversations()
        clearBodies()
        clearMeta()
        clearReminders()
        clearAllImapHeaders()
        clearAllImapFolders()
    }
}

@Database(
    entities = [
        ConversationEntity::class, MessageRefEntity::class, MetaEntity::class, MessageBodyEntity::class, ReminderEntity::class,
        ImapFolderEntity::class, ImapHeaderEntity::class,
    ],
    version = 4,
    exportSchema = true,
    autoMigrations = [AutoMigration(from = 1, to = 2), AutoMigration(from = 2, to = 3), AutoMigration(from = 3, to = 4)],
)
abstract class CacheDb : RoomDatabase() {
    abstract fun dao(): CacheDao

    companion object {
        fun open(context: Context): CacheDb =
            Room.databaseBuilder(context, CacheDb::class.java, "cache.db").build()
    }
}
