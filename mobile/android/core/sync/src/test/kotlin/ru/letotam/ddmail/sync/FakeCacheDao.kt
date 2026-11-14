package ru.letotam.ddmail.sync

import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flowOf
import ru.letotam.ddmail.db.CacheDao
import ru.letotam.ddmail.db.ConversationEntity
import ru.letotam.ddmail.db.ImapFolderEntity
import ru.letotam.ddmail.db.ImapHeaderEntity
import ru.letotam.ddmail.db.MessageBodyEntity
import ru.letotam.ddmail.db.MessageRefEntity
import ru.letotam.ddmail.db.MetaEntity
import ru.letotam.ddmail.db.ReminderEntity

/**
 * Кэш в памяти для тестов синка: метаданные — в карте, беседы принимаются и
 * забываются. Прочее (IMAP, тела, напоминания) синку бесед не нужно — падает.
 */
class FakeCacheDao : CacheDao() {
    val metaMap = mutableMapOf<String, String>()

    override fun observeConversations(account: String): Flow<List<ConversationEntity>> = flowOf(emptyList())
    override fun observeRefs(account: String): Flow<List<MessageRefEntity>> = flowOf(emptyList())
    protected override suspend fun insertConversations(c: List<ConversationEntity>) {}
    protected override suspend fun insertRefs(r: List<MessageRefEntity>) {}
    protected override suspend fun deleteAllConversations(account: String) {}
    protected override suspend fun deleteAllRefs(account: String) {}
    protected override suspend fun deleteRefsOf(account: String, ids: List<String>) {}
    protected override suspend fun deleteRefsByMessageId(account: String, messageIds: List<String>): Int = 0
    protected override suspend fun deleteBodiesByMessageId(account: String, messageIds: List<String>) {}
    protected override suspend fun deleteEmptyConversations(account: String) {}
    override suspend fun bodies(account: String, uids: List<Long>): List<MessageBodyEntity> = error("bodies")
    override suspend fun putBodies(b: List<MessageBodyEntity>) { error("putBodies") }
    override suspend fun cachedBodyUids(account: String): List<Long> = error("cachedBodyUids")
    protected override suspend fun setRefDelivery(account: String, uid: Long, messageId: String, status: String) { error("setRefDelivery") }
    protected override suspend fun setBodyDelivery(account: String, uid: Long, messageId: String, status: String, reason: String, error: String, failedJson: String) { error("setBodyDelivery") }
    protected override suspend fun seeRefs(account: String, conv: String) { error("seeRefs") }
    protected override suspend fun zeroUnread(account: String, conv: String) { error("zeroUnread") }
    protected override suspend fun dropRefs(account: String, ids: List<String>) { error("dropRefs") }
    protected override suspend fun dropConversations(account: String, ids: List<String>) { error("dropConversations") }
    protected override suspend fun dropRefsByUid(account: String, uids: List<Long>) { error("dropRefsByUid") }
    protected override suspend fun dropBodiesByUid(account: String, uids: List<Long>) { error("dropBodiesByUid") }
    protected override suspend fun unseeRef(account: String, conv: String, uid: Long) { error("unseeRef") }
    protected override suspend fun bumpUnread(account: String, conv: String) { error("bumpUnread") }
    override suspend fun meta(key: String): String? = metaMap[key]
    override fun observeMeta(key: String): Flow<String?> = flowOf(metaMap[key])
    protected override suspend fun putMeta(m: MetaEntity) { metaMap[m.key] = m.value }
    override suspend fun deleteMeta(key: String) {}
    override suspend fun reminders(): List<ReminderEntity> = error("reminders")
    override suspend fun reminder(eventId: Long, occStart: Long): ReminderEntity? = error("reminder")
    override suspend fun putReminders(r: List<ReminderEntity>) { error("putReminders") }
    protected override suspend fun deleteReminder(eventId: Long, occStart: Long) { error("deleteReminder") }
    override suspend fun deleteCalendarReminders(calendarId: Long) { error("deleteCalendarReminders") }
    protected override suspend fun clearReminders() { error("clearReminders") }
    protected override suspend fun clearMeta() { error("clearMeta") }
    protected override suspend fun clearConversations() { error("clearConversations") }
    protected override suspend fun clearRefs() { error("clearRefs") }
    protected override suspend fun clearBodies() { error("clearBodies") }
    override suspend fun imapFolders(account: String): List<ImapFolderEntity> = error("imapFolders")
    override suspend fun imapHeaders(account: String): List<ImapHeaderEntity> = error("imapHeaders")
    override suspend fun imapUids(account: String, role: Int): List<Long> = error("imapUids")
    override suspend fun imapFlags(account: String, role: Int): List<ImapFlags> = error("imapFlags")
    protected override suspend fun putImapHeaders(h: List<ImapHeaderEntity>) { error("putImapHeaders") }
    protected override suspend fun putImapFolder(f: ImapFolderEntity) { error("putImapFolder") }
    protected override suspend fun dropImapRole(account: String, role: Int) { error("dropImapRole") }
    protected override suspend fun dropImapHeaders(account: String, role: Int, uids: List<Long>) { error("dropImapHeaders") }
    protected override suspend fun dropBodies(account: String, uids: List<Long>) { error("dropBodies") }
    protected override suspend fun dropBodiesRange(account: String, from: Long, to: Long) { error("dropBodiesRange") }
    protected override suspend fun setImapFlags(account: String, role: Int, uid: Long, seen: Boolean, flagged: Boolean, draft: Boolean, at: Long) { error("setImapFlags") }
    protected override suspend fun setImapSeen(account: String, role: Int, uids: List<Long>, seen: Boolean, at: Long) { error("setImapSeen") }
    protected override suspend fun dropImapFolder(account: String, role: Int) { error("dropImapFolder") }
    protected override suspend fun clearImapHeaders(account: String) { error("clearImapHeaders") }
    protected override suspend fun clearImapFolders(account: String) { error("clearImapFolders") }
    protected override suspend fun clearAccountBodies(account: String) { error("clearAccountBodies") }
    protected override suspend fun clearMetaPrefix(prefix: String) { error("clearMetaPrefix") }
    protected override suspend fun clearAllImapHeaders() { error("clearAllImapHeaders") }
    protected override suspend fun clearAllImapFolders() { error("clearAllImapFolders") }
}
