package ru.letotam.ddmail.imap

import java.io.BufferedInputStream
import java.io.BufferedOutputStream
import java.io.Closeable
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import java.net.Socket
import java.net.SocketTimeoutException

/** Аргумент команды: готовый текст (атомы, множества UID) или строка/литерал. */
sealed class Arg {
    class Raw(val s: String) : Arg()

    /** Строка: в кавычках, если можно, иначе литералом. */
    class Str(val s: String) : Arg()

    class Literal(val bytes: ByteArray) : Arg()
}

/**
 * Одно соединение IMAP: команды с тегами, литералы с продолжением (или
 * LITERAL+), нетегированные ответы — обработчику команды. Блокирующее:
 * вызывается с Dispatchers.IO. Не потокобезопасно, кроме [stopIdle].
 */
class ImapConnection internal constructor(
    private val input: InputStream,
    private val output: OutputStream,
    private val socket: Socket?,
) : Closeable {
    private val reader = ImapReader(BufferedInputStream(input, 64 * 1024))
    private val out = BufferedOutputStream(output, 16 * 1024)
    private var seq = 0
    var capabilities: Set<String> = emptySet()
        private set

    /** Нетегированные ответы вне команд (EXISTS посреди IDLE и т.п.). */
    var greeting: Resp.Status? = null
        private set

    fun has(cap: String) = cap.uppercase() in capabilities

    internal fun readGreeting() {
        val r = read()
        val st = r as? Resp.Status ?: throw MailException.Network("IMAP: неожиданное приветствие")
        if (st.status == "BYE" || st.status == "NO" || st.status == "BAD") throw MailException.Network("IMAP: ${st.text}")
        greeting = st
        takeCapabilityCode(st)
    }

    private fun takeCapabilityCode(st: Resp.Status) {
        if (st.codeName == "CAPABILITY") capabilities = st.code.drop(1).mapNotNull { it.str?.uppercase() }.toSet()
    }

    private fun read(): Resp = try {
        reader.read()
    } catch (e: IOException) {
        throw Tls.networkError(e)
    }

    /**
     * Выполнить команду; [onData] получает нетегированные ответы. Тегированный
     * NO/BAD — [MailException.Rejected] с текстом сервера; BYE — обрыв.
     */
    fun command(vararg args: Arg, onData: (Resp.Data) -> Unit = {}): Resp.Status {
        val tag = "A" + (++seq).toString().padStart(3, '0')
        try {
            out.write(tag.toByteArray())
            for (a in args) {
                out.write(' '.code)
                when (a) {
                    is Arg.Raw -> out.write(a.s.toByteArray(Charsets.UTF_8))
                    is Arg.Str -> {
                        val b = a.s.toByteArray(Charsets.UTF_8)
                        if (quotable(a.s)) out.write(quote(a.s).toByteArray(Charsets.UTF_8)) else literal(b, onData)
                    }
                    is Arg.Literal -> literal(a.bytes, onData)
                }
            }
            out.write(CRLF)
            out.flush()
        } catch (e: IOException) {
            throw Tls.networkError(e)
        }
        return awaitTagged(tag, onData)
    }

    private fun literal(b: ByteArray, onData: (Resp.Data) -> Unit) {
        if (has("LITERAL+")) {
            out.write("{${b.size}+}".toByteArray())
            out.write(CRLF)
        } else {
            out.write("{${b.size}}".toByteArray())
            out.write(CRLF)
            out.flush()
            while (true) {
                when (val r = read()) {
                    is Resp.Continue -> break
                    is Resp.Data -> onData(r)
                    is Resp.Status -> {
                        if (r.tag != null) throw MailException.Rejected(r.text.ifBlank { "команда отвергнута" })
                        if (r.status == "BYE") throw MailException.Network("IMAP: сервер закрыл соединение")
                    }
                }
            }
        }
        out.write(b)
    }

    private fun awaitTagged(tag: String, onData: (Resp.Data) -> Unit): Resp.Status {
        while (true) {
            when (val r = read()) {
                is Resp.Continue -> {}
                is Resp.Data -> onData(r)
                is Resp.Status -> {
                    if (r.tag == null) {
                        if (r.status == "BYE") {
                            // После BYE будет и тегированный ответ (LOGOUT) — или обрыв.
                            if (!r.text.contains("LOGOUT", ignoreCase = true)) lastBye = r
                        }
                        takeCapabilityCode(r)
                        untaggedStatus?.invoke(r)
                        continue
                    }
                    if (r.tag != tag) continue
                    takeCapabilityCode(r)
                    if (r.status != "OK") {
                        val e = MailException.Rejected(r.text.ifBlank { r.status })
                        lastRejectCode = r.codeName
                        throw e
                    }
                    return r
                }
            }
        }
    }

    /** Код последнего отказа (`AUTHENTICATIONFAILED` и т.п.). */
    var lastRejectCode: String? = null
        private set
    var lastBye: Resp.Status? = null
        private set

    /** Нетегированные OK/NO с кодами (`[UIDVALIDITY]` и т.п.) — на время команды. */
    var untaggedStatus: ((Resp.Status) -> Unit)? = null

    fun refreshCapabilities() {
        command(Arg.Raw("CAPABILITY")) { d ->
            if (d.kind == "CAPABILITY") capabilities = d.items.drop(1).mapNotNull { it.str?.uppercase() }.toSet()
        }
    }

    // ── IDLE ──

    @Volatile private var idleStop = false

    /** Остановить IDLE из другого потока: ответ придёт в течение [IDLE_POLL_MS]. */
    fun stopIdle() {
        idleStop = true
    }

    /**
     * IDLE (RFC 2177) не дольше [maxMs]: [onData] на каждое событие, `true` —
     * хватит ждать. Чтение опрашивается раз в [IDLE_POLL_MS], чтобы
     * [stopIdle] срабатывал без закрытия сокета.
     */
    fun idle(maxMs: Long, onData: (Resp.Data) -> Boolean) {
        idleStop = false
        val tag = "A" + (++seq).toString().padStart(3, '0')
        try {
            out.write("$tag IDLE".toByteArray())
            out.write(CRLF)
            out.flush()
        } catch (e: IOException) {
            throw Tls.networkError(e)
        }
        // Ждём «+ idling».
        while (true) {
            when (val r = read()) {
                is Resp.Continue -> break
                is Resp.Data -> if (onData(r)) idleStop = true
                is Resp.Status -> if (r.tag == tag) {
                    if (r.status == "OK") return
                    throw MailException.Rejected(r.text)
                } else if (r.status == "BYE") throw MailException.Network("IMAP: сервер закрыл соединение")
            }
        }
        val prevTimeout = socket?.soTimeout ?: 0
        val deadline = System.currentTimeMillis() + maxMs
        try {
            socket?.soTimeout = IDLE_POLL_MS
            while (!idleStop && System.currentTimeMillis() < deadline) {
                val r = try {
                    reader.read()
                } catch (e: SocketTimeoutException) {
                    continue
                } catch (e: IOException) {
                    throw Tls.networkError(e)
                }
                when (r) {
                    is Resp.Data -> if (onData(r)) idleStop = true
                    is Resp.Status -> if (r.status == "BYE") throw MailException.Network("IMAP: сервер закрыл соединение")
                    else -> {}
                }
            }
        } finally {
            socket?.soTimeout = prevTimeout
        }
        try {
            out.write("DONE".toByteArray())
            out.write(CRLF)
            out.flush()
        } catch (e: IOException) {
            throw Tls.networkError(e)
        }
        awaitTagged(tag) { onData(it) }
    }

    override fun close() {
        runCatching { socket?.close() ?: run { input.close(); output.close() } }
    }

    companion object {
        private val CRLF = byteArrayOf('\r'.code.toByte(), '\n'.code.toByte())
        const val IDLE_POLL_MS = 3_000

        /** Строку можно передать в кавычках: ASCII без CR/LF (кавычки и \ экранируются). */
        internal fun quotable(s: String) = s.length < 1000 && s.all { it.code in 0x20..0x7e }

        internal fun quote(s: String) = "\"" + s.replace("\\", "\\\\").replace("\"", "\\\"") + "\""

        /**
         * Открыть соединение: 993 и прочие — TLS сразу, 143 — STARTTLS
         * (обязателен: сервер без него — ошибка, открытым текстом не ходим).
         */
        fun open(host: String, port: Int, timeoutMs: Int = 25_000): ImapConnection {
            val plain = Tls.tcp(host, port, timeoutMs)
            plain.soTimeout = timeoutMs
            if (imapSecurity(port) == Security.Tls) {
                val s = Tls.wrap(plain, host, port)
                s.soTimeout = timeoutMs
                return ImapConnection(s.getInputStream(), s.getOutputStream(), s).also { it.start() }
            }
            val pre = ImapConnection(plain.getInputStream(), plain.getOutputStream(), plain)
            try {
                pre.readGreeting()
                pre.command(Arg.Raw("STARTTLS"))
            } catch (e: MailException.Rejected) {
                pre.close()
                throw MailException.Network("Сервер не поддерживает STARTTLS — открытым текстом не подключаемся")
            } catch (e: MailException) {
                pre.close()
                throw e
            }
            val s = Tls.wrap(plain, host, port)
            s.soTimeout = timeoutMs
            val c = ImapConnection(s.getInputStream(), s.getOutputStream(), s)
            // После STARTTLS приветствия нет; возможности — заново.
            try {
                c.refreshCapabilities()
            } catch (e: MailException) {
                c.close()
                throw e
            }
            return c
        }

        /** Только для тестов: без TLS, поверх готового сокета. */
        internal fun overSocket(s: Socket): ImapConnection =
            ImapConnection(s.getInputStream(), s.getOutputStream(), s).also { it.start() }
    }

    private fun start() {
        try {
            readGreeting()
            if (capabilities.isEmpty()) refreshCapabilities()
        } catch (e: MailException) {
            close()
            throw e
        }
    }
}
