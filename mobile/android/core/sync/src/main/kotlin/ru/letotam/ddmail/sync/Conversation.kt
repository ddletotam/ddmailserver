package ru.letotam.ddmail.sync

import ru.letotam.ddmail.api.ContactInfo
import ru.letotam.ddmail.api.MessageRef

/** Беседа для UI. Семантика — сервера и десктопного контракта (§4). */
data class Conversation(
    val accountKey: String,
    val id: String,
    val label: String,
    val receivedBy: String,
    val counterparts: List<ContactInfo>,
    val isGroup: Boolean,
    /** Секунды. */
    val lastDateTs: Long,
    val lastSubject: String,
    val unreadCount: Int,
    val totalCount: Int,
    val messages: List<MessageRef>,
    /** Из спама: место — только системная папка «Спам» (её состав задаёт сервер). */
    val spam: Boolean = false,
    /**
     * Статус доставки последнего письма, если оно наше исходящее (сервер
     * ddmail, `last_delivery_status`): `failed` / `pending` — значок у времени
     * в строке списка; пусто и `delivered` — ничего.
     */
    val lastDeliveryStatus: String = "",
) {
    /**
     * Ключ беседы в интерфейсе: id уникален только внутри учётки, одинаковые
     * id в разных учётках — разные беседы (mobile-external-accounts §1).
     */
    val ref: String get() = refOf(accountKey, id)

    companion object {
        fun refOf(accountKey: String, id: String) = "$accountKey\u0000$id"
    }
}

/** Ключ ручного назначения в папку — как `MergeKey` десктопа. */
data class DialogKey(val account: String, val id: String)

val Conversation.key: DialogKey get() = DialogKey(accountKey, id)
