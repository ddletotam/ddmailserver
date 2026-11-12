package ru.letotam.ddmail.imap

import java.io.BufferedInputStream
import java.io.ByteArrayOutputStream
import java.net.ServerSocket
import java.net.Socket
import kotlin.concurrent.thread

/**
 * Записанная сессия IMAP для тестов: сервер на localhost без TLS. [script] —
 * пары «команда без тега (начало) → ответ»; в ответе `TAG` заменяется тегом
 * команды. Непредусмотренная команда — `BAD`, и она попадает в [unexpected].
 * Литералы клиента (`{n+}`) читаются и отбрасываются.
 */
class FakeImapServer(
    private val capabilities: String,
    private val script: List<Pair<String, String>>,
) : AutoCloseable {
    private val server = ServerSocket(0)
    val commands = mutableListOf<String>()
    val unexpected = mutableListOf<String>()
    private var used = BooleanArray(script.size)

    init {
        thread(isDaemon = true) {
            runCatching { server.accept().use(::serve) }
        }
    }

    fun client(): ImapClient = ImapClient(ImapConnection.overSocket(Socket("127.0.0.1", server.localPort)))

    private fun serve(s: Socket) {
        val input = BufferedInputStream(s.getInputStream())
        val out = s.getOutputStream()
        fun send(text: String) {
            out.write(text.replace("\n", "\r\n").replace("\r\r\n", "\r\n").toByteArray(Charsets.UTF_8))
            out.flush()
        }
        send("* OK [CAPABILITY $capabilities] ready\n")
        while (true) {
            val line = readLine(input) ?: return
            val tag = line.substringBefore(' ')
            var cmd = line.substringAfter(' ')
            // Литерал клиента: прочитать и приклеить как есть.
            while (true) {
                val m = Regex("""\{(\d+)\+?}$""").find(cmd) ?: break
                val n = m.groupValues[1].toInt()
                val buf = ByteArray(n)
                var off = 0
                while (off < n) off += input.read(buf, off, n - off)
                cmd = cmd + String(buf, Charsets.UTF_8) + (readLine(input) ?: "")
            }
            synchronized(commands) { commands += cmd }
            if (cmd.uppercase() == "LOGOUT") {
                send("* BYE bye\n$tag OK LOGOUT done\n")
                return
            }
            val i = script.indices.firstOrNull { !used[it] && cmd.startsWith(script[it].first) }
            if (i == null) {
                synchronized(unexpected) { unexpected += cmd }
                send("$tag BAD unexpected\n")
                continue
            }
            used[i] = true
            send(script[i].second.replace("TAG", tag))
        }
    }

    private fun readLine(input: BufferedInputStream): String? {
        val b = ByteArrayOutputStream()
        while (true) {
            val c = input.read()
            if (c < 0) return null
            if (c == '\n'.code) break
            if (c != '\r'.code) b.write(c)
        }
        return String(b.toByteArray(), Charsets.UTF_8)
    }

    override fun close() {
        server.close()
    }
}
