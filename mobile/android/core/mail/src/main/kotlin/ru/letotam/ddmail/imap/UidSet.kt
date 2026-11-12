package ru.letotam.ddmail.imap

/** Множества UID (RFC 3501 sequence-set): сжатие в диапазоны и разбор. */
object UidSet {
    /** `[1,2,3,7,9,10]` → `1:3,7,9:10`. Пусто — пустая строка. */
    fun format(uids: Collection<Long>): String {
        if (uids.isEmpty()) return ""
        val sorted = uids.toSortedSet().toList()
        val sb = StringBuilder()
        var start = sorted[0]
        var prev = start
        fun flush() {
            if (sb.isNotEmpty()) sb.append(',')
            sb.append(start)
            if (prev != start) sb.append(':').append(prev)
        }
        for (u in sorted.drop(1)) {
            if (u == prev + 1) {
                prev = u
                continue
            }
            flush()
            start = u
            prev = u
        }
        flush()
        return sb.toString()
    }

    /** Порции не длиннее [max] UID — длинная команда упирается в лимиты серверов. */
    fun chunks(uids: Collection<Long>, max: Int = 500): List<String> =
        uids.toSortedSet().chunked(max).map { format(it) }

    /** `1:3,7` → диапазоны; `*` — [Long.MAX_VALUE]. */
    fun parse(s: String): List<LongRange> = s.split(',').mapNotNull { part ->
        val p = part.trim()
        if (p.isEmpty()) return@mapNotNull null
        fun n(x: String) = if (x == "*") Long.MAX_VALUE else x.toLongOrNull()
        val i = p.indexOf(':')
        if (i < 0) n(p)?.let { it..it } else {
            val a = n(p.substring(0, i)) ?: return@mapNotNull null
            val b = n(p.substring(i + 1)) ?: return@mapNotNull null
            minOf(a, b)..maxOf(a, b)
        }
    }

    fun contains(ranges: List<LongRange>, uid: Long) = ranges.any { uid in it }
}
