package ru.letotam.ddmail.mime

import jakarta.mail.Multipart
import jakarta.mail.Part
import jakarta.mail.Session
import jakarta.mail.internet.ContentType
import jakarta.mail.internet.InternetAddress
import jakarta.mail.internet.MimeMessage
import jakarta.mail.internet.MimeUtility
import ru.letotam.ddmail.api.AttachmentDto
import ru.letotam.ddmail.api.MessageBody
import ru.letotam.ddmail.imap.Headers
import java.nio.ByteBuffer
import java.nio.charset.Charset
import java.nio.charset.CodingErrorAction
import java.util.Properties

/**
 * Разбор письма на устройстве в ту же модель тела, что отдаёт сервер ddmail
 * (`/conversations/messages`): html, text, вложения с индексами, части по
 * `cid:` — лента (`conversation-doc`) не знает, откуда письмо (контракт §3).
 *
 * Кодировки — всё, что встречается в RU-почте: UTF-8, windows-1251, koi8-r,
 * iso-8859-*, QP и base64, RFC 2047 и RFC 2231. Без charset у текстовой
 * части — UTF-8, если байты им являются, иначе windows-1251.
 *
 * Вложения — листья, не ставшие телом. Индекс — порядковый номер среди них
 * (по нему [attachment] находит лист повторно); прячутся, как у сервера
 * (`hideInlineAttachment`), картинки, на которые HTML ссылается по `cid:`, и
 * `text/calendar`.
 */
object MimeParse {
    val session: Session = Session.getInstance(
        Properties().apply {
            setProperty("mail.mime.allowutf8", "true")
            setProperty("mail.mime.decodefilename", "true")
            setProperty("mail.mime.decodeparameters", "true")
            setProperty("mail.mime.address.strict", "false")
            setProperty("mail.mime.parameters.strict", "false")
            setProperty("mail.mime.base64.ignoreerrors", "true")
            setProperty("mail.mime.multipart.allowempty", "true")
            setProperty("mail.mime.multipart.ignoremissingendboundary", "true")
        },
    )

    private class Leaf(val part: Part, val type: String, val filename: String?, val contentId: String?)

    private class Walk {
        var html: String? = null
        var text: String? = null
        val leaves = mutableListOf<Leaf>()
    }

    fun message(raw: ByteArray): MimeMessage = MimeMessage(session, raw.inputStream())

    /** Тело для ленты. [fallbackDateTs] — INTERNALDATE, если `Date` не разобралась. */
    fun body(raw: ByteArray, uid: Long, folder: String, ourAddrs: Collection<String>, fallbackDateTs: Long = 0): MessageBody {
        val msg = message(raw)
        val w = Walk()
        walk(msg, w)
        val html = w.html
        val atts = w.leaves.mapIndexedNotNull { i, l ->
            if (hidden(l, html)) return@mapIndexedNotNull null
            val size = runCatching { l.part.inputStream.use { it.readBytes().size.toLong() } }.getOrDefault(0)
            AttachmentDto(l.filename ?: defaultName(i, l.type), l.type, size, i)
        }
        fun header(name: String) = runCatching { msg.getHeader(name, null) }.getOrNull()?.let { MimeUtility.unfold(it) }
        val fromRaw = header("From")?.let(Headers::decodeText).orEmpty()
        val from = Headers.parseFromHeader(fromRaw)
        val fromAddr = from?.second.orEmpty()
        val ours = ourAddrs.map { it.lowercase() }.toSet()
        return MessageBody(
            uid = uid,
            folder = folder,
            subject = runCatching { msg.subject }.getOrNull().orEmpty(),
            from = from?.first.orEmpty(),
            fromAddr = fromAddr,
            to = addressList(header("To")),
            cc = addressList(header("Cc")),
            dateTs = MailDates.parse(header("Date")).takeIf { it > 0 } ?: fallbackDateTs,
            html = html,
            text = w.text,
            attachments = atts,
            isOutgoing = fromAddr in ours,
            messageId = Headers.normalizeMsgId(header("Message-ID").orEmpty()),
            inReplyTo = Headers.normalizeMsgId(header("In-Reply-To").orEmpty()),
            references = Headers.extractMsgIds(header("References").orEmpty()),
        )
    }

    /** Встроенная часть по `cid:` — байты и тип. */
    fun part(raw: ByteArray, cid: String): Pair<ByteArray, String>? {
        val want = normCid(cid)
        val w = Walk()
        walk(message(raw), w)
        val leaf = w.leaves.firstOrNull { it.contentId?.let(::normCid) == want } ?: return null
        return leaf.part.inputStream.use { it.readBytes() } to leaf.type
    }

    /** Вложение по индексу — тот же обход, что дал индекс в [body]. */
    fun attachment(raw: ByteArray, index: Int): Triple<ByteArray, String, String>? {
        val w = Walk()
        walk(message(raw), w)
        val leaf = w.leaves.getOrNull(index) ?: return null
        return Triple(leaf.part.inputStream.use { it.readBytes() }, leaf.type, leaf.filename ?: defaultName(index, leaf.type))
    }

    private fun defaultName(i: Int, type: String): String {
        val ext = when (type) {
            "image/png" -> ".png"
            "image/jpeg" -> ".jpg"
            "image/gif" -> ".gif"
            "application/pdf" -> ".pdf"
            "message/rfc822" -> ".eml"
            else -> ""
        }
        return "attachment_$i$ext"
    }

    private fun hidden(l: Leaf, html: String?): Boolean {
        if (l.type.startsWith("text/calendar") || l.type.startsWith("application/ics")) return true
        val cid = l.contentId?.let(::normCid) ?: return false
        return cid.isNotEmpty() && html?.contains(cid, ignoreCase = true) == true
    }

    private fun normCid(s: String) = s.trim().trimStart('<').trimEnd('>').trim().lowercase()

    private fun walk(p: Part, w: Walk) {
        val type = baseType(p)
        if (type.startsWith("multipart/")) {
            val mp = runCatching { p.content as? Multipart }.getOrNull()
            if (mp != null) {
                for (i in 0 until runCatching { mp.count }.getOrDefault(0)) {
                    runCatching { mp.getBodyPart(i) }.getOrNull()?.let { walk(it, w) }
                }
                return
            }
        }
        val disp = runCatching { p.disposition }.getOrNull()?.lowercase()
        val filename = runCatching { p.fileName }.getOrNull()?.let(Headers::decodeText)?.takeIf { it.isNotBlank() }
            ?: runCatching { ContentType(p.contentType).getParameter("name") }.getOrNull()?.let(Headers::decodeText)?.takeIf { it.isNotBlank() }
        val cid = runCatching { p.getHeader("Content-ID")?.firstOrNull() }.getOrNull()
        val asBody = disp != Part.ATTACHMENT && filename == null
        when {
            asBody && type == "text/html" && w.html == null -> w.html = text(p, html = true)
            asBody && type == "text/plain" && w.text == null -> w.text = text(p, html = false)
            else -> w.leaves += Leaf(p, type, filename, cid)
        }
    }

    private fun baseType(p: Part): String =
        runCatching { ContentType(p.contentType).baseType.lowercase() }.getOrDefault("text/plain")

    /** Текст части с её кодировкой (с запасными путями для битых и без charset). */
    fun text(p: Part, html: Boolean): String {
        val bytes = runCatching { p.inputStream.use { it.readBytes() } }.getOrElse { return "" }
        val cs = runCatching { ContentType(p.contentType).getParameter("charset") }.getOrNull()
        return decodeBytes(bytes, cs, html)
    }

    fun decodeBytes(bytes: ByteArray, charset: String?, html: Boolean = false): String {
        val declared = charset?.trim()?.trim('"')?.takeIf { it.isNotEmpty() }
            ?: if (html) metaCharset(bytes) else null
        if (declared != null) {
            val c = runCatching { Charset.forName(MimeUtility.javaCharset(declared)) }.getOrNull()
            // us-ascii с 8-битными байтами внутри — на деле UTF-8 или 1251.
            if (c != null && !(c.name() == "US-ASCII" && bytes.any { it < 0 })) return String(bytes, c)
        }
        return if (validUtf8(bytes)) String(bytes, Charsets.UTF_8) else String(bytes, Charset.forName("windows-1251"))
    }

    private fun metaCharset(bytes: ByteArray): String? {
        val head = String(bytes, 0, minOf(bytes.size, 2048), Charsets.ISO_8859_1)
        return Regex("""<meta[^>]+charset\s*=\s*["']?([A-Za-z0-9_\-]+)""", RegexOption.IGNORE_CASE).find(head)?.groupValues?.get(1)
    }

    private fun validUtf8(b: ByteArray): Boolean = try {
        Charsets.UTF_8.newDecoder()
            .onMalformedInput(CodingErrorAction.REPORT)
            .onUnmappableCharacter(CodingErrorAction.REPORT)
            .decode(ByteBuffer.wrap(b))
        true
    } catch (e: Exception) {
        false
    }

    /** Адреса заголовка строками «Имя <адрес>» — как отдаёт сервер. */
    private fun addressList(value: String?): List<String> {
        if (value.isNullOrBlank()) return emptyList()
        val parsed = runCatching { InternetAddress.parseHeader(value, false) }.getOrNull()
        if (parsed != null) {
            return parsed.map { a ->
                val p = a.personal
                if (p.isNullOrBlank()) a.address.orEmpty() else "$p <${a.address}>"
            }.filter { it.isNotBlank() }
        }
        return Headers.decodeText(value).split(',').map { it.trim() }.filter { it.isNotEmpty() }
    }
}
