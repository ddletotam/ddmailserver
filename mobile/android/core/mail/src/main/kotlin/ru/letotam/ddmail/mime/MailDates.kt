package ru.letotam.ddmail.mime

import java.time.LocalDateTime
import java.time.ZoneOffset

/**
 * Даты почты. `Date:` (RFC 5322, со старыми формами: без дня недели,
 * двузначный год, зоны `GMT`/`UT`/`MSK`, комментарии) и INTERNALDATE IMAP
 * (`17-Jul-1996 02:44:25 -0700`). Не разобралась — 0 (тогда берут INTERNALDATE).
 */
object MailDates {
    private val MONTHS = listOf("jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec")
    private val RFC = Regex("""(\d{1,2})[\s-]+([A-Za-z]{3})[A-Za-z]*[\s-]+(\d{2,4})\s+(\d{1,2}):(\d{2})(?::(\d{2}))?(?:\.\d+)?\s*([+-]\d{2}:?\d{2}|[A-Za-z]{1,5})?""")
    private val ZONES = mapOf(
        "UT" to 0, "UTC" to 0, "GMT" to 0, "Z" to 0,
        "EST" to -5 * 60, "EDT" to -4 * 60, "CST" to -6 * 60, "CDT" to -5 * 60,
        "MST" to -7 * 60, "MDT" to -6 * 60, "PST" to -8 * 60, "PDT" to -7 * 60,
        "MSK" to 3 * 60, "CET" to 60, "CEST" to 120, "EET" to 120, "EEST" to 180,
    )

    /** Секунды Unix или 0. */
    fun parse(s: String?): Long {
        if (s.isNullOrBlank()) return 0
        val noComments = s.replace(Regex("""\([^)]*\)"""), " ")
        val m = RFC.find(noComments) ?: return 0
        val (d, mon, y, hh, mm, ss, zone) = m.destructured
        val month = MONTHS.indexOf(mon.lowercase()) + 1
        if (month == 0) return 0
        var year = y.toInt()
        if (y.length == 2) year += if (year < 50) 2000 else 1900 else if (y.length == 3) year += 1900
        val offsetMin = when {
            zone.isEmpty() -> 0
            zone[0] == '+' || zone[0] == '-' -> {
                val z = zone.replace(":", "")
                val sign = if (z[0] == '-') -1 else 1
                sign * (z.substring(1, 3).toInt() * 60 + z.substring(3, 5).toInt())
            }
            else -> ZONES[zone.uppercase()] ?: 0
        }
        return try {
            val ldt = LocalDateTime.of(year, month, d.toInt(), hh.toInt(), mm.toInt(), ss.ifEmpty { "0" }.toInt())
            ldt.toEpochSecond(ZoneOffset.ofTotalSeconds(offsetMin * 60))
        } catch (e: Exception) {
            0
        }
    }
}
