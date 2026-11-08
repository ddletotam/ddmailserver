package ru.letotam.ddmail.auth

/**
 * Разбор сырого DNS-ответа (RFC 1035) на MX-записи: (приоритет, хост).
 * `DnsResolver.rawQuery` отдаёт ответ байтами, разбора у Android нет.
 * Битый ответ — пустой список, а не исключение: MX для поиска — подсказка.
 */
internal fun parseMxAnswer(msg: ByteArray): List<Pair<Int, String>> {
    fun u16(i: Int) = ((msg[i].toInt() and 0xff) shl 8) or (msg[i + 1].toInt() and 0xff)
    return try {
        if (msg.size < 12) return emptyList()
        val rcode = msg[3].toInt() and 0x0f
        if (rcode != 0) return emptyList()
        val qd = u16(4)
        val an = u16(6)
        var pos = 12
        repeat(qd) { pos = skipName(msg, pos) + 4 }
        val out = mutableListOf<Pair<Int, String>>()
        repeat(an) {
            pos = skipName(msg, pos)
            val type = u16(pos)
            val rdlen = u16(pos + 8)
            val rdata = pos + 10
            if (type == 15) {
                val pref = u16(rdata)
                val host = readName(msg, rdata + 2).trimEnd('.').lowercase()
                if (host.isNotEmpty()) out += pref to host
            }
            pos = rdata + rdlen
        }
        out.sortedBy { it.first }
    } catch (_: IndexOutOfBoundsException) {
        emptyList()
    }
}

private fun skipName(msg: ByteArray, start: Int): Int {
    var pos = start
    while (true) {
        val len = msg[pos].toInt() and 0xff
        when {
            len == 0 -> return pos + 1
            len and 0xc0 == 0xc0 -> return pos + 2
            else -> pos += len + 1
        }
    }
}

/** Имя с поддержкой сжатия (указатели 0xC0); защита от петель — по числу прыжков. */
private fun readName(msg: ByteArray, start: Int): String {
    val labels = mutableListOf<String>()
    var pos = start
    var jumps = 0
    while (true) {
        val len = msg[pos].toInt() and 0xff
        when {
            len == 0 -> return labels.joinToString(".")
            len and 0xc0 == 0xc0 -> {
                if (++jumps > 16) return labels.joinToString(".")
                pos = ((len and 0x3f) shl 8) or (msg[pos + 1].toInt() and 0xff)
            }
            else -> {
                labels += String(msg, pos + 1, len, Charsets.US_ASCII)
                pos += len + 1
            }
        }
    }
}
