package ru.letotam.ddmail.calendar

import ru.letotam.ddmail.api.EventDto
import ru.letotam.ddmail.db.ReminderEntity
import java.time.ZoneId

/**
 * Машина состояний напоминаний — та же модель, что у десктопа
 * (`reminders.rs`, спецификация 2026-07-11), без Android: посев из событий,
 * срок следующего звена, что делать при срабатывании.
 *
 * Каскад — VALARM'ы события (минуты до начала). Звено сработало →
 * уведомление и взводится следующее; «✕» (смахнули уведомление) — тишина
 * по вхождению навсегда; «напомнить позже» — каскад заменяется одним сроком.
 */
object ReminderPlanner {
    const val ARMED = "armed"
    const val DONE = "done"
    const val CANCELLED = "cancelled"

    /** Вхождение без конца считается идущим столько. */
    const val NO_END_RUNNING_MS = 30 * 60 * 1000L

    /** Окно посева: от сегодняшней полуночи на 8 суток (как `for_reminders` десктопа). */
    const val WINDOW_DAYS = 8L

    private const val PRUNE_AFTER_MS = 48 * 3600 * 1000L

    /** «Наступило» превращается в «уже идёт», если опоздали больше чем на это. */
    private const val LATE_MS = 2 * 60 * 1000L

    enum class Mode { Soon, AtStart, Running }

    data class Seed(val delete: List<Pair<Long, Long>>, val put: List<ReminderEntity>)

    fun leadsOf(r: ReminderEntity): List<Int> = r.leads.split(',').mapNotNull { it.trim().toIntOrNull() }

    /** Когда сработать; `null` — ничего не взведено. */
    fun fireAt(r: ReminderEntity): Long? {
        if (r.state != ARMED) return null
        r.snoozeAt?.let { return it }
        val lead = leadsOf(r).getOrNull(r.next) ?: return null
        return r.occStart - lead * 60_000L
    }

    fun end(r: ReminderEntity): Long = if (r.occEnd > 0) r.occEnd else r.occStart + NO_END_RUNNING_MS

    /** Первое звено, которое ещё впереди; все прошли, а событие нет — последнее (сработает сразу). */
    private fun firstNext(occStart: Long, leads: List<Int>, now: Long): Int {
        val i = leads.indexOfFirst { occStart - it * 60_000L > now }
        return if (i >= 0) i else (leads.size - 1).coerceAtLeast(0)
    }

    /**
     * Посев из событий окна `[windowFrom, windowTo)`. Скрытые календари и
     * события на весь день не звонят (как у десктопа). Строки окна, которых
     * события больше не производят (удалили, перенесли, скрыли календарь), —
     * удаляются. Решение пользователя (снуз, ✕) держится за логическое
     * вхождение «начало + название», а не за id события: сервер может
     * пересоздать событие с новым id, и без этого напоминание воскресло бы.
     */
    fun seed(
        existing: List<ReminderEntity>,
        events: List<EventDto>,
        hidden: Set<Long>,
        now: Long,
        windowFrom: Long,
        windowTo: Long,
        zone: ZoneId = ZoneId.systemDefault(),
    ): Seed {
        val byKey = existing.associateBy { it.eventId to it.occStart }.toMutableMap()
        val put = mutableListOf<ReminderEntity>()
        val delete = mutableListOf<Pair<Long, Long>>()
        val produced = mutableSetOf<Pair<Long, Long>>()
        // Логическое вхождение → уже есть строка (своя или чужого id).
        val logical = existing.associateBy { it.occStart to it.summary }.toMutableMap()

        for (ev in events) {
            val summary = ev.summary.trim()
            if (summary.isEmpty() || ev.allDay || ev.calendarId in hidden) continue
            val leads = (ev.alarmLeads.ifEmpty { if (ev.alarmLeadMin > 0) listOf(ev.alarmLeadMin) else listOf(10) })
                .filter { it >= 0 }.distinct().sortedDescending()
            if (leads.isEmpty()) continue
            val signature = "${ev.dtstart}|${ev.dtend ?: 0}|${leads.joinToString(",")}|$summary"
            val dur = ev.dtend?.let { (it - ev.dtstart).coerceAtLeast(0) } ?: 0
            val starts = if (ev.rrule.isBlank()) listOf(ev.dtstart)
            else Recurrence.expand(ev.dtstart, ev.dtend, ev.rrule, ev.exdates, windowFrom, windowTo, zone).map { it.startMs }
            for (start in starts) {
                if (start <= now || start >= windowTo) continue
                val key = ev.id to start
                produced += key
                val old = byKey[key]
                if (old != null) {
                    if (old.signature != signature && old.state != CANCELLED) {
                        put += old.copy(
                            occEnd = if (dur > 0) start + dur else 0, calendarId = ev.calendarId, summary = summary,
                            leads = leads.joinToString(","), signature = signature,
                            next = firstNext(start, leads, now), snoozeAt = null, state = ARMED,
                        )
                    }
                    continue
                }
                val twin = logical[start to summary]
                val row = ReminderEntity(
                    eventId = ev.id, occStart = start, occEnd = if (dur > 0) start + dur else 0,
                    calendarId = ev.calendarId, summary = summary, leads = leads.joinToString(","),
                    signature = signature, next = firstNext(start, leads, now), snoozeAt = null, state = ARMED,
                )
                if (twin != null && twin.eventId != ev.id) {
                    if (twin.eventId to twin.occStart in produced) continue // живой близнец — одна встреча, одно напоминание
                    // Событие пересоздано с новым id: решение переезжает на него.
                    put += row.copy(next = twin.next, snoozeAt = twin.snoozeAt, state = twin.state)
                    delete += twin.eventId to twin.occStart
                    byKey.remove(twin.eventId to twin.occStart)
                } else {
                    put += row
                }
                logical[start to summary] = row
            }
        }
        for (r in byKey.values) {
            val key = r.eventId to r.occStart
            if (key in produced) continue
            val inWindow = r.occStart in windowFrom until windowTo
            if (inWindow || r.occStart < now - PRUNE_AFTER_MS) delete += key
        }
        return Seed(delete.distinct(), put)
    }

    /** Звено сработало: что показать и какой стала строка. `null`-режим — молча. */
    fun fire(r: ReminderEntity, now: Long): Pair<Mode?, ReminderEntity> {
        if (now >= end(r)) return null to r.copy(state = DONE, snoozeAt = null)
        if (now >= r.occStart) {
            val mode = if (now - r.occStart > LATE_MS) Mode.Running else Mode.AtStart
            return mode to r.copy(state = DONE, snoozeAt = null)
        }
        val wasSnooze = r.snoozeAt != null
        val leads = leadsOf(r)
        // После снуза каскад не продолжается: пользователь выбрал срок сам.
        val next = if (wasSnooze) leads.size else r.next + 1
        val mode = if (r.occStart - now <= 60_000L) Mode.AtStart else Mode.Soon
        // Звенья, которые уже в прошлом (телефон спал), пропускаем — иначе пачка уведомлений подряд.
        var n = next
        while (n < leads.size && r.occStart - leads[n] * 60_000L <= now) n++
        val state = if (n < leads.size) ARMED else DONE
        return mode to r.copy(next = n, snoozeAt = null, state = state)
    }

    /** «Напомнить позже» — шаги, которые ложатся до начала (как `snooze_steps` десктопа). */
    val SNOOZE_STEPS = listOf(1, 5, 10, 15, 30, 60, 120, 180, 360, 720, 1440)

    fun snoozeSteps(minsUntil: Long): List<Int> = SNOOZE_STEPS.filter { it < minsUntil }

    fun snoozeLabel(m: Int): String = when {
        m < 60 -> "Через $m мин"
        m < 1440 -> "Через ${m / 60} ч"
        else -> "Через ${m / 1440} д"
    }
}
