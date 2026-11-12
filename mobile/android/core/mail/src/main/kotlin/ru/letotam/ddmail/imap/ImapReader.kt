package ru.letotam.ddmail.imap

import java.io.EOFException
import java.io.InputStream

/** Значение из ответа IMAP (RFC 3501 §4): атом, строка/литерал, NIL, список. */
sealed class IVal {
    /** Атом или число; секция `BODY[HEADER.FIELDS (…)]` — тоже атом целиком. */
    data class Atom(val s: String) : IVal() {
        override fun toString() = s
    }

    /** Строка в кавычках или литерал `{n}` — байты как есть. */
    class Str(val data: ByteArray) : IVal() {
        val text: String get() = String(data, Charsets.UTF_8)
        override fun toString() = "\"" + text + "\""
    }

    data object Nil : IVal()

    data class Lst(val items: List<IVal>) : IVal() {
        override fun toString() = items.joinToString(" ", "(", ")")
    }

    /** Текст атома или строки; NIL и список — `null`. */
    val str: String?
        get() = when (this) {
            is Atom -> s
            is Str -> text
            else -> null
        }

    val bytes: ByteArray?
        get() = when (this) {
            is Str -> data
            is Atom -> s.toByteArray(Charsets.UTF_8)
            else -> null
        }

    val list: List<IVal> get() = (this as? Lst)?.items ?: emptyList()
    val long: Long? get() = (this as? Atom)?.s?.toLongOrNull()
}

/** Ответ сервера целиком (с литералами внутри). */
sealed class Resp {
    /** `+ текст` — сервер ждёт продолжения команды. */
    data class Continue(val text: String) : Resp()

    /**
     * Статус: тегированный (`a1 OK …`) или нетегированный (`* OK`, `* NO`,
     * `* BYE`, `* PREAUTH`). [code] — содержимое `[…]` токенами.
     */
    data class Status(val tag: String?, val status: String, val code: List<IVal>, val text: String) : Resp() {
        val ok get() = status == "OK"
        val codeName: String? get() = code.firstOrNull()?.str?.uppercase()
    }

    /** Прочие нетегированные: `* 3 EXISTS`, `* 5 FETCH (…)`, `* LIST …`. */
    data class Data(val items: List<IVal>) : Resp() {
        /** `* CAPABILITY …` → "CAPABILITY"; `* 5 FETCH` → "FETCH". */
        val kind: String
            get() {
                val a = items.getOrNull(0)?.str?.uppercase() ?: return ""
                return if (a.toLongOrNull() != null) items.getOrNull(1)?.str?.uppercase().orEmpty() else a
            }
        val number: Long? get() = items.getOrNull(0)?.long
    }
}

/**
 * Разбор ответов IMAP из потока. Литералы (`{n}`, `~{n}`) читаются байтами;
 * строка кончается на CRLF (терпим и голый LF). Атом с `[` читается до
 * парной `]` вместе с пробелами — так `BODY[HEADER.FIELDS (FROM TO)]`
 * остаётся одним ключом.
 */
class ImapReader(private val input: InputStream) {
    private var peeked = -2

    private fun peek(): Int {
        if (peeked == -2) peeked = input.read()
        return peeked
    }

    private fun next(): Int {
        val c = peek()
        peeked = -2
        if (c < 0) throw EOFException("сервер закрыл соединение")
        consumed++
        return c
    }

    /**
     * Есть ли начало ответа (ждёт с таймаутом сокета). Таймаут здесь не
     * портит разбор: ни один байт ответа ещё не прочитан.
     */
    fun awaitData(): Boolean = try {
        peek() >= 0 || true
    } catch (e: java.net.SocketTimeoutException) {
        false
    }

    fun read(): Resp {
        val first = next()
        if (first == '+'.code) {
            if (peek() == ' '.code) next()
            return Resp.Continue(restOfLine())
        }
        val tag = if (first == '*'.code) {
            expect(' ')
            null
        } else {
            val sb = StringBuilder().append(first.toChar())
            while (peek() != ' '.code && peek() != '\r'.code && peek() != '\n'.code) sb.append(next().toChar())
            if (peek() == ' '.code) next()
            sb.toString()
        }
        // Статус: OK/NO/BAD/BYE/PREAUTH — дальше код в [] и свободный текст.
        val word = readWordPeek()
        if (word != null && word.uppercase() in STATUS) {
            val status = word.uppercase()
            if (peek() == ' '.code) next()
            var code = emptyList<IVal>()
            if (peek() == '['.code) {
                next()
                code = readUntilBracket()
                if (peek() == ' '.code) next()
            }
            return Resp.Status(tag, status, code, restOfLine())
        }
        val items = mutableListOf<IVal>()
        if (word != null) {
            items += IVal.Atom(word)
            if (peek() == ' '.code) next()
        }
        while (true) {
            skipSpaces()
            val c = peek()
            if (c == '\r'.code || c == '\n'.code || c < 0) {
                eol()
                break
            }
            items += progress { value() }
        }
        return if (tag == null) Resp.Data(items) else Resp.Status(tag, "", emptyList(), items.joinToString(" "))
    }

    /** Первое слово (атом без скобок) — для распознавания статуса. */
    private fun readWordPeek(): String? {
        val c = peek()
        if (c < 0 || c == '('.code || c == '"'.code || c == '{'.code || c == '\r'.code || c == '\n'.code) return null
        val sb = StringBuilder()
        while (true) {
            val d = peek()
            if (d == ' '.code || d == '\r'.code || d == '\n'.code || d < 0 || d == '['.code || d == '('.code) break
            sb.append(next().toChar())
        }
        return sb.toString()
    }

    private fun readUntilBracket(): List<IVal> {
        val items = mutableListOf<IVal>()
        while (true) {
            skipSpaces()
            val c = peek()
            if (c == ']'.code) {
                next()
                return items
            }
            if (c == '\r'.code || c == '\n'.code || c < 0) return items
            items += progress { value(stopAtBracket = true) }
        }
    }

    private fun skipSpaces() {
        while (peek() == ' '.code) next()
    }

    private fun value(stopAtBracket: Boolean = false): IVal {
        val c = peek()
        return when {
            c == '('.code -> {
                next()
                val items = mutableListOf<IVal>()
                while (true) {
                    skipSpaces()
                    val d = peek()
                    if (d == ')'.code) {
                        next()
                        break
                    }
                    if (d == '\r'.code || d == '\n'.code || d < 0) break
                    items += progress { value(stopAtBracket) }
                }
                IVal.Lst(items)
            }
            c == '"'.code -> quoted()
            c == '{'.code -> literal()
            c == '~'.code -> {
                next()
                if (peek() == '{'.code) literal() else IVal.Atom(atom("~", stopAtBracket))
            }
            else -> {
                val a = atom("", stopAtBracket)
                if (a.equals("NIL", ignoreCase = true)) IVal.Nil else IVal.Atom(a)
            }
        }
    }

    private fun atom(prefix: String, stopAtBracket: Boolean): String {
        val sb = StringBuilder(prefix)
        var depth = 0
        while (true) {
            val c = peek()
            if (c < 0 || c == '\r'.code || c == '\n'.code) break
            if (depth == 0 && (c == ' '.code || c == '('.code || c == ')'.code)) break
            if (depth == 0 && stopAtBracket && c == ']'.code) break
            if (c == '['.code) depth++
            if (c == ']'.code && depth > 0) depth--
            sb.append(next().toChar())
        }
        return sb.toString()
    }

    private fun quoted(): IVal.Str {
        next()
        val out = java.io.ByteArrayOutputStream()
        while (true) {
            var c = next()
            if (c == '"'.code) break
            if (c == '\\'.code) c = next()
            out.write(c)
        }
        return IVal.Str(out.toByteArray())
    }

    private fun literal(): IVal.Str {
        next()
        val sb = StringBuilder()
        while (true) {
            val c = next()
            if (c == '}'.code) break
            if (c != '+'.code) sb.append(c.toChar())
        }
        val n = sb.toString().toIntOrNull()?.takeIf { it in 0..MAX_LITERAL }
            ?: throw ru.letotam.ddmail.imap.MailException.Network("IMAP: литерал недопустимого размера")
        eol()
        val buf = ByteArray(n)
        var off = 0
        if (peeked >= 0 && n > 0) {
            buf[0] = peeked.toByte()
            peeked = -2
            off = 1
        }
        while (off < n) {
            val r = input.read(buf, off, n - off)
            if (r < 0) throw EOFException("литерал оборван")
            off += r
        }
        consumed += n
        return IVal.Str(buf)
    }

    private fun eol() {
        if (peek() == '\r'.code) next()
        if (peek() == '\n'.code) next()
    }

    private fun expect(ch: Char) {
        if (peek() == ch.code) next()
    }

    private fun restOfLine(): String {
        val out = java.io.ByteArrayOutputStream()
        while (true) {
            val c = peek()
            if (c < 0) break
            if (c == '\r'.code || c == '\n'.code) {
                eol()
                break
            }
            out.write(next())
        }
        return String(out.toByteArray(), Charsets.UTF_8)
    }

    /** Значение, не сдвинувшее разбор (лишняя `)` или `]`), — битый ответ, а не вечный цикл. */
    private var consumed = 0L

    private inline fun progress(read: () -> IVal): IVal {
        val before = consumed
        val v = read()
        if (consumed == before) throw MailException.Network("IMAP: ошибка разбора ответа")
        return v
    }

    private companion object {
        val STATUS = setOf("OK", "NO", "BAD", "BYE", "PREAUTH")

        /** Литерал больше — не письмо, а атака или сбой: 64 МБ. */
        const val MAX_LITERAL = 64 shl 20
    }
}
