package ru.letotam.ddmail.dav

import ru.letotam.ddmail.api.AttendeeDto
import ru.letotam.ddmail.api.EventDto
import java.time.Instant
import java.time.LocalDate
import java.time.LocalDateTime
import java.time.ZoneId
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter

/**
 * iCalendar для внешних CalDAV (порт `caldav_client.rs`), с тем, что у
 * десктопа отсутствует (контракт mobile-external-accounts §8): `TZID`
 * толкуется по базе часовых поясов ОС (`ZoneId`), незнакомое имя — по
 * `VTIMEZONE` того же календаря (смещение `STANDARD`), «плавающее» время —
 * в поясе телефона, а не «как UTC». «Весь день» — полночь UTC, как у сервера.
 */
object Ical {
    private val UTC_STAMP = DateTimeFormatter.ofPattern("yyyyMMdd'T'HHmmss'Z'").withZone(ZoneOffset.UTC)
    private val DATE = DateTimeFormatter.ofPattern("yyyyMMdd")
    private val LOCAL = DateTimeFormatter.ofPattern("yyyyMMdd'T'HHmmss")

    /** Строка свойства: имя, параметры, значение. */
    data class Prop(val name: String, val params: Map<String, String>, val value: String)

    /** RFC 5545 §3.1: строка, начинающаяся с пробела/таба, продолжает предыдущую. */
    fun unfold(s: String): List<String> {
        val out = mutableListOf<String>()
        for (line in s.replace("\r\n", "\n").split('\n')) {
            if ((line.startsWith(" ") || line.startsWith("\t")) && out.isNotEmpty()) {
                out[out.size - 1] = out.last() + line.substring(1)
            } else if (line.isNotEmpty()) {
                out += line
            }
        }
        return out
    }

    /** `NAME;P=V;Q="a:b":value` — двоеточие внутри кавычек параметра не делит. */
    fun prop(line: String): Prop? {
        var inQuote = false
        var colon = -1
        for ((i, c) in line.withIndex()) {
            if (c == '"') inQuote = !inQuote
            if (c == ':' && !inQuote) {
                colon = i
                break
            }
        }
        if (colon < 0) return null
        val head = line.substring(0, colon).split(';')
        val params = head.drop(1).mapNotNull { p ->
            val eq = p.indexOf('=')
            if (eq < 0) null else p.substring(0, eq).uppercase() to p.substring(eq + 1).trim('"')
        }.toMap()
        return Prop(head[0].uppercase(), params, line.substring(colon + 1))
    }

    fun unescape(s: String) = s.replace("\\n", "\n").replace("\\N", "\n").replace("\\,", ",").replace("\\;", ";").replace("\\\\", "\\")

    fun escape(s: String) = s.replace("\\", "\\\\").replace("\n", "\\n").replace(",", "\\,").replace(";", "\\;")

    /** VTIMEZONE календаря: TZID → смещение стандартного времени. */
    private fun vtimezones(lines: List<String>): Map<String, ZoneOffset> {
        val out = mutableMapOf<String, ZoneOffset>()
        var tzid: String? = null
        var inStd = false
        for (l in lines) {
            val p = prop(l) ?: continue
            when {
                p.name == "BEGIN" && p.value == "VTIMEZONE" -> tzid = null
                p.name == "TZID" && !inStd -> tzid = p.value
                p.name == "BEGIN" && p.value == "STANDARD" -> inStd = true
                p.name == "END" && p.value == "STANDARD" -> inStd = false
                p.name == "TZOFFSETTO" && inStd && tzid != null ->
                    runCatching { ZoneOffset.of(p.value.let { if (it.length == 5) it.substring(0, 3) + ":" + it.substring(3) else it }) }.getOrNull()?.let { out[tzid!!] = it }
            }
        }
        return out
    }

    /** Пояс по TZID: база ОС, затем VTIMEZONE, затем пояс телефона. */
    fun zone(tzid: String?, vtz: Map<String, ZoneOffset>, local: ZoneId): ZoneId {
        if (tzid.isNullOrBlank()) return local
        val clean = tzid.trim().removePrefix("/")
        return runCatching { ZoneId.of(clean) }.getOrNull() ?: vtz[tzid] ?: local
    }

    /** Значение даты/времени → (мс, весь день). */
    fun parseDt(p: Prop, vtz: Map<String, ZoneOffset>, local: ZoneId): Pair<Long, Boolean>? {
        val v = p.value.trim()
        return try {
            if (p.params["VALUE"] == "DATE" || v.length == 8) {
                LocalDate.parse(v, DATE).atStartOfDay(ZoneOffset.UTC).toInstant().toEpochMilli() to true
            } else if (v.endsWith("Z")) {
                LocalDateTime.parse(v.dropLast(1), LOCAL).toInstant(ZoneOffset.UTC).toEpochMilli() to false
            } else {
                LocalDateTime.parse(v, LOCAL).atZone(zone(p.params["TZID"], vtz, local)).toInstant().toEpochMilli() to false
            }
        } catch (e: Exception) {
            null
        }
    }

    /** `TRIGGER` относительно начала → минуты до начала (`-PT15M` → 15). */
    fun triggerMinutes(v: String): Int? {
        val m = Regex("""^([+-])?P(?:(\d+)W)?(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$""").find(v.trim()) ?: return null
        val (sign, w, d, h, mi, s) = m.destructured
        val total = (w.toIntOrNull() ?: 0) * 7 * 1440 + (d.toIntOrNull() ?: 0) * 1440 + (h.toIntOrNull() ?: 0) * 60 + (mi.toIntOrNull() ?: 0) + (s.toIntOrNull() ?: 0) / 60
        if (sign == "+" && total > 0) return null
        return total
    }

    /** События VCALENDAR (мастер и его переопределения — отдельно, как у десктопа). */
    fun events(vcal: String, local: ZoneId = ZoneId.systemDefault()): List<EventDto> {
        val lines = unfold(vcal)
        val vtz = vtimezones(lines)
        val out = mutableListOf<EventDto>()
        var cur: MutableList<Prop>? = null
        var depth = 0
        var leads = mutableListOf<Int>()
        for (l in lines) {
            val p = prop(l) ?: continue
            if (p.name == "BEGIN" && p.value.uppercase() == "VEVENT") {
                cur = mutableListOf()
                leads = mutableListOf()
                depth = 0
                continue
            }
            val c = cur ?: continue
            if (p.name == "BEGIN") depth++
            if (p.name == "END" && p.value.uppercase() == "VEVENT" && depth == 0) {
                parse(c, leads, vtz, local)?.let(out::add)
                cur = null
                continue
            }
            if (p.name == "END") depth--
            if (depth > 0) {
                // Внутри VALARM: только TRIGGER относительно начала.
                if (p.name == "TRIGGER" && p.params["RELATED"] != "END" && p.params["VALUE"] != "DATE-TIME") triggerMinutes(p.value)?.let(leads::add)
                continue
            }
            c += p
        }
        return out
    }

    private fun parse(props: List<Prop>, leads: List<Int>, vtz: Map<String, ZoneOffset>, local: ZoneId): EventDto? {
        fun v(n: String) = props.firstOrNull { it.name == n }?.value
        val start = props.firstOrNull { it.name == "DTSTART" }?.let { parseDt(it, vtz, local) } ?: return null
        val end = props.firstOrNull { it.name == "DTEND" }?.let { parseDt(it, vtz, local) }?.first
        val org = props.firstOrNull { it.name == "ORGANIZER" }
        val ex = props.filter { it.name == "EXDATE" }.flatMap { p ->
            p.value.split(',').mapNotNull { x -> parseDt(p.copy(value = x), vtz, local)?.first }
        }
        val sorted = leads.distinct().sortedDescending()
        return EventDto(
            id = 0,
            uid = v("UID").orEmpty(),
            summary = unescape(v("SUMMARY").orEmpty()),
            description = unescape(v("DESCRIPTION").orEmpty()),
            location = unescape(v("LOCATION").orEmpty()),
            dtstart = start.first,
            dtend = end,
            allDay = start.second,
            organizerEmail = org?.value?.removePrefix("mailto:")?.removePrefix("MAILTO:").orEmpty(),
            organizerName = org?.params?.get("CN").orEmpty(),
            status = v("STATUS").orEmpty(),
            rrule = v("RRULE").orEmpty(),
            recurrenceId = v("RECURRENCE-ID").orEmpty(),
            exdates = ex,
            attendees = props.filter { it.name == "ATTENDEE" }.map { a ->
                AttendeeDto(
                    email = a.value.removePrefix("mailto:").removePrefix("MAILTO:"),
                    name = a.params["CN"].orEmpty(),
                    role = a.params["ROLE"].orEmpty(),
                    partstat = a.params["PARTSTAT"].orEmpty(),
                )
            },
            alarmLeadMin = sorted.firstOrNull() ?: 0,
            alarmLeads = sorted,
        )
    }

    private fun dtLine(name: String, ms: Long, allDay: Boolean): String =
        if (allDay) "$name;VALUE=DATE:" + DATE.format(Instant.ofEpochMilli(ms).atZone(ZoneOffset.UTC)) else "$name:" + UTC_STAMP.format(Instant.ofEpochMilli(ms))

    fun stamp(ms: Long): String = UTC_STAMP.format(Instant.ofEpochMilli(ms))

    /** Новое событие (`build_ical` десктопа). */
    fun build(uid: String, summary: String, description: String, location: String, start: Long, end: Long?, allDay: Boolean, now: Long = System.currentTimeMillis()): String {
        val sb = StringBuilder("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//ddmail//android//RU\r\nBEGIN:VEVENT\r\n")
        sb.append("UID:").append(uid).append("\r\n")
        sb.append("DTSTAMP:").append(stamp(now)).append("\r\n")
        sb.append(dtLine("DTSTART", start, allDay)).append("\r\n")
        end?.let { sb.append(dtLine("DTEND", it, allDay)).append("\r\n") }
        if (summary.isNotEmpty()) sb.append("SUMMARY:").append(escape(summary)).append("\r\n")
        if (description.isNotEmpty()) sb.append("DESCRIPTION:").append(escape(description)).append("\r\n")
        if (location.isNotEmpty()) sb.append("LOCATION:").append(escape(location)).append("\r\n")
        sb.append("END:VEVENT\r\nEND:VCALENDAR\r\n")
        return sb.toString()
    }

    /**
     * Правка существующего (`merge_ical` десктопа): заменяются переданные
     * (не `null`) свойства мастера, остальное — RRULE, участники, будильники —
     * сохраняется. Переопределения (RECURRENCE-ID) не трогаются.
     */
    fun merge(
        existing: String,
        summary: String?,
        description: String?,
        location: String?,
        start: Long?,
        end: Long?,
        allDay: Boolean?,
        dropEnd: Boolean = false,
    ): String {
        val raw = existing.replace("\r\n", "\n").split('\n')
        // Свёрнутые строки склеиваем, чтобы заменить свойство целиком.
        val lines = mutableListOf<String>()
        for (l in raw) if ((l.startsWith(" ") || l.startsWith("\t")) && lines.isNotEmpty()) lines[lines.size - 1] += l.substring(1) else lines += l
        val isAllDay = allDay ?: lines.any { it.uppercase().startsWith("DTSTART;VALUE=DATE:") }
        val updates = LinkedHashMap<String, String?>()
        summary?.let { updates["SUMMARY"] = "SUMMARY:" + escape(it) }
        description?.let { updates["DESCRIPTION"] = "DESCRIPTION:" + escape(it) }
        location?.let { updates["LOCATION"] = "LOCATION:" + escape(it) }
        start?.let { updates["DTSTART"] = dtLine("DTSTART", it, isAllDay) }
        if (end != null) updates["DTEND"] = dtLine("DTEND", end, isAllDay) else if (dropEnd) updates["DTEND"] = null
        val out = mutableListOf<String>()
        var i = 0
        while (i < lines.size) {
            val l = lines[i]
            if (l.uppercase() != "BEGIN:VEVENT") {
                if (l.isNotEmpty()) out += l
                i++
                continue
            }
            // Блок VEVENT целиком; верхний уровень — вне VALARM и т.п.
            val block = mutableListOf<Pair<String, Boolean>>()
            var depth = 0
            while (i < lines.size) {
                val x = lines[i]
                val u = x.uppercase()
                val top = depth == 0
                if (u.startsWith("BEGIN:") && u != "BEGIN:VEVENT") depth++
                if (u.startsWith("END:") && u != "END:VEVENT") depth--
                block += x to (top && !u.startsWith("BEGIN:V") || u == "BEGIN:VEVENT")
                i++
                if (u == "END:VEVENT") break
            }
            val isOverride = block.any { (x, top) -> top && x.uppercase().startsWith("RECURRENCE-ID") }
            if (isOverride) {
                out += block.map { it.first }
                continue
            }
            val seen = mutableSetOf<String>()
            val result = mutableListOf<String>()
            for ((x, top) in block) {
                val name = x.substringBefore(':').substringBefore(';').uppercase()
                if (top && name in updates) {
                    seen += name
                    updates[name]?.let(result::add)
                } else {
                    result += x
                }
            }
            val missing = updates.filter { it.key !in seen }.values.filterNotNull()
            result.addAll(result.size - 1, missing)
            out += result
        }
        return out.joinToString("\r\n", postfix = "\r\n")
    }

    fun stampNow(): String = stamp(System.currentTimeMillis())
}
