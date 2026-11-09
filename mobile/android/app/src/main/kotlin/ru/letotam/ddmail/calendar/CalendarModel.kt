package ru.letotam.ddmail.calendar

import androidx.compose.ui.graphics.Color
import ru.letotam.ddmail.api.CalendarDto
import ru.letotam.ddmail.api.EventDto
import java.time.Instant
import java.time.LocalDate
import java.time.LocalDateTime
import java.time.YearMonth
import java.time.ZoneId
import java.time.ZoneOffset

/** Вхождение события на экране: мастер развёрнут, цвет решён. */
data class Occ(val ev: EventDto, val start: Long, val end: Long, val color: Color) {
    val key: String get() = "${ev.id}:$start"
    val allDay: Boolean get() = ev.allDay
    val recurring: Boolean get() = ev.rrule.isNotBlank()

    /** Даты, которые вхождение занимает (конец исключён). */
    fun dates(zone: ZoneId = ZoneId.systemDefault()): Sequence<LocalDate> {
        val first = dateOf(start, allDay, zone)
        val lastExcl = if (allDay) dateOf(end, true, zone).coerceAtLeast(first.plusDays(1))
        else dateOf((end - 1).coerceAtLeast(start), false, zone).plusDays(1)
        return generateSequence(first) { it.plusDays(1) }.takeWhile { it < lastExcl }
    }

    fun startTime(zone: ZoneId = ZoneId.systemDefault()): LocalDateTime = Instant.ofEpochMilli(start).atZone(zone).toLocalDateTime()
    fun endTime(zone: ZoneId = ZoneId.systemDefault()): LocalDateTime = Instant.ofEpochMilli(end).atZone(zone).toLocalDateTime()

    companion object {
        /**
         * Дата события «на весь день». Из CalDAV они приходят полночью UTC
         * (`VALUE=DATE`), созданные клиентом — местной полночью; полночь UTC
         * читается как дата UTC, иначе к западу от Гринвича день съезжал бы на
         * сутки назад.
         */
        fun dateOf(ms: Long, allDay: Boolean, zone: ZoneId = ZoneId.systemDefault()): LocalDate =
            if (allDay && Math.floorMod(ms, 86_400_000L) == 0L) Instant.ofEpochMilli(ms).atZone(ZoneOffset.UTC).toLocalDate()
            else Instant.ofEpochMilli(ms).atZone(zone).toLocalDate()
    }
}

/** Цвета календарей: серверный, а без него — из той же пастели, что у групп адресов. */
object CalendarColors {
    private val FALLBACK = listOf(
        0xFF3B82F6, 0xFF10B981, 0xFFF59E0B, 0xFFEF4444, 0xFF8B5CF6, 0xFF06B6D4, 0xFFEC4899, 0xFF84CC16,
    )

    fun parse(hex: String): Color? {
        val h = hex.trim().removePrefix("#")
        val v = h.toLongOrNull(16) ?: return null
        return when (h.length) {
            6 -> Color(0xFF000000 or v)
            8 -> Color(((v and 0xFF) shl 24) or (v ushr 8)) // #RRGGBBAA (CalDAV/Apple)
            else -> null
        }
    }

    fun of(cal: CalendarDto?, id: Long, eventColor: String = ""): Color =
        parse(eventColor) ?: cal?.let { parse(it.color) } ?: Color(FALLBACK[Math.floorMod(id, FALLBACK.size.toLong()).toInt()])
}

/**
 * Развёртка событий месяца по дням. Многодневное попадает в каждый свой
 * день; месяц знает только свои дни — соседний месяц приходит своим запросом
 * (сервер отдаёт всё, что пересекает окно).
 */
fun indexMonth(
    ym: YearMonth,
    events: List<EventDto>,
    calendars: Map<Long, CalendarDto>,
    hidden: Set<Long>,
    zone: ZoneId = ZoneId.systemDefault(),
): Map<LocalDate, List<Occ>> {
    val from = ym.atDay(1).atStartOfDay(zone).toInstant().toEpochMilli()
    val to = ym.plusMonths(1).atDay(1).atStartOfDay(zone).toInstant().toEpochMilli()
    val out = HashMap<LocalDate, MutableList<Occ>>()
    val seen = HashSet<String>()
    for (ev in events) {
        if (ev.calendarId in hidden) continue
        val color = CalendarColors.of(calendars[ev.calendarId], ev.calendarId, ev.color)
        val occs = if (ev.allDay && ev.rrule.isBlank()) {
            listOf(ru.letotam.ddmail.calendar.Occurrence(ev.dtstart, ev.dtend ?: ev.dtstart))
        } else {
            // Окно расширено на сутки: у дат «весь день» в UTC другая полночь.
            Recurrence.expand(ev.dtstart, ev.dtend, ev.rrule, ev.exdates, from - 86_400_000L, to + 86_400_000L, zone)
        }
        for (o in occs) {
            val occ = Occ(ev, o.startMs, o.endMs, color)
            if (!seen.add(occ.key)) continue
            for (d in occ.dates(zone)) if (YearMonth.from(d) == ym) out.getOrPut(d) { mutableListOf() } += occ
        }
    }
    return out.mapValues { (_, l) -> l.sortedWith(compareBy<Occ>({ !it.allDay }, { it.start }, { it.ev.summary })) }
}
