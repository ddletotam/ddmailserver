package ru.letotam.ddmail.sync

import kotlinx.coroutines.flow.Flow
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.api.Binary
import ru.letotam.ddmail.api.Identity
import ru.letotam.ddmail.api.MessageBody
import ru.letotam.ddmail.api.MessageRef
import ru.letotam.ddmail.api.PurgeOutcome
import ru.letotam.ddmail.api.SendRequest
import ru.letotam.ddmail.imap.MailException
import ru.letotam.ddmail.imap.humanText

/** Итог «В спам». */
sealed interface SpamOutcome {
    /** Сервер ddmail: письма удалены, отправитель заблокирован правилом. */
    data class Rule(val outcome: PurgeOutcome) : SpamOutcome

    /** IMAP: письма перенесены в папку «Спам» (Junk) ящика. */
    data class Moved(val messages: Int) : SpamOutcome
}

/**
 * Письмо, найденное поиском на источнике (секция «Сообщения»). [folder]
 * пустой — источник папку не сообщает (ddmail: `uid` уникален сам по себе).
 */
data class MessageHit(
    val accountKey: String,
    val folder: String,
    val uid: Long,
    val messageId: String,
    val from: String,
    val subject: String,
    /** Секунды. */
    val dateTs: Long,
)

/**
 * Источник почты одной учётки (mobile-external-accounts §1). Сервер ddmail
 * ([SyncEngine]) и внешний IMAP ([ImapEngine]) — две реализации; экран не
 * знает, откуда пришла беседа: все правки идут через [MailHub] по
 * `Conversation.accountKey`.
 *
 * Ошибки: [ApiException] у ddmail, [ru.letotam.ddmail.imap.MailException] у
 * IMAP; толкование — [isAuthError] и [errorText].
 */
interface MailSource {
    val accountKey: String

    /** Подпись учётки для людей: адрес или `логин · сервер`. */
    val title: String

    /** Беседы учётки вместе со спамом (`spam = true`). */
    val conversations: Flow<List<Conversation>>

    /** Свои адреса учётки (From, «свои» в беседах). */
    val identities: Flow<List<Identity>>

    /** «В спам» с правилами «адрес/домен» — только у сервера ddmail. */
    val spamRules: Boolean

    /** Есть куда отправлять в спам (у IMAP — найдена папка Junk). */
    suspend fun canSpam(): Boolean

    suspend fun sync()

    suspend fun bodies(refs: List<MessageRef>): List<MessageBody>

    suspend fun prefetchBodies(allowed: suspend () -> Boolean, batch: Int = 30): Int

    suspend fun markRead(c: Conversation)

    suspend fun markUnread(c: Conversation)

    suspend fun deleteConversations(convs: Collection<Conversation>)

    /**
     * Отдельные письма беседы [c] — меню пузыря «Удалить письмо» (десктоп,
     * меню пузыря): из кэша сразу, на сервере следом; беседа без писем уходит
     * целиком. Необратимо, как удаление беседы.
     */
    suspend fun deleteMessages(c: Conversation, refs: List<MessageRef>)

    /** Письмо целиком (RFC 822) — «Показать → Заголовки / Исходник сообщения». */
    suspend fun source(uid: Long): ByteArray

    /** «В спам»; [scope] (`address`/`domain`) значим только при [spamRules]. */
    suspend fun spam(convs: Collection<Conversation>, scope: String): SpamOutcome

    /** «Не спам»: во «Входящие». [trust] — только у ddmail. */
    suspend fun restoreSpam(convs: Collection<Conversation>, trust: Boolean): Int

    suspend fun send(req: SendRequest)

    /** Поиск писем на источнике; поиска у источника нет — пусто. */
    suspend fun searchMessages(query: String, limit: Int): List<MessageHit> = emptyList()

    suspend fun part(uid: Long, cid: String): Binary

    suspend fun attachment(uid: Long, index: Int): Binary

    /** Учётку удаляют: её кэш уходит с телефона. */
    suspend fun forget()

    /** Освободить соединения (IMAP); у ddmail — нечего. */
    fun close() {}
}

/**
 * Письмо IMAP-учётки не ушло: свой SMTP отказал или недоступен. Статус
 * доставки у таких учёток определяет клиент (контракт Android §5): SMTP принял
 * — доставлено, отказал — «Не доставлено» с причиной [reason].
 */
class SmtpFailed(val reason: Throwable) : Exception(reason.message, reason)

/** Отказ входа: ретраи не лечат, нужен вход (контракт §7). */
fun Throwable.isAuthError(): Boolean =
    this is ApiException.Unauthorized || this is MailException.Auth || (this is SmtpFailed && reason.isAuthError())

/** Ошибка, которую можно показать человеку. */
fun Throwable.errorText(): String = when (this) {
    is SmtpFailed -> reason.errorText()
    is MailException -> humanText()
    is ApiException.Unauthorized -> "сессия истекла — войдите заново"
    is ApiException.Network -> "нет соединения с сервером"
    is ApiException.Server -> serverMessage.ifBlank { "сервер ответил ошибкой $code" }
    else -> message ?: javaClass.simpleName
}

/** Ошибка источника почты, а не программы. */
fun Throwable.isMailError(): Boolean = this is ApiException || this is MailException || this is SmtpFailed
