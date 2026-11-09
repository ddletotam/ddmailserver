package ru.letotam.ddmail.calendar

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import ru.letotam.ddmail.api.EventDto
import ru.letotam.ddmail.calendar.ReminderPlanner.ARMED
import ru.letotam.ddmail.calendar.ReminderPlanner.CANCELLED
import ru.letotam.ddmail.calendar.ReminderPlanner.DONE
import ru.letotam.ddmail.calendar.ReminderPlanner.Mode
import java.time.ZoneOffset

class ReminderPlannerTest {
    private val min = 60_000L
    private val now = 1_800_000_000_000L
    private val from = now - 10 * 3600_000L
    private val to = now + 8 * 86_400_000L

    private fun ev(id: Long, start: Long, leads: List<Int> = listOf(15, 5), summary: String = "Созвон", cal: Long = 1, rrule: String = "") =
        EventDto(id = id, calendarId = cal, summary = summary, dtstart = start, dtend = start + 60 * min, alarmLeads = leads, rrule = rrule)

    private fun seed(existing: List<ru.letotam.ddmail.db.ReminderEntity>, vararg e: EventDto, hidden: Set<Long> = emptySet(), at: Long = now) =
        ReminderPlanner.seed(existing, e.toList(), hidden, at, from, to, ZoneOffset.UTC)

    @Test fun seedsCascadeSortedAndFiresInOrder() {
        val s = seed(emptyList(), ev(1, now + 60 * min, leads = listOf(5, 15)))
        val r = s.put.single()
        assertEquals("15,5", r.leads)
        assertEquals(now + 45 * min, ReminderPlanner.fireAt(r))
        val (mode, after) = ReminderPlanner.fire(r, now + 45 * min)
        assertEquals(Mode.Soon, mode)
        assertEquals(now + 55 * min, ReminderPlanner.fireAt(after))
        val (_, last) = ReminderPlanner.fire(after, now + 55 * min)
        assertEquals(DONE, last.state)
        assertNull(ReminderPlanner.fireAt(last))
    }

    @Test fun pastLeadsSkippedButLateSeedStillFiresOnce() {
        // До начала 3 минуты, оба звена (15 и 5) уже в прошлом — одно напоминание сразу.
        val r = seed(emptyList(), ev(1, now + 3 * min)).put.single()
        assertEquals(1, r.next)
        assertTrue(ReminderPlanner.fireAt(r)!! <= now)
    }

    @Test fun hiddenCalendarAndAllDayAreSilent() {
        val allDay = ev(2, now + 60 * min).copy(allDay = true)
        assertTrue(seed(emptyList(), ev(1, now + 60 * min, cal = 7), allDay, hidden = setOf(7)).put.isEmpty())
    }

    @Test fun unchangedEventKeepsProgress() {
        val r = seed(emptyList(), ev(1, now + 60 * min)).put.single()
        val progressed = ReminderPlanner.fire(r, now + 45 * min).second
        val again = seed(listOf(progressed), ev(1, now + 60 * min), at = now + 46 * min)
        assertTrue(again.put.isEmpty())
        assertTrue(again.delete.isEmpty())
    }

    @Test fun movedEventDropsOldOccurrence() {
        val r = seed(emptyList(), ev(1, now + 60 * min)).put.single()
        val moved = seed(listOf(r), ev(1, now + 120 * min))
        assertEquals(listOf(1L to now + 60 * min), moved.delete)
        assertEquals(now + 120 * min, moved.put.single().occStart)
    }

    @Test fun dismissalSurvivesEventIdChurn() {
        val r = seed(emptyList(), ev(1, now + 60 * min)).put.single().copy(state = CANCELLED)
        val churn = seed(listOf(r), ev(99, now + 60 * min))
        val row = churn.put.single()
        assertEquals(99L, row.eventId)
        assertEquals(CANCELLED, row.state)
        assertEquals(listOf(1L to now + 60 * min), churn.delete)
    }

    @Test fun twinsInTwoCalendarsRingOnce() {
        val s = seed(emptyList(), ev(1, now + 60 * min, cal = 1), ev(2, now + 60 * min, cal = 2))
        assertEquals(1, s.put.size)
    }

    @Test fun snoozeReplacesCascade() {
        val r = seed(emptyList(), ev(1, now + 60 * min)).put.single()
        val snoozed = r.copy(snoozeAt = now + 10 * min)
        assertEquals(now + 10 * min, ReminderPlanner.fireAt(snoozed))
        val (mode, after) = ReminderPlanner.fire(snoozed, now + 10 * min)
        assertEquals(Mode.Soon, mode)
        assertEquals(DONE, after.state)
    }

    @Test fun firingAfterStartIsAtStartOrRunning() {
        val r = seed(emptyList(), ev(1, now + 60 * min, leads = listOf(0))).put.single()
        assertEquals(Mode.AtStart, ReminderPlanner.fire(r, now + 60 * min).first)
        assertEquals(Mode.Running, ReminderPlanner.fire(r, now + 70 * min).first)
        val (mode, row) = ReminderPlanner.fire(r, now + 61 * 60 * min)
        assertNull(mode)
        assertEquals(DONE, row.state)
    }

    @Test fun recurringMasterSeedsEachOccurrence() {
        val s = seed(emptyList(), ev(1, now - 86_400_000L + 60 * min, rrule = "FREQ=DAILY"))
        assertTrue(s.put.size >= 7)
        assertTrue(s.put.all { it.state == ARMED && it.occStart > now })
    }

    @Test fun snoozeStepsLandBeforeStart() {
        assertEquals(listOf(1, 5, 10, 15), ReminderPlanner.snoozeSteps(20))
    }
}
