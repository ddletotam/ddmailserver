package ru.letotam.ddmail.sync

import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.combine
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.builtins.serializer
import kotlinx.serialization.json.Json
import ru.letotam.ddmail.api.AttachmentDto
import ru.letotam.ddmail.api.ContactInfo
import ru.letotam.ddmail.api.ConversationDto
import ru.letotam.ddmail.api.MessageBody
import ru.letotam.ddmail.api.MessageRef
import ru.letotam.ddmail.db.CacheDao
import ru.letotam.ddmail.db.ConversationEntity
import ru.letotam.ddmail.db.MessageBodyEntity
import ru.letotam.ddmail.db.MessageRefEntity

/** Строки кэша ↔ доменные типы — общее для источников, кроме SyncEngine (у него свои). */
internal object CacheRows {
    val json = Json { ignoreUnknownKeys = true }
    val contacts = ListSerializer(ContactInfo.serializer())
    val strings = ListSerializer(String.serializer())
    val atts = ListSerializer(AttachmentDto.serializer())

    /** Беседы учётки и её спама ([spamKey]) из кэша; порядок — по дате. */
    fun conversations(dao: CacheDao, accountKey: String, spamKey: String): Flow<List<Conversation>> =
        combine(
            dao.observeConversations(accountKey),
            dao.observeRefs(accountKey),
            dao.observeConversations(spamKey),
            dao.observeRefs(spamKey),
        ) { convs, refs, spamConvs, spamRefs ->
            val byConv = refs.groupBy { it.conversationId }
            val bySpam = spamRefs.groupBy { it.conversationId }
            val ham = convs.map { it.toDomain(byConv[it.id].orEmpty()) }
            if (spamConvs.isEmpty()) return@combine ham
            val spam = spamConvs.map { it.toDomain(bySpam[it.id].orEmpty()).copy(accountKey = accountKey, spam = true) }
            (ham + spam).sortedByDescending { it.lastDateTs }
        }

    fun ConversationEntity.toDomain(refs: List<MessageRefEntity>) = Conversation(
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

    fun ConversationDto.toEntity(accountKey: String) = ConversationEntity(
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

    fun MessageBody.toEntity(accountKey: String) = MessageBodyEntity(
        accountKey = accountKey,
        uid = uid,
        messageId = messageId,
        folder = folder,
        subject = subject,
        fromName = from,
        fromAddr = fromAddr,
        toJson = json.encodeToString(strings, to),
        ccJson = json.encodeToString(strings, cc),
        dateTs = dateTs,
        html = html,
        text = text,
        attachmentsJson = json.encodeToString(atts, attachments),
        isOutgoing = isOutgoing,
        inReplyTo = inReplyTo,
        referencesJson = json.encodeToString(strings, references),
    )

    fun MessageBodyEntity.toDomain() = MessageBody(
        uid = uid,
        folder = folder,
        subject = subject,
        from = fromName,
        fromAddr = fromAddr,
        to = runCatching { json.decodeFromString(strings, toJson) }.getOrDefault(emptyList()),
        cc = runCatching { json.decodeFromString(strings, ccJson) }.getOrDefault(emptyList()),
        dateTs = dateTs,
        html = html,
        text = text,
        attachments = runCatching { json.decodeFromString(atts, attachmentsJson) }.getOrDefault(emptyList()),
        isOutgoing = isOutgoing,
        messageId = messageId,
        inReplyTo = inReplyTo,
        references = runCatching { json.decodeFromString(strings, referencesJson) }.getOrDefault(emptyList()),
    )
}
