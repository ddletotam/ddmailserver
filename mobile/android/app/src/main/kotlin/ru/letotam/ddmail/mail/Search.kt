package ru.letotam.ddmail.mail

import ru.letotam.ddmail.api.ContactDto
import ru.letotam.ddmail.sync.Conversation
import ru.letotam.ddmail.sync.MessageHit

/**
 * Выпадашка поиска над списком (§4 «Поиск» десктопа, docs/mobile-layout.md).
 * Секции по порядку: «Написать по адресу…», «Диалоги», «Контакты»,
 * «Сообщения». Других способов начать новое письмо нет.
 */
data class SearchResults(
    val query: String = "",
    /** Введённое похоже на адрес — первая строка «Написать по адресу …». */
    val composeEmail: String = "",
    val convs: List<ConvHit> = emptyList(),
    val contacts: List<ContactHit> = emptyList(),
    val messages: List<MsgHit> = emptyList(),
    /** Серверный поиск сообщений ещё идёт. */
    val loading: Boolean = false,
) {
    val open: Boolean get() = query.isNotBlank()
    val total: Int get() = convs.size + contacts.size + messages.size
}

/** Строка «Диалоги»: беседа и вторая строка — что совпало. */
data class ConvHit(val conv: Conversation, val detail: String)

data class ContactHit(val name: String, val email: String)

/** Строка «Сообщения»: письмо и беседа, в которой оно лежит. */
data class MsgHit(val ref: String, val from: String, val subject: String, val dateTs: Long)

/**
 * `parse_email_like` десктопа: одна «словоформа» с `@` и точкой в домене —
 * адрес в нижнем регистре; иначе `null`.
 */
internal fun parseEmailLike(q: String): String? {
    val s = q.trim()
    if (s.any { it.isWhitespace() || it == '<' || it == '>' || it == '"' || it == ',' }) return null
    val at = s.indexOf('@')
    if (at <= 0) return null
    val domain = s.substring(at + 1)
    if (domain.isEmpty() || domain.contains('@')) return null
    val dot = domain.indexOf('.')
    if (dot <= 0 || dot == domain.length - 1) return null
    return s.lowercase()
}

/**
 * «Диалоги» — только локально (§4 десктопа): по имени беседы, адресу
 * любого участника, теме последнего письма. Порядок — имя, адрес, тема,
 * внутри — как в списке; вторая строка говорит, что совпало.
 */
internal fun searchConvHits(convs: List<Conversation>, q: String): List<ConvHit> {
    val needle = q.trim().lowercase()
    if (needle.isEmpty()) return emptyList()
    return searchConversations(convs, needle).map { c ->
        val addr = c.counterparts.firstOrNull { it.addr.lowercase().contains(needle) || it.name.lowercase().contains(needle) }
        val detail = when {
            c.label.lowercase().contains(needle) -> c.counterparts.firstOrNull()?.addr.orEmpty()
            addr != null -> addr.addr
            c.receivedBy.lowercase().contains(needle) -> "→ ${c.receivedBy}"
            else -> c.lastSubject
        }
        ConvHit(c, if (detail == c.label) "" else detail)
    }
}

/**
 * «Контакты» — адресная книга: имя или адрес содержит запрос. Адреса
 * собеседников уже найденных диалогов не повторяются (`conv_hit_addrs`).
 */
internal fun searchContacts(book: List<ContactDto>, skip: Set<String>, q: String, limit: Int = 8): List<ContactHit> {
    val needle = q.trim().lowercase()
    if (needle.isEmpty()) return emptyList()
    val out = mutableListOf<ContactHit>()
    val seen = skip.toMutableSet()
    for (c in book) {
        val nameHit = c.fullName.lowercase().contains(needle)
        for (e in c.emails) {
            val email = e.trim().lowercase()
            if (email.isEmpty() || email in seen) continue
            if (nameHit || email.contains(needle)) {
                seen += email
                out += ContactHit(c.fullName.trim(), email)
            }
        }
        if (out.size >= limit) break
    }
    return out.take(limit)
}

/**
 * Письма серверного поиска → строки «Сообщения»: только те, что лежат в
 * беседе списка (спам в поиск не попадает), по одной строке на письмо.
 * Найти — по uid (у ddmail он — id письма, у IMAP — с ролью папки), а не
 * нашлось — по Message-ID.
 */
internal fun mapMessageHits(hits: List<MessageHit>, convs: List<Conversation>, limit: Int = 12): List<MsgHit> {
    fun norm(id: String) = id.trim().trimStart('<').trimEnd('>').trim().lowercase()
    val byAccount = convs.filter { !it.spam }.groupBy { it.accountKey }
    val out = mutableListOf<MsgHit>()
    val seen = mutableSetOf<String>()
    for (h in hits) {
        val list = byAccount[h.accountKey] ?: continue
        val mid = norm(h.messageId)
        val conv = list.firstOrNull { c -> c.messages.any { it.uid == h.uid && (h.folder.isEmpty() || it.folder == h.folder) } }
            ?: mid.takeIf { it.isNotEmpty() }?.let { m -> list.firstOrNull { c -> c.messages.any { norm(it.messageId) == m } } }
            ?: continue
        if (!seen.add("${h.accountKey}\u0000${h.folder}\u0000${h.uid}")) continue
        out += MsgHit(conv.ref, displayFrom(h.from), h.subject, h.dateTs)
        if (out.size >= limit) break
    }
    return out
}

/** `Имя <адрес>` → имя; голый адрес — он же. */
internal fun displayFrom(raw: String): String {
    val lt = raw.indexOf('<')
    val name = if (lt > 0) raw.substring(0, lt).trim().trim('"').trim() else ""
    return name.ifEmpty { raw.trim().trimStart('<').trimEnd('>') }
}
