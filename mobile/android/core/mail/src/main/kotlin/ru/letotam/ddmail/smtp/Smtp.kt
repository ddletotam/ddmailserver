package ru.letotam.ddmail.smtp

import jakarta.mail.AuthenticationFailedException
import jakarta.mail.Message
import jakarta.mail.MessagingException
import jakarta.mail.SendFailedException
import jakarta.mail.Session
import jakarta.mail.internet.InternetAddress
import jakarta.mail.internet.MimeBodyPart
import jakarta.mail.internet.MimeMessage
import jakarta.mail.internet.MimeMultipart
import jakarta.mail.util.ByteArrayDataSource
import ru.letotam.ddmail.api.SendRequest
import ru.letotam.ddmail.imap.MailException
import ru.letotam.ddmail.imap.Security
import ru.letotam.ddmail.imap.smtpSecurity
import java.io.ByteArrayOutputStream
import java.io.IOException
import java.util.Date
import java.util.Properties
import java.util.concurrent.atomic.AtomicLong
import jakarta.activation.DataHandler

/** Исходящий сервер учётки. [auth] = false — релей без авторизации (по флагу). */
data class SmtpConfig(val host: String, val port: Int, val auth: Boolean, val user: String, val password: String)

/** Письмо, собранное для отправки, и его Message-ID (без `<>`). */
class Outgoing(val message: MimeMessage, val messageId: String, val recipients: List<InternetAddress>) {
    fun bytes(): ByteArray = ByteArrayOutputStream().also { message.writeTo(it) }.toByteArray()
}

/**
 * SMTP (контракт §5): 465 — неявный TLS, 587/25/2525 — STARTTLS, без него
 * не шлём; сертификат — системным хранилищем, имя хоста проверяется. AUTH —
 * PLAIN/LOGIN паролем, «без авторизации» — по флагу учётки.
 */
object Smtp {
    private val counter = AtomicLong(0)

    private fun session(cfg: SmtpConfig, timeoutMs: Int): Session {
        val p = Properties()
        p["mail.transport.protocol"] = "smtp"
        p["mail.smtp.host"] = cfg.host
        p["mail.smtp.port"] = cfg.port.toString()
        p["mail.smtp.auth"] = cfg.auth.toString()
        p["mail.smtp.connectiontimeout"] = timeoutMs.toString()
        p["mail.smtp.timeout"] = timeoutMs.toString()
        p["mail.smtp.writetimeout"] = timeoutMs.toString()
        p["mail.smtp.localhost"] = "[127.0.0.1]"
        p["mail.smtp.ssl.checkserveridentity"] = "true"
        p["mail.smtp.ssl.protocols"] = "TLSv1.2 TLSv1.3"
        p["mail.smtp.auth.mechanisms"] = "PLAIN LOGIN"
        if (smtpSecurity(cfg.port) == Security.Tls) {
            p["mail.smtp.ssl.enable"] = "true"
        } else {
            p["mail.smtp.starttls.enable"] = "true"
            p["mail.smtp.starttls.required"] = "true"
        }
        return Session.getInstance(p)
    }

    /** Проверка входа (форма подключения): соединиться, войти, выйти. */
    fun check(cfg: SmtpConfig, timeoutMs: Int = 25_000) = wrap {
        val t = session(cfg, timeoutMs).getTransport("smtp")
        try {
            if (cfg.auth) t.connect(cfg.host, cfg.port, cfg.user, cfg.password) else t.connect()
        } finally {
            runCatching { t.close() }
        }
    }

    fun send(cfg: SmtpConfig, out: Outgoing, timeoutMs: Int = 60_000) = wrap {
        val s = session(cfg, timeoutMs)
        val t = s.getTransport("smtp")
        try {
            if (cfg.auth) t.connect(cfg.host, cfg.port, cfg.user, cfg.password) else t.connect()
            t.sendMessage(out.message, out.recipients.toTypedArray())
        } finally {
            runCatching { t.close() }
        }
    }

    private fun wrap(block: () -> Unit) {
        try {
            block()
        } catch (e: AuthenticationFailedException) {
            throw MailException.Auth(e.message ?: "")
        } catch (e: SendFailedException) {
            throw MailException.Rejected("Сервер не принял письмо: ${e.message}")
        } catch (e: MessagingException) {
            val io = generateSequence<Throwable>(e) { it.cause }.firstOrNull { it is IOException } as? IOException
            if (io != null) throw ru.letotam.ddmail.imap.Tls.networkError(io)
            val msg = e.message.orEmpty()
            if (msg.contains("STARTTLS", ignoreCase = true)) throw MailException.Network("Сервер не поддерживает STARTTLS — открытым текстом не шлём")
            throw MailException.Rejected(msg.ifBlank { "Ошибка SMTP" })
        }
    }

    /** Message-ID как у десктопа: `<hex наносекунд>.<hex счётчика>@<домен from>`. */
    fun makeMessageId(fromEmail: String): String {
        val domain = fromEmail.substringAfterLast('@', "localhost").ifEmpty { "localhost" }
        val nanos = System.currentTimeMillis() * 1_000_000 + (System.nanoTime() % 1_000_000)
        return "${java.lang.Long.toHexString(nanos)}.${java.lang.Long.toHexString(counter.getAndIncrement())}@$domain"
    }

    /** Адрес с именем в RFC 2047 (а не сырым UTF-8 в заголовке). */
    private fun address(s: String): InternetAddress {
        val a = InternetAddress(s.trim(), false)
        val p = a.personal
        return if (p.isNullOrBlank()) InternetAddress(a.address) else InternetAddress(a.address, p, "UTF-8")
    }

    /**
     * Сборка как у десктопа (`smtp::build_body`): `alternative{text, html}`,
     * `related` со встроенными картинками, `mixed` с файлами. Bcc в
     * заголовки не пишется (его у запроса и нет).
     */
    fun build(req: SendRequest): Outgoing {
        val from = address(req.from)
        val id = makeMessageId(from.address.orEmpty())
        val msg = object : MimeMessage(ru.letotam.ddmail.mime.MimeParse.session) {
            override fun updateMessageID() {
                setHeader("Message-ID", "<$id>")
            }
        }
        msg.setFrom(from)
        val to = req.to.map(::address)
        val cc = req.cc.map(::address)
        msg.setRecipients(Message.RecipientType.TO, to.toTypedArray())
        if (cc.isNotEmpty()) msg.setRecipients(Message.RecipientType.CC, cc.toTypedArray())
        msg.setSubject(req.subject, "UTF-8")
        msg.sentDate = Date()
        if (req.inReplyTo.isNotBlank()) msg.setHeader("In-Reply-To", "<${req.inReplyTo.trim().trim('<', '>')}>")
        val refs = ru.letotam.ddmail.imap.Headers.extractMsgIds(req.references).ifEmpty {
            req.references.split(' ').map { it.trim('<', '>', ' ') }.filter { it.isNotEmpty() }
        }
        if (refs.isNotEmpty()) msg.setHeader("References", refs.joinToString(" ") { "<$it>" })

        val alt = MimeMultipart("alternative").apply {
            addBodyPart(MimeBodyPart().apply { setText(req.text, "UTF-8", "plain") })
            addBodyPart(MimeBodyPart().apply { setText(req.html, "UTF-8", "html") })
        }
        val (inline, files) = req.attachments.partition { !it.contentId.isNullOrEmpty() }
        val bodyPart: MimeMultipart = if (inline.isEmpty()) alt else MimeMultipart("related").apply {
            addBodyPart(MimeBodyPart().apply { setContent(alt) })
            inline.forEach { a ->
                addBodyPart(MimeBodyPart().apply {
                    dataHandler = DataHandler(ByteArrayDataSource(a.content, a.mimeType))
                    setHeader("Content-ID", "<${a.contentId!!.trim('<', '>')}>")
                    disposition = MimeBodyPart.INLINE
                    if (a.filename.isNotBlank()) setFileName(a.filename)
                })
            }
        }
        val root = if (files.isEmpty()) bodyPart else MimeMultipart("mixed").apply {
            addBodyPart(MimeBodyPart().apply { setContent(bodyPart) })
            files.forEach { a ->
                addBodyPart(MimeBodyPart().apply {
                    dataHandler = DataHandler(ByteArrayDataSource(a.content, a.mimeType.ifBlank { "application/octet-stream" }))
                    disposition = MimeBodyPart.ATTACHMENT
                    setFileName(a.filename)
                })
            }
        }
        msg.setContent(root)
        msg.saveChanges()
        return Outgoing(msg, id, to + cc)
    }
}
