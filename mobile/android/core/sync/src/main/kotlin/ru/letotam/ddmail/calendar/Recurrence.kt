package ru.letotam.ddmail.calendar

import java.time.DayOfWeek
import java.time.Instant
import java.time.LocalDate
import java.time.LocalDateTime
import java.time.ZoneId
import java.time.ZoneOffset
import java.time.ZonedDateTime
import java.time.temporal.TemporalAdjusters

/** Одно вхождение события, мс. */
data class Occurrence(val startMs: Long, val endMs: Long)

/**
 * Развёртка RRULE — тот же поднабор, что у десктопа (`recurrence.rs`):
 * FREQ=DAILY|WEEKLY|MONTHLY|YEARLY, INTERVAL, COUNT, UNTIL, BYDAY (у WEEKLY).
 * Прочее (BYMONTHDAY, BYSETPOS…) игнорируется; неизвестная частота — событие
 * как разовое.
 *
 * Отличие от десктопа намеренное: шаг считается по стенным часам [zone], а не
 * по UTC. Иначе событие «по понедельникам в 01:00 МСК» (в UTC — воскресенье
 * 22:00) раскладывалось бы на вторники, а ежедневное — уезжало бы на час при
 * переходе на летнее время там, где он есть.
 */
object Recurrence {
    private const val MAX_ITER = 20_000
    private const val DEFAULT_DURATION = 30 * 60 * 1000L

    private enum class Freq { DAILY, WEEKLY, MONTHLY, YEARLY }

    private class Rule(val freq: Freq, val interval: Long, val count: Int?, val untilMs: Long?, val byDay: List<DayOfWeek>)

    fun expand(
        dtstartMs: Long,
        dtendMs: Long?,
        rrule: String,
        exdates: Collection<Long>,
        winStartMs: Long,
        winEndMs: Long,
        zone: ZoneId = ZoneId.systemDefault(),
    ): List<Occurrence> {
        val dur = dtendMs?.let { (it - dtstartMs).coerceAtLeast(0) } ?: DEFAULT_DURATION
        val ex = exdates.toHashSet()
        fun overlaps(s: Long): Boolean {
            val e = s + dur
            // Нулевая длительность (точка во времени) тоже попадает в окно.
            return (e > winStartMs || (dur == 0L && s >= winStartMs)) && s < winEndMs
        }
        val out = mutableListOf<Occurrence>()
        fun push(s: Long) {
            if (overlaps(s) && s !in ex) out += Occurrence(s, s + dur)
        }
        val rule = parse(rrule)
        if (rule == null) {
            push(dtstartMs)
            return out
        }
        val start = Instant.ofEpochMilli(dtstartMs).atZone(zone)

        if (rule.freq == Freq.WEEKLY && rule.byDay.isNotEmpty()) {
            val monday = start.toLocalDate().with(TemporalAdjusters.previousOrSame(DayOfWeek.MONDAY))
            val time = start.toLocalTime()
            val days = rule.byDay.sortedBy { it.value }
            var produced = 0
            var block = 0L
            repeat(MAX_ITER) {
                val weekStart = monday.plusWeeks(block * rule.interval)
                var any = false
                for (d in days) {
                    val s = ZonedDateTime.of(weekStart.plusDays((d.value - 1).toLong()), time, zone).toInstant().toEpochMilli()
                    if (s < dtstartMs) continue
                    if (rule.untilMs != null && s > rule.untilMs) return out
                    if (rule.count != null && produced >= rule.count) return out
                    if (s > winEndMs && produced > 0) return out
                    produced++
                    any = true
                    push(s)
                }
                if (!any && weekStart > start.toLocalDate() &&
                    weekStart.atStartOfDay(zone).toInstant().toEpochMilli() > winEndMs
                ) return out
                block++
            }
            return out
        }

        for (i in 0 until MAX_ITER) {
            val s = nth(start, rule, i.toLong()) ?: continue
            if (rule.untilMs != null && s > rule.untilMs) break
            if (rule.count != null && i >= rule.count) break
            if (s > winEndMs) break
            push(s)
        }
        return out
    }

    /** n-е вхождение простого правила; `null` — такой даты нет (31 февраля). */
    private fun nth(start: ZonedDateTime, rule: Rule, n: Long): Long? {
        val step = n * rule.interval
        val d: LocalDateTime = when (rule.freq) {
            Freq.DAILY -> start.toLocalDateTime().plusDays(step)
            Freq.WEEKLY -> start.toLocalDateTime().plusWeeks(step)
            Freq.MONTHLY, Freq.YEARLY -> {
                val months = if (rule.freq == Freq.MONTHLY) step else step * 12
                val ym = java.time.YearMonth.from(start).plusMonths(months)
                // Нет такого числа в месяце — вхождение пропускается (как у десктопа и RFC 5545).
                if (start.dayOfMonth > ym.lengthOfMonth()) return null
                LocalDateTime.of(ym.atDay(start.dayOfMonth), start.toLocalTime())
            }
        }
        return d.atZone(start.zone).toInstant().toEpochMilli()
    }

    private fun parse(rrule: String): Rule? {
        val s = rrule.trim().removePrefix("RRULE:").takeIf { it.isNotEmpty() } ?: return null
        var freq: Freq? = null
        var interval = 1L
        var count: Int? = null
        var until: Long? = null
        val byDay = mutableListOf<DayOfWeek>()
        for (part in s.split(';')) {
            val k = part.substringBefore('=', "").trim().uppercase()
            val v = part.substringAfter('=', "").trim()
            if (k.isEmpty()) return null
            when (k) {
                "FREQ" -> freq = runCatching { Freq.valueOf(v.uppercase()) }.getOrNull() ?: return null
                "INTERVAL" -> interval = v.toLongOrNull()?.takeIf { it > 0 } ?: 1
                "COUNT" -> count = v.toIntOrNull()
                "UNTIL" -> until = parseUntil(v)
                "BYDAY" -> v.split(',').forEach { tok ->
                    weekday(tok.trim().trimStart('+', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9'))?.let(byDay::add)
                }
            }
        }
        return Rule(freq ?: return null, interval, count, until, byDay)
    }

    /** UNTIL: `YYYYMMDD` (конец дня) или `YYYYMMDDTHHMMSS[Z]`, как у десктопа — в UTC. */
    internal fun parseUntil(v: String): Long? {
        val t = v.trimEnd('Z')
        val date = t.substringBefore('T')
        if (date.length != 8) return null
        val y = date.substring(0, 4).toIntOrNull() ?: return null
        val m = date.substring(4, 6).toIntOrNull() ?: return null
        val d = date.substring(6, 8).toIntOrNull() ?: return null
        val time = t.substringAfter('T', "")
        val (h, mi, se) = if (time.length >= 6) {
            Triple(time.substring(0, 2).toIntOrNull() ?: return null, time.substring(2, 4).toIntOrNull() ?: return null, time.substring(4, 6).toIntOrNull() ?: return null)
        } else Triple(23, 59, 59)
        return runCatching { LocalDateTime.of(LocalDate.of(y, m, d), java.time.LocalTime.of(h, mi, se)).toInstant(ZoneOffset.UTC).toEpochMilli() }.getOrNull()
    }

    private fun weekday(s: String): DayOfWeek? = when (s.uppercase()) {
        "MO" -> DayOfWeek.MONDAY
        "TU" -> DayOfWeek.TUESDAY
        "WE" -> DayOfWeek.WEDNESDAY
        "TH" -> DayOfWeek.THURSDAY
        "FR" -> DayOfWeek.FRIDAY
        "SA" -> DayOfWeek.SATURDAY
        "SU" -> DayOfWeek.SUNDAY
        else -> null
    }

    /** Человеческое описание правила для карточки: «каждую неделю», «каждые 2 дня»… */
    fun describe(rrule: String): String {
        val r = parse(rrule) ?: return ""
        val n = r.interval
        val base = when (r.freq) {
            Freq.DAILY -> if (n == 1L) "Каждый день" else "Каждые $n дн."
            Freq.WEEKLY -> if (n == 1L) "Каждую неделю" else "Каждые $n нед."
            Freq.MONTHLY -> if (n == 1L) "Каждый месяц" else "Каждые $n мес."
            Freq.YEARLY -> if (n == 1L) "Каждый год" else "Каждые $n г."
        }
        val days = if (r.byDay.isNotEmpty()) {
            val names = listOf("пн", "вт", "ср", "чт", "пт", "сб", "вс")
            ": " + r.byDay.sortedBy { it.value }.joinToString(", ") { names[it.value - 1] }
        } else ""
        return base + days
    }
}
