package ru.letotam.ddmail.conv

import ru.letotam.ddmail.api.ContactInfo
import ru.letotam.ddmail.api.ConversationDto
import ru.letotam.ddmail.api.MessageRef
import java.security.MessageDigest

/** Письмо для склейки бесед: то, что лежит в кэше заголовков. */
data class ConvMessage(
    val folder: String,
    /** Папка «Отправленные»: её копия побеждает при дублях по Message-ID. */
    val isSent: Boolean,
    /** UID для ссылки на письмо (в кэше — упакованный с ролью папки). */
    val uid: Long,
    val fromName: String,
    val fromAddr: String,
    val toAddrs: List<String>,
    val toNames: List<String>,
    val ccAddrs: List<String>,
    val ccNames: List<String>,
    val subject: String,
    /** Секунды. */
    val dateTs: Long,
    val seen: Boolean,
    val draft: Boolean,
    val messageId: String,
    /** Миллисекунды: когда строка кэша менялась (для `updated_at`). */
    val updatedAt: Long = 0,
)

/**
 * Беседы из заголовков — порт `fetch_conversations_impl` десктопа
 * (`desktop/core/src/imap.rs`) и `groupConversations` сервера
 * (`internal/web/handlers_desktop.go`): один и тот же ящик приходит и через
 * ddmail, и по IMAP, id обязаны совпасть.
 *
 * Где десктоп и сервер расходятся, берётся сервер (контракт
 * mobile-external-accounts §3):
 * - нашего адреса среди видимых нет (bcc) — в набор добавляется адрес
 *   учётки [own], как `accountEmail` сервера: так у входящего `{bob}` и
 *   нашего ответа `{me, bob}` один набор, один id и одна беседа (у десктопа —
 *   две строки с одним id), а `N чел` у группы считает и нас;
 * - имя собеседника — первое письмо, где он есть с именем (From, затем
 *   To, затем Cc), как `nameForAddr`;
 * - тема группы обрезается по краям, как у сервера.
 * Группировка — по готовому id.
 */
object Conversations {
    fun build(messages: List<ConvMessage>, ourAddrs: Collection<String>, own: String): List<ConversationDto> {
        val ours = ourAddrs.map { it.trim().lowercase() }.filter { it.isNotEmpty() }.toMutableSet()
        val ownLc = own.trim().lowercase()
        if (ownLc.isNotEmpty()) ours += ownLc
        fun isOurs(a: String) = a.lowercase() in ours

        class Group(val participants: List<String>, val msgs: MutableList<ConvMessage> = mutableListOf())
        val groups = LinkedHashMap<String, Group>()
        for (m in messages) {
            var participants = participants(m.fromAddr, m.toAddrs, m.ccAddrs)
            if (participants.isEmpty()) continue
            if (participants.none(::isOurs) && ownLc.isNotEmpty()) participants = (participants + ownLc).sorted()
            val mine = participants.filter(::isOurs)
            val others = participants.filter { !isOurs(it) }
            val id = conversationId(mine, others, ownLc)
            groups.getOrPut(id) { Group(participants) }.msgs += m
        }

        val out = mutableListOf<ConversationDto>()
        for ((id, g) in groups) {
            // Устойчивая сортировка по дате; при равенстве — порядок входа.
            val sorted = g.msgs.sortedBy { it.dateTs }
            val msgs = dedup(sorted)
            val regular = msgs.filter { !it.draft }
            // Черновики сами беседу не начинают (как у сервера и десктопа).
            if (regular.isEmpty()) continue
            val first = regular.first()
            val last = regular.last()
            val participants = g.participants
            val mine = participants.filter(::isOurs)
            val others = participants.filter { !isOurs(it) }

            val cpAddrs: List<String>
            val myId: String
            if (others.isNotEmpty()) {
                cpAddrs = others
                val firstFrom = first.fromAddr.lowercase()
                myId = if (isOurs(firstFrom)) firstFrom else {
                    (first.toAddrs + first.ccAddrs).map { it.lowercase() }.firstOrNull(::isOurs)
                        ?: mine.firstOrNull()
                        ?: ownLc
                }
            } else {
                // Все адреса — мои: собеседник — отправитель первого письма.
                val fromLc = first.fromAddr.lowercase()
                val cp = if (fromLc in participants) fromLc else participants[0]
                cpAddrs = listOf(cp)
                myId = participants.firstOrNull { it != cp } ?: cp
            }

            val counterparts = cpAddrs.map { ContactInfo(nameFor(msgs, it), it) }
            val isGroup = others.size > 1
            val label = when {
                isGroup -> "${first.subject.trim().ifEmpty { "(без темы)" }} (${participants.size} чел)"
                else -> counterparts[0].name.ifEmpty { counterparts[0].addr }
            }
            out += ConversationDto(
                id = id,
                label = label,
                avatarHash = md5(cpAddrs[0]),
                receivedBy = myId,
                counterparts = counterparts,
                isGroup = isGroup,
                lastDateTs = last.dateTs,
                lastSubject = last.subject,
                unreadCount = regular.count { !it.seen },
                totalCount = regular.size,
                messages = regular.map { MessageRef(it.folder, it.uid, it.messageId, it.seen) },
                updatedAt = msgs.maxOf { it.updatedAt },
            )
        }
        return out.sortedByDescending { it.lastDateTs }
    }

    /** `conversation_participants`: From+To+Cc, нижний регистр, без повторов, по алфавиту. */
    fun participants(from: String, to: List<String>, cc: List<String>): List<String> {
        val seen = LinkedHashSet<String>()
        (listOf(from) + to + cc).forEach { a ->
            val lc = a.trim().lowercase()
            if (lc.isNotEmpty()) seen += lc
        }
        return seen.sorted()
    }

    /** `conversation_id` — байт в байт с десктопом и сервером. */
    fun conversationId(mine: List<String>, others: List<String>, own: String): String {
        val mineKey = if (mine.isEmpty() && own.isNotEmpty()) own.lowercase() else mine.joinToString("+")
        return when {
            others.isEmpty() -> "$mineKey|self"
            others.size > 1 -> "$mineKey|group:${others.joinToString(",")}"
            else -> "$mineKey|${others.joinToString(",")}"
        }
    }

    /** Дубли по Message-ID: остаётся одна копия, из «Отправленных», если есть. */
    private fun dedup(msgs: List<ConvMessage>): List<ConvMessage> {
        if (msgs.none { it.messageId.isNotEmpty() }) return msgs
        val keep = BooleanArray(msgs.size) { true }
        val seen = HashMap<String, Int>()
        msgs.forEachIndexed { i, m ->
            if (m.messageId.isEmpty()) return@forEachIndexed
            val j = seen[m.messageId]
            if (j == null) {
                seen[m.messageId] = i
            } else if (m.isSent && !msgs[j].isSent) {
                keep[j] = false
                seen[m.messageId] = i
            } else {
                keep[i] = false
            }
        }
        return msgs.filterIndexed { i, _ -> keep[i] }
    }

    /** Имя собеседника: первое письмо, где он подписан не адресом (From, To, Cc). */
    private fun nameFor(msgs: List<ConvMessage>, addr: String): String {
        for (m in msgs) {
            if (m.fromAddr.equals(addr, ignoreCase = true)) {
                cleanDisplayName(m.fromName).takeIf { it.isNotEmpty() }?.let { return it }
            }
            for ((a, n) in m.toAddrs.zip(m.toNames) + m.ccAddrs.zip(m.ccNames)) {
                if (a.equals(addr, ignoreCase = true)) cleanDisplayName(n).takeIf { it.isNotEmpty() }?.let { return it }
            }
        }
        return ""
    }

    /** `clean_display_name`: без `<…>`, без `(…@…)`; похоже на адрес — не имя. */
    fun cleanDisplayName(s: String): String {
        var r = s
        r.indexOf('<').takeIf { it >= 0 }?.let { r = r.substring(0, it) }
        val start = r.indexOf('(')
        val end = r.indexOf(')')
        if (start >= 0 && end > start && r.substring(start, end).contains('@')) r = r.substring(0, start)
        val t = r.trim().trim('"', '\'').trim()
        return if (t.contains('@')) "" else t
    }

    /** `gravatar_hash`: md5 адреса в нижнем регистре. */
    fun md5(email: String): String =
        MessageDigest.getInstance("MD5").digest(email.trim().lowercase().toByteArray())
            .joinToString("") { "%02x".format(it) }
}
