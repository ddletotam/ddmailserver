package ru.letotam.ddmail.imap

import jakarta.mail.internet.InternetHeaders
import jakarta.mail.internet.MimeUtility
import ru.letotam.ddmail.mime.MailDates

/**
 * Заголовки письма из `UID FETCH (UID FLAGS ENVELOPE INTERNALDATE
 * BODY.PEEK[HEADER.FIELDS (…)])` — то, из чего на устройстве строятся беседы
 * (контракт §3). Смысл полей — `RawEnvelope` десктопа (`imap.rs`).
 */
data class MessageHeader(
    val uid: Long,
    val seen: Boolean,
    val flagged: Boolean,
    val draft: Boolean,
    val subject: String,
    /** Имя из сырого `From` (регистр сохранён, RFC 2047 раскодирован). */
    val fromName: String,
    /** Адрес в нижнем регистре. */
    val fromAddr: String,
    val toAddrs: List<String>,
    /** Имя адресата или его адрес, если имени нет — параллельно [toAddrs]. */
    val toNames: List<String>,
    val ccAddrs: List<String>,
    val ccNames: List<String>,
    /** Секунды: `Date`, не разобралась — INTERNALDATE. */
    val dateTs: Long,
    val messageId: String,
    val inReplyTo: String,
    val references: List<String>,
    val hasAttachments: Boolean,
)

/** Нетегированный FETCH: пары «имя значение» в карту (имя — в верхнем регистре). */
fun fetchItems(d: Resp.Data): Map<String, IVal> {
    val list = d.items.getOrNull(2)?.list ?: return emptyMap()
    val m = LinkedHashMap<String, IVal>()
    var i = 0
    while (i + 1 < list.size) {
        val k = list[i].str?.uppercase() ?: break
        m[k] = list[i + 1]
        i += 2
    }
    return m
}

fun Map<String, IVal>.flags(): Set<String> = this["FLAGS"]?.list?.mapNotNull { it.str?.uppercase() }?.toSet().orEmpty()

/** Значение секции `BODY[...]` (имя ключа с секцией). */
fun Map<String, IVal>.section(prefix: String): ByteArray? =
    entries.firstOrNull { it.key.startsWith(prefix) }?.value?.bytes

object Headers {
    const val FIELDS = "FROM TO CC DATE SUBJECT MESSAGE-ID IN-REPLY-TO REFERENCES CONTENT-TYPE"
    const val FETCH_ITEMS = "(UID FLAGS ENVELOPE INTERNALDATE BODY.PEEK[HEADER.FIELDS ($FIELDS)])"

    fun parse(items: Map<String, IVal>): MessageHeader? {
        val uid = items["UID"]?.long ?: return null
        val env = items["ENVELOPE"]?.list ?: return null
        val flags = items.flags()
        val raw = items.section("BODY[HEADER") ?: ByteArray(0)
        val hdr = runCatching { InternetHeaders(raw.inputStream()) }.getOrNull()
        fun header(name: String) = hdr?.getHeader(name, null)?.let { MimeUtility.unfold(it) }

        val subject = decode(env.getOrNull(1)?.bytes)
        val (fromName, fromAddr) = parseFromHeader(header("From")?.let(::decodeText))
            ?: env.getOrNull(2)?.list?.firstOrNull()?.let { name(it) to addr(it) }
            ?: ("" to "")
        val to = addresses(env.getOrNull(5))
        val cc = addresses(env.getOrNull(6))
        val date = env.getOrNull(0)?.str
        val dateTs = MailDates.parse(date).takeIf { it > 0 } ?: MailDates.parse(items["INTERNALDATE"]?.str)
        val messageId = normalizeMsgId(env.getOrNull(9)?.str.orEmpty())
        val inReplyTo = normalizeMsgId(env.getOrNull(8)?.str.orEmpty())
        val refs = extractMsgIds(header("References").orEmpty())
        return MessageHeader(
            uid = uid,
            seen = "\\SEEN" in flags,
            flagged = "\\FLAGGED" in flags,
            draft = "\\DRAFT" in flags,
            subject = subject,
            fromName = fromName,
            fromAddr = fromAddr,
            toAddrs = to.map { it.second },
            toNames = to.map { (n, a) -> n.ifEmpty { a } },
            ccAddrs = cc.map { it.second },
            ccNames = cc.map { (n, a) -> n.ifEmpty { a } },
            dateTs = dateTs,
            messageId = messageId,
            inReplyTo = inReplyTo,
            references = refs,
            hasAttachments = header("Content-Type").orEmpty().lowercase().contains("multipart/mixed"),
        )
    }

    /** Список адресов ENVELOPE; маркеры групп (host NIL) пропускаются. */
    private fun addresses(v: IVal?): List<Pair<String, String>> =
        v?.list.orEmpty().mapNotNull { a ->
            val parts = a.list
            if (parts.size < 4 || parts[3] is IVal.Nil) return@mapNotNull null
            name(a) to addr(a)
        }.filter { it.second.isNotEmpty() }

    /** `addr_str` десктопа: `mailbox@host` в нижнем регистре без `<>`. */
    private fun addr(a: IVal): String {
        val p = a.list
        val mbox = p.getOrNull(2)?.bytes?.let { String(it, Charsets.UTF_8) }.orEmpty().replace("<", "").replace(">", "")
        val host = p.getOrNull(3)?.bytes?.let { String(it, Charsets.UTF_8) }.orEmpty().replace("<", "").replace(">", "")
        return "$mbox@$host".lowercase().trimEnd('@')
    }

    private fun name(a: IVal): String = decode(a.list.getOrNull(0)?.bytes)

    /** RFC 2047 в поле ENVELOPE; сырой UTF-8 (SMTPUTF8) тоже читается. */
    fun decode(b: ByteArray?): String {
        if (b == null) return ""
        return decodeText(String(b, Charsets.UTF_8))
    }

    fun decodeText(s: String): String = try {
        MimeUtility.decodeText(s)
    } catch (e: Exception) {
        s
    }

    /** `parse_from_header` десктопа: «Имя <адрес>» или голый адрес. */
    fun parseFromHeader(value: String?): Pair<String, String>? {
        val v = value ?: return null
        val lt = v.lastIndexOf('<')
        val gt = v.lastIndexOf('>')
        val (name, addr) = if (lt >= 0 && gt > lt) {
            v.substring(0, lt).trim().trim('"') to v.substring(lt + 1, gt).trim().lowercase()
        } else {
            "" to v.trim().lowercase()
        }
        if (addr.isEmpty()) return null
        return name to addr
    }

    /** `normalize_msg_id`: без `<>` и пробелов. */
    fun normalizeMsgId(s: String) = s.trim().trimStart('<').trimEnd('>').trim()

    /** `extract_msg_ids`: все `<id>` из значения (References). */
    fun extractMsgIds(value: String): List<String> {
        val out = mutableListOf<String>()
        val cur = StringBuilder()
        var depth = 0
        for (ch in value) {
            when (ch) {
                '<' -> {
                    depth++
                    cur.clear()
                }
                '>' -> if (depth > 0) {
                    depth--
                    val id = cur.toString().trim()
                    if (id.isNotEmpty()) out += id
                    cur.clear()
                }
                else -> if (depth > 0) cur.append(ch)
            }
        }
        return out
    }
}
