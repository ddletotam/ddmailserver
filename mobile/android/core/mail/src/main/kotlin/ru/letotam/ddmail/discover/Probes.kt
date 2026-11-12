package ru.letotam.ddmail.discover

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.runInterruptible
import ru.letotam.ddmail.imap.Security
import ru.letotam.ddmail.imap.Tls
import ru.letotam.ddmail.imap.imapSecurity
import ru.letotam.ddmail.imap.smtpSecurity
import java.io.BufferedReader
import java.io.InputStreamReader
import java.io.OutputStream
import java.net.Socket

/**
 * Проверки портов для автонастройки (`probe_imap`/`probe_smtp` десктопа):
 * живой сервер с рабочим TLS, без входа. TCP — [Discovery.CONNECT_MS],
 * ответ — [Discovery.PROBE_MS].
 */
object Probes {
    suspend fun imap(host: String, port: Int): Boolean = runInterruptible(Dispatchers.IO) {
        runCatching { imapBlocking(host, port) }.getOrDefault(false)
    }

    suspend fun smtp(host: String, port: Int): Boolean? = runInterruptible(Dispatchers.IO) {
        runCatching { smtpBlocking(host, port) }.getOrNull()
    }

    private fun open(host: String, port: Int): Socket =
        Tls.tcp(host, port, Discovery.CONNECT_MS).apply { soTimeout = Discovery.PROBE_MS.toInt() }

    private fun send(o: OutputStream, s: String) {
        o.write(s.toByteArray())
        o.flush()
    }

    internal fun imapGreetingOk(line: String): Boolean {
        val u = line.uppercase()
        return u.startsWith("* OK") || u.startsWith("* PREAUTH")
    }

    private fun taggedOk(r: BufferedReader, tag: String): Boolean {
        while (true) {
            val line = r.readLine() ?: return false
            if (line.startsWith("$tag ")) return line.substring(tag.length + 1).uppercase().startsWith("OK")
        }
    }

    private fun imapBlocking(host: String, port: Int): Boolean {
        val plain = open(host, port)
        plain.use {
            val s = if (imapSecurity(port) == Security.StartTls) {
                val r = BufferedReader(InputStreamReader(plain.getInputStream(), Charsets.ISO_8859_1))
                if (!imapGreetingOk(r.readLine() ?: return false)) return false
                send(plain.getOutputStream(), "a1 STARTTLS\r\n")
                if (!taggedOk(r, "a1")) return false
                val tls = Tls.wrap(plain, host, port)
                val r2 = BufferedReader(InputStreamReader(tls.getInputStream(), Charsets.ISO_8859_1))
                send(tls.getOutputStream(), "a2 NOOP\r\n")
                if (!taggedOk(r2, "a2")) return false
                tls
            } else {
                val tls = Tls.wrap(plain, host, port)
                val r = BufferedReader(InputStreamReader(tls.getInputStream(), Charsets.ISO_8859_1))
                if (!imapGreetingOk(r.readLine() ?: return false)) return false
                tls
            }
            runCatching { send(s.getOutputStream(), "a9 LOGOUT\r\n") }
            s.close()
            return true
        }
    }

    /** Ответ SMTP целиком (многострочный `250-…`): код и тексты строк. */
    internal fun smtpReply(r: BufferedReader): Pair<Int, List<String>>? {
        val lines = mutableListOf<String>()
        while (true) {
            val line = r.readLine() ?: return null
            if (line.length < 3) return null
            val code = line.substring(0, 3).toIntOrNull() ?: return null
            lines += if (line.length > 4) line.substring(4).trim() else ""
            if (line.length < 4 || line[3] != '-') return code to lines
        }
    }

    internal fun smtpHasAuth(caps: List<String>) = caps.any { it.uppercase().startsWith("AUTH") }

    private fun smtpBlocking(host: String, port: Int): Boolean? {
        val plain = open(host, port)
        plain.use {
            val tls = if (smtpSecurity(port) == Security.StartTls) {
                val r = BufferedReader(InputStreamReader(plain.getInputStream(), Charsets.ISO_8859_1))
                if (smtpReply(r)?.first != 220) return null
                send(plain.getOutputStream(), "EHLO localhost\r\n")
                val (code, caps) = smtpReply(r) ?: return null
                if (code != 250 || caps.none { it.equals("STARTTLS", ignoreCase = true) }) return null
                send(plain.getOutputStream(), "STARTTLS\r\n")
                if (smtpReply(r)?.first != 220) return null
                Tls.wrap(plain, host, port)
            } else {
                val t = Tls.wrap(plain, host, port)
                val r = BufferedReader(InputStreamReader(t.getInputStream(), Charsets.ISO_8859_1))
                if (smtpReply(r)?.first != 220) return null
                // Читатель с буфером дальше не нужен: следующий ответ читаем новым поверх того же потока.
                return ehloQuit(t, r)
            }
            val r = BufferedReader(InputStreamReader(tls.getInputStream(), Charsets.ISO_8859_1))
            return ehloQuit(tls, r)
        }
    }

    private fun ehloQuit(s: Socket, r: BufferedReader): Boolean? {
        send(s.getOutputStream(), "EHLO localhost\r\n")
        val (code, caps) = smtpReply(r) ?: return null
        if (code != 250) return null
        runCatching { send(s.getOutputStream(), "QUIT\r\n") }
        s.close()
        return smtpHasAuth(caps)
    }
}
