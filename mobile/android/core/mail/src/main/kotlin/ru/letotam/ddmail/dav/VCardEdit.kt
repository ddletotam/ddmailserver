package ru.letotam.ddmail.dav

import java.time.Instant
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter

/**
 * Правка vCard по строкам — порт `internal/carddav/vcardedit` сервера
 * (контракт Android §15): меняются только свойства, которые правит форма
 * контакта (FN/N, EMAIL, TEL, ORG, TITLE и REV), всё остальное — фото, дни
 * рождения, адреса, заметки, X-свойства, группы `item1.…` с подписями Apple,
 * свёртка строк — остаётся байт в байт.
 *
 * Пересобирать карточку из полей формы ([VCard.build]) нельзя: у контакта
 * iCloud или Google так пропадало всё, чего форма не показывает.
 *
 * Значения сравниваются так, как их видит Android ([VCard.parse]): полное
 * снятие экранирования; у ORG форма видит только первый компонент —
 * остальные (отдел) правка не трогает.
 */
object VCardEdit {
    /** Правка: `null` — поле не трогать; совпавшее с карточкой значение тоже ничего не меняет. */
    data class Patch(
        val fullName: String? = null,
        val emails: List<String>? = null,
        val phones: List<String>? = null,
        val organization: String? = null,
        val title: String? = null,
    )

    /** Итог: карточка и имена изменившихся полей (пусто — карточка та же). */
    data class Result(val data: String, val changed: List<String>)

    class NoVCard : Exception("в данных нет карточки vCard")

    private val REV = DateTimeFormatter.ofPattern("yyyyMMdd'T'HHmmss'Z'").withZone(ZoneOffset.UTC)

    fun apply(data: String, p: Patch, now: Instant = Instant.now()): Result {
        val c = Card.parse(data) ?: throw NoVCard()
        val changed = mutableListOf<String>()
        p.fullName?.trim()?.let { want -> if (want.isNotEmpty() && c.setFullName(want)) changed += "full_name" }
        p.emails?.let { if (c.setList("EMAIL", it)) changed += "emails" }
        p.phones?.let { if (c.setList("TEL", it)) changed += "phones" }
        p.organization?.let { if (c.setOrg(it.trim())) changed += "organization" }
        p.title?.let { if (c.setSingle("TITLE", it.trim())) changed += "title" }
        if (changed.isEmpty()) return Result(data, emptyList())
        for (l in c.props("REV")) l.repl = listOf(l.prefix + ":" + REV.format(now))
        return Result(c.render(), changed)
    }

    /** Логическая строка: физические строки (с продолжениями) и разбор. */
    private class Line(val raw: MutableList<String>) {
        var group = ""
        var name = ""
        var prefix = ""
        var value = ""
        var drop = false
        var repl: List<String>? = null
    }

    private class Card(val lines: List<Line>, val eol: String, val begin: Int, val end: Int) {
        /** Новые строки — после строки с индексом `at`. */
        val adds = mutableListOf<Pair<Int, List<String>>>()

        companion object {
            fun parse(data: String): Card? {
                val eol = if (data.contains("\r\n")) "\r\n" else "\n"
                val lines = mutableListOf<Line>()
                for (p in data.replace("\r\n", "\n").split('\n')) {
                    if ((p.startsWith(" ") || p.startsWith("\t")) && lines.isNotEmpty()) {
                        lines.last().raw += p
                        continue
                    }
                    lines += Line(mutableListOf(p))
                }
                for (l in lines) {
                    val unfolded = l.raw[0] + l.raw.drop(1).joinToString("") { it.substring(1) }
                    val (prefix, value) = splitValue(unfolded)
                    l.prefix = prefix
                    l.value = value
                    var head = prefix.substringBefore(';')
                    val dot = head.indexOf('.')
                    if (dot >= 0) {
                        l.group = head.substring(0, dot).lowercase()
                        head = head.substring(dot + 1)
                    }
                    l.name = head.trim().uppercase()
                }
                var depth = 0
                var begin = -1
                for ((i, l) in lines.withIndex()) {
                    val vcard = l.value.trim().equals("VCARD", ignoreCase = true)
                    if (l.name == "BEGIN" && vcard) {
                        depth++
                        if (begin < 0) begin = i
                    } else if (l.name == "END" && vcard && depth > 0) {
                        depth--
                        if (depth == 0 && begin >= 0) return Card(lines, eol, begin, i)
                    }
                }
                return null
            }

            /** Имя с параметрами и значение — по первому двоеточию вне кавычек. */
            fun splitValue(s: String): Pair<String, String> {
                var quoted = false
                for ((i, ch) in s.withIndex()) {
                    if (ch == '"') quoted = !quoted
                    if (ch == ':' && !quoted) return s.substring(0, i) to s.substring(i + 1)
                }
                return s to ""
            }
        }

        /** Строки свойства самой карточки (не вложенных), ещё не удалённые. */
        fun props(name: String): List<Line> {
            val out = mutableListOf<Line>()
            var depth = 0
            for (i in begin..end) {
                val l = lines[i]
                val vcard = l.value.trim().equals("VCARD", ignoreCase = true)
                if (l.name == "BEGIN" && vcard) { depth++; continue }
                if (l.name == "END" && vcard) { depth--; continue }
                if (depth == 1 && l.name == name && !l.drop) out += l
            }
            return out
        }

        /** После какой строки добавлять свойство: за последним таким же (и его подписью группы), иначе перед END. */
        fun insertAfter(name: String): Int {
            val last = props(name).lastOrNull() ?: return end - 1
            var i = lines.indexOf(last)
            while (last.group.isNotEmpty() && i + 1 < end && lines[i + 1].group == last.group) i++
            return i
        }

        fun afterVersion(): Int = (begin..end).firstOrNull { lines[it].name == "VERSION" } ?: begin

        fun render(): String {
            val out = mutableListOf<String>()
            for ((i, l) in lines.withIndex()) {
                when {
                    l.drop -> {}
                    l.repl != null -> out += l.repl!!
                    else -> out += l.raw
                }
                adds.filter { it.first == i }.forEach { out += it.second }
            }
            return out.joinToString(eol)
        }

        fun setFullName(want: String): Boolean {
            val fns = props("FN")
            val old = fns.firstOrNull()?.let { clientValue(it.value) }.orEmpty()
            if (old == want) return false
            if (fns.isNotEmpty()) replaceValue(fns[0], escapeText(want))
            else adds += afterVersion() to fold("FN:" + escapeText(want))

            val n = props("N").firstOrNull()
            if (n == null) {
                // N обязателен в vCard 3.0; у карточки без него — из нового имени.
                val (given, family) = splitName(want, false)
                adds += afterVersion() to fold("N:" + escapeComponent(family) + ";" + escapeComponent(given) + ";;;")
                return true
            }
            val parts = splitComponents(n.value).toMutableList()
            while (parts.size < 5) parts += ""
            val oldFamily = Ical.unescape(parts[0])
            val oldGiven = Ical.unescape(parts[1])
            // Пустой N — карточка организации или сознательно без имени: фамилию не выдумываем.
            if (oldFamily.isBlank() && oldGiven.isBlank()) return true
            val familyFirst = old.isNotEmpty() && old == "$oldFamily $oldGiven".trim() && old != "$oldGiven $oldFamily".trim()
            val (given, family) = splitName(want, familyFirst)
            // Отчество (компонент 2) вошло в новое FN — убираем; приставку и суффикс оставляем.
            parts[0] = escapeComponent(family)
            parts[1] = escapeComponent(given)
            parts[2] = ""
            replaceValue(n, parts.joinToString(";"))
            return true
        }

        /** Свойство с одним значением; пустое значение убирает его целиком. */
        fun setSingle(name: String, want: String): Boolean {
            val ps = props(name)
            val old = ps.firstOrNull()?.let { clientValue(it.value) }.orEmpty()
            if (old == want) return false
            when {
                want.isEmpty() -> ps.forEach(::dropWithLabels)
                ps.isNotEmpty() -> replaceValue(ps[0], escapeText(want))
                else -> adds += insertAfter(name) to fold("$name:" + escapeText(want))
            }
            return true
        }

        /** ORG: форма видит первый компонент (организацию) — меняется только он, отдел остаётся. */
        fun setOrg(want: String): Boolean {
            val ps = props("ORG")
            val first = ps.firstOrNull()
            val parts = first?.let { splitComponents(it.value) }.orEmpty()
            val old = parts.firstOrNull()?.let { Ical.unescape(it).trim() }.orEmpty()
            if (old == want) return false
            when {
                want.isEmpty() -> ps.forEach(::dropWithLabels)
                first != null -> replaceValue(first, (listOf(escapeComponent(want)) + parts.drop(1)).joinToString(";"))
                else -> adds += insertAfter("ORG") to fold("ORG:" + escapeComponent(want))
            }
            return true
        }

        /**
         * Значения EMAIL/TEL — к [want]. Совпавшие остаются своими строками (с
         * типом, группой, подписью); несовпавшие старые и новые сопоставляются
         * по порядку — значение меняется в той же строке; лишние старые уходят
         * (с подписью группы), лишние новые встают за последней строкой.
         */
        fun setList(name: String, want: List<String>): Boolean {
            val clean = want.map { it.trim() }.filter { it.isNotEmpty() }
            val ps = props(name)
            val cur = ps.map { clientValue(it.value) }
            if (cur == clean) return false
            val remaining = clean.groupingBy { it }.eachCount().toMutableMap()
            val unmatched = mutableListOf<Line>()
            for ((i, l) in ps.withIndex()) {
                val n = remaining[cur[i]] ?: 0
                if (n > 0) remaining[cur[i]] = n - 1 else unmatched += l
            }
            val fresh = mutableListOf<String>()
            for (v in clean) {
                val n = remaining[v] ?: 0
                if (n > 0) {
                    remaining[v] = n - 1
                    fresh += v
                }
            }
            val at = insertAfter(name)
            for (i in 0 until maxOf(unmatched.size, fresh.size)) {
                when {
                    i < unmatched.size && i < fresh.size -> replaceValue(unmatched[i], escapeText(fresh[i]))
                    i < unmatched.size -> dropWithLabels(unmatched[i])
                    else -> adds += at to fold("$name:" + escapeText(fresh[i]))
                }
            }
            return true
        }

        /** Убрать строку; у группы, где кроме подписей Apple (`X-AB…`) ничего не осталось, — и их. */
        fun dropWithLabels(l: Line) {
            l.drop = true
            if (l.group.isEmpty()) return
            val mates = mutableListOf<Line>()
            for (i in begin..end) {
                val m = lines[i]
                if (m.drop || m.group != l.group) continue
                if (!m.name.startsWith("X-AB")) return
                mates += m
            }
            mates.forEach { it.drop = true }
        }
    }

    /** Значение так, как его видит Android ([VCard.parse]). */
    private fun clientValue(raw: String) = Ical.unescape(raw.trim()).trim()

    /** Новое значение с группой и параметрами строки; параметры кодировки (vCard 2.1) к нему не подходят. */
    private fun replaceValue(l: Line, escaped: String) {
        var prefix = l.prefix
        if (prefix.uppercase().contains("ENCODING=")) {
            prefix = if (l.group.isNotEmpty()) l.prefix.substring(0, l.group.length) + "." + l.name else l.name
        }
        l.repl = fold("$prefix:$escaped")
    }

    /** Имя и фамилия: фамилия — последнее слово (или первое, если карточка писала «Фамилия Имя»). */
    private fun splitName(full: String, familyFirst: Boolean): Pair<String, String> {
        val w = full.split(Regex("\\s+")).filter { it.isNotEmpty() }
        return when {
            w.isEmpty() -> "" to ""
            w.size == 1 -> w[0] to ""
            familyFirst -> w.drop(1).joinToString(" ") to w[0]
            else -> w.dropLast(1).joinToString(" ") to w.last()
        }
    }

    /** Компоненты структурного значения — по неэкранированной `;`, экранирование внутри сохраняется. */
    private fun splitComponents(v: String): List<String> {
        val out = mutableListOf<String>()
        var start = 0
        var i = 0
        while (i < v.length) {
            when (v[i]) {
                '\\' -> i++
                ';' -> {
                    out += v.substring(start, i)
                    start = i + 1
                }
            }
            i++
        }
        out += v.substring(minOf(start, v.length))
        return out
    }

    /** Простое значение: `\`, запятая, перевод строки (точку с запятой RFC 6350 §3.4 экранировать не требует). */
    private fun escapeText(s: String) =
        s.replace("\\", "\\\\").replace(",", "\\,").replace("\r\n", "\\n").replace("\n", "\\n").replace("\r", "\\n")

    /** Компонент составного значения (N, ORG): ещё и точка с запятой. */
    private fun escapeComponent(s: String) = escapeText(s).replace(";", "\\;")

    /** Свёртка по 75 октетов (RFC 6350 §3.2), не разрывая UTF-8. */
    private fun fold(s: String): List<String> {
        val bytes = s.toByteArray(Charsets.UTF_8)
        if (bytes.size <= 75) return listOf(s)
        val out = mutableListOf<String>()
        var pos = 0
        var limit = 75
        var prefix = ""
        while (bytes.size - pos > limit) {
            var cut = pos + limit
            while (cut > pos && (bytes[cut].toInt() and 0xC0) == 0x80) cut--
            if (cut == pos) cut = pos + limit
            out += prefix + String(bytes, pos, cut - pos, Charsets.UTF_8)
            pos = cut
            prefix = " "
            limit = 74
        }
        out += prefix + String(bytes, pos, bytes.size - pos, Charsets.UTF_8)
        return out
    }
}
