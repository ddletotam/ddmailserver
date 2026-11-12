package ru.letotam.ddmail.imap

/**
 * Имена папок IMAP — modified UTF-7 (RFC 3501 §5.1.3): печатный ASCII как
 * есть, `&` → `&-`, прочее — `&<base64 UTF-16BE с ',' вместо '/'>-`.
 * «Отправленные» на сервере — `&BB4EQgQ,BEAEMAQyBDsENQQ9BD0ESwQ1-`.
 */
object ModifiedUtf7 {
    private const val ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,"

    fun decode(s: String): String {
        val out = StringBuilder()
        var i = 0
        while (i < s.length) {
            val c = s[i]
            if (c != '&') {
                out.append(c)
                i++
                continue
            }
            val end = s.indexOf('-', i + 1)
            if (end < 0) {
                out.append(s, i, s.length)
                break
            }
            if (end == i + 1) {
                out.append('&')
            } else {
                val bytes = b64decode(s.substring(i + 1, end))
                if (bytes == null) out.append(s, i, end + 1) else out.append(String(bytes, Charsets.UTF_16BE))
            }
            i = end + 1
        }
        return out.toString()
    }

    fun encode(s: String): String {
        val out = StringBuilder()
        var i = 0
        while (i < s.length) {
            val c = s[i]
            if (c in ' '..'~') {
                out.append(if (c == '&') "&-" else c.toString())
                i++
                continue
            }
            var j = i
            while (j < s.length && s[j] !in ' '..'~') j++
            out.append('&').append(b64encode(s.substring(i, j).toByteArray(Charsets.UTF_16BE))).append('-')
            i = j
        }
        return out.toString()
    }

    private fun b64encode(b: ByteArray): String {
        val sb = StringBuilder()
        var bits = 0
        var acc = 0
        for (x in b) {
            acc = (acc shl 8) or (x.toInt() and 0xff)
            bits += 8
            while (bits >= 6) {
                bits -= 6
                sb.append(ALPHABET[(acc shr bits) and 0x3f])
            }
        }
        if (bits > 0) sb.append(ALPHABET[(acc shl (6 - bits)) and 0x3f])
        return sb.toString()
    }

    private fun b64decode(s: String): ByteArray? {
        val out = java.io.ByteArrayOutputStream()
        var bits = 0
        var acc = 0
        for (c in s) {
            val v = ALPHABET.indexOf(c)
            if (v < 0) return null
            acc = (acc shl 6) or v
            bits += 6
            if (bits >= 8) {
                bits -= 8
                out.write((acc shr bits) and 0xff)
            }
        }
        return out.toByteArray()
    }
}
