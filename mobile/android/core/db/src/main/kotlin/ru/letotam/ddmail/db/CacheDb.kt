package ru.letotam.ddmail.db

import android.content.Context
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
        ids.chunked(500).forEach { n += deleteRefsByMessageId(account, it) }
        deleteEmptyConversations(account)
        return n
    }

    @Query("SELECT value FROM meta WHERE `key` = :key")
    abstract suspend fun meta(key: String): String?

    @Query("SELECT value FROM meta WHERE `key` = :key")
    abstract fun observeMeta(key: String): Flow<String?>

    @Insert(onConflict = OnConflictStrategy.REPLACE)
    protected abstract suspend fun putMeta(m: MetaEntity)

    suspend fun setMeta(key: String, value: String) = putMeta(MetaEntity(key, value))

    @Query("DELETE FROM meta")
    protected abstract suspend fun clearMeta()

    @Query("DELETE FROM conversations")
    protected abstract suspend fun clearConversations()

    @Query("DELETE FROM conversation_messages")
    protected abstract suspend fun clearRefs()

    /** Выход из учётки: кэш чужой сессии не должен остаться на телефоне. */
    @Transaction
    open suspend fun clearAll() {
        clearRefs()
        clearConversations()
        clearMeta()
    }
}

@Database(
    entities = [ConversationEntity::class, MessageRefEntity::class, MetaEntity::class],
    version = 1,
    exportSchema = true,
)
abstract class CacheDb : RoomDatabase() {
    abstract fun dao(): CacheDao

    companion object {
        fun open(context: Context): CacheDb =
            Room.databaseBuilder(context, CacheDb::class.java, "cache.db").build()
    }
}
