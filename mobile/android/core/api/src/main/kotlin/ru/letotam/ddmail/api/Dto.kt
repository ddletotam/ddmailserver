package ru.letotam.ddmail.api

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonElement

// Формы ответов /api/desktop/v1 — зеркало Desktop*-структур
// internal/web/handlers_desktop.go. Лишние поля игнорируются, null у
// коллекций превращается в пустую (Go отдаёт nil-срез как null).

@Serializable
data class ContactInfo(val name: String = "", val addr: String = "")

@Serializable
data class MessageRef(
    val folder: String = "",
    val uid: Long = 0,
    /** Стабильный ключ письма (RFC Message-ID); uid — волатильный. */
    @SerialName("message_id") val messageId: String = "",
    val seen: Boolean = false,
    /**
     * Статус доставки своего исходящего: [Delivery.PENDING], [Delivery.DELIVERED],
     * [Delivery.FAILED]; пусто — неизвестно (входящие, старые письма, IMAP) —
     * считать доставленным. Смена статуса двигает `updated_at` беседы.
     */
    @SerialName("delivery_status") val deliveryStatus: String = "",
)

/** Статус доставки исходящего (десктоп §1, `delivery_status`). */
object Delivery {
    const val PENDING = "pending"
    const val DELIVERED = "delivered"
    const val FAILED = "failed"
}

@Serializable
data class ConversationDto(
    /** Задаёт сервер (набор адресов); клиент его не разбирает. */
    val id: String,
    val label: String = "",
    @SerialName("avatar_hash") val avatarHash: String = "",
    @SerialName("received_by") val receivedBy: String = "",
    val counterparts: List<ContactInfo> = emptyList(),
    @SerialName("is_group") val isGroup: Boolean = false,
    /** Секунды. */
    @SerialName("last_date_ts") val lastDateTs: Long = 0,
    @SerialName("last_subject") val lastSubject: String = "",
    @SerialName("unread_count") val unreadCount: Int = 0,
    @SerialName("total_count") val totalCount: Int = 0,
    val messages: List<MessageRef> = emptyList(),
    /** Миллисекунды, часы сервера. */
    @SerialName("updated_at") val updatedAt: Long = 0,
    /** Беседа из спама (`/conversations?spam=1`): место ей — только папка «Спам». */
    val spam: Boolean = false,
    /** Статус доставки последнего письма беседы, если оно наше (см. [MessageRef.deliveryStatus]). */
    @SerialName("last_delivery_status") val lastDeliveryStatus: String = "",
)

/** `/conversations?since=` — изменившиеся беседы и водяной знак часов сервера. */
@Serializable
data class ConversationsDelta(
    @SerialName("server_now_ms") val serverNowMs: Long = 0,
    val conversations: List<ConversationDto> = emptyList(),
)

@Serializable
data class ChangeEntry(
    val seq: Long = 0,
    @SerialName("message_id") val messageId: String = "",
    val kind: Int = 0,
) {
    companion object {
        const val UPSERT = 1
        const val DELETE = 2
    }
}

/** Хвост журнала изменений; `reset` — курсор негоден, нужна полная синхронизация. */
@Serializable
data class ChangesResponse(
    val entries: List<ChangeEntry> = emptyList(),
    @SerialName("latest_seq") val latestSeq: Long = 0,
    @SerialName("low_watermark") val lowWatermark: Long = 0,
    val reset: Boolean = false,
)

/** Документ папок диалогов: сервер его не толкует, хранит с версией. */
@Serializable
data class DialogFoldersResponse(
    val version: Long = 0,
    val doc: JsonElement? = null,
)

@Serializable
data class AttachmentDto(
    val filename: String = "",
    @SerialName("mime_type") val mimeType: String = "",
    val size: Long = 0,
    val index: Int = 0,
)

/** Тело письма (`/conversations/messages`). */
@Serializable
data class MessageBody(
    val uid: Long,
    val folder: String = "",
    val subject: String = "",
    val from: String = "",
    @SerialName("from_addr") val fromAddr: String = "",
    val to: List<String> = emptyList(),
    val cc: List<String> = emptyList(),
    /** Секунды. */
    @SerialName("date_ts") val dateTs: Long = 0,
    val html: String? = null,
    val text: String? = null,
    val attachments: List<AttachmentDto> = emptyList(),
    @SerialName("is_outgoing") val isOutgoing: Boolean = false,
    @SerialName("message_id") val messageId: String = "",
    @SerialName("in_reply_to") val inReplyTo: String = "",
    val references: List<String> = emptyList(),
    /** Доставка своего исходящего: статус ([Delivery]); пусто — считать доставленным. */
    @SerialName("delivery_status") val deliveryStatus: String = "",
    /** Код причины: у `failed` — почему не доставлено, у `pending` — почему не удалась последняя попытка. */
    @SerialName("delivery_reason") val deliveryReason: String = "",
    /** Причина словами на языке пользователя (готовый текст сервера). */
    @SerialName("delivery_error") val deliveryError: String = "",
    /** Кому не доставлено (частичный отказ — тоже `failed`). */
    @SerialName("delivery_failed") val deliveryFailed: List<String> = emptyList(),
) {
    /** Ни HTML, ни текста, ни вложений — сбойный фетч, перезапросить (§4а десктопа). */
    val isBlank: Boolean get() = html.isNullOrBlank() && text.isNullOrBlank() && attachments.isEmpty()
}

/**
 * WS-событие `delivery_status`: статус доставки письма сменился. [messageId] —
 * id копии в «Отправленных» (он же `uid` в ссылках бесед), [rfcMessageId] —
 * её Message-ID; остальные поля — как в [MessageBody].
 */
@Serializable
data class DeliveryEvent(
    @SerialName("message_id") val messageId: Long = 0,
    @SerialName("rfc_message_id") val rfcMessageId: String = "",
    @SerialName("delivery_status") val status: String = "",
    @SerialName("delivery_reason") val reason: String = "",
    @SerialName("delivery_error") val error: String = "",
    @SerialName("delivery_failed") val failed: List<String> = emptyList(),
)

/** Свой адрес (`/identities`): от него можно отправлять. */
@Serializable
data class Identity(
    val email: String = "",
    val name: String = "",
    val signature: String = "",
    @SerialName("is_default") val isDefault: Boolean = false,
)

@Serializable
data class OutgoingAttachment(
    val filename: String,
    @SerialName("mime_type") val mimeType: String,
    /** Байты; в JSON — base64 (как `[]byte` у Go). */
    @Serializable(with = Base64Bytes::class) val content: ByteArray,
    @SerialName("content_id") val contentId: String? = null,
)

/** Тело `/send`. Отправка асинхронная: сервер ставит письмо в очередь. */
@Serializable
data class SendRequest(
    val from: String,
    val to: List<String>,
    val cc: List<String> = emptyList(),
    val subject: String,
    val html: String,
    val text: String,
    @SerialName("in_reply_to") val inReplyTo: String = "",
    val references: String = "",
    val attachments: List<OutgoingAttachment> = emptyList(),
)

object Base64Bytes : kotlinx.serialization.KSerializer<ByteArray> {
    override val descriptor = kotlinx.serialization.descriptors.PrimitiveSerialDescriptor(
        "Base64Bytes", kotlinx.serialization.descriptors.PrimitiveKind.STRING,
    )
    override fun serialize(encoder: kotlinx.serialization.encoding.Encoder, value: ByteArray) =
        encoder.encodeString(java.util.Base64.getEncoder().encodeToString(value))
    override fun deserialize(decoder: kotlinx.serialization.encoding.Decoder): ByteArray =
        java.util.Base64.getDecoder().decode(decoder.decodeString())
}

/** Итог «В спам»: сколько писем удалено и какое правило заведено первым. */
@Serializable
data class PurgeOutcome(
    val deleted: Long = 0,
    @SerialName("rule_type") val ruleType: String = "",
    @SerialName("rule_value") val ruleValue: String = "",
    @SerialName("rule_count") val ruleCount: Int = 0,
)

/**
 * Письмо, найденное серверным поиском (`/search`, документ индекса
 * Meilisearch). `id` — id письма на сервере, он же `uid` в ссылках бесед.
 */
@Serializable
data class SearchHitDto(
    val id: Long = 0,
    val subject: String = "",
    val from: String = "",
    /** Секунды. */
    val date: Long = 0,
    @SerialName("message_id") val messageId: String = "",
    /** `email` или `calendar`: события календаря поиску писем не нужны. */
    val type: String = "",
)
