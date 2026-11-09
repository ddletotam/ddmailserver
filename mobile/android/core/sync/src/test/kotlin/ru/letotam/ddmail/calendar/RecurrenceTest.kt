package ru.letotam.ddmail.calendar

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.time.DayOfWeek
import java.time.Instant
import java.time.ZoneId
import java.time.ZoneOffset
import java.time.ZonedDateTime

/** Те же случаи, что `recurrence.rs` десктопа, плюс стенные часы пояса. */
class RecurrenceTest {
    private val utc = ZoneOffset.UTC
    private val mon0900 = ZonedDateTime.of(2026, 6, 8, 9, 0, 0, 0, utc).toInstant().toEpochMilli()
    private fun day(n: Long) = n * 86_400_000

    private fun expand(s: Long, e: Long?, rule: String, ex: List<Long>, from: Long, to: Long, zone: ZoneId = utc) =
        Recurrence.expand(s, e, rule, ex, from, to, zone)

    @Test fun nonRecurringInWindow() {
        val occ = expand(mon0900, mon0900 + 3_600_000, "", emptyList(), mon0900 - day(1), mon0900 + day(1))
        assertEquals(listOf(Occurrence(mon0900, mon0900 + 3_600_000)), occ)
    }

    @Test fun nonRecurringOutOfWindow() {
        assertTrue(expand(mon0900, null, "", emptyList(), mon0900 + day(5), mon0900 + day(7)).isEmpty())
    }

    @Test fun dailyMasterInPastStillFillsWindow() {
        val occ = expand(mon0900, mon0900 + 3_600_000, "FREQ=DAILY", emptyList(), mon0900 + day(30), mon0900 + day(31))
        assertEquals(listOf(mon0900 + day(30)), occ.map { it.startMs })
    }

    @Test fun dailyIntervalAndCount() {
        val occ = expand(mon0900, null, "FREQ=DAILY;INTERVAL=2;COUNT=3", emptyList(), mon0900 - day(1), mon0900 + day(30))
        assertEquals(listOf(mon0900, mon0900 + day(2), mon0900 + day(4)), occ.map { it.startMs })
    }

    @Test fun weeklyUntilBound() {
        val occ = expand(mon0900, null, "FREQ=WEEKLY;UNTIL=20260624T090000Z", emptyList(), mon0900 - day(1), mon0900 + day(60))
        assertEquals(3, occ.size)
        assertEquals(mon0900 + day(14), occ[2].startMs)
    }

    @Test fun weeklyBydayMulti() {
        val occ = expand(mon0900, null, "FREQ=WEEKLY;BYDAY=MO,WE,FR", emptyList(), mon0900, mon0900 + day(7))
        assertEquals(listOf(mon0900, mon0900 + day(2), mon0900 + day(4)), occ.map { it.startMs })
    }

    @Test fun exdateRemovesInstance() {
        val occ = expand(mon0900, null, "FREQ=DAILY;COUNT=3", listOf(mon0900 + day(1)), mon0900 - day(1), mon0900 + day(10))
        assertEquals(listOf(mon0900, mon0900 + day(2)), occ.map { it.startMs })
    }

    @Test fun monthlyKeepsDayOfMonth() {
        val jan31 = ZonedDateTime.of(2026, 1, 31, 9, 0, 0, 0, utc).toInstant().toEpochMilli()
        val end = ZonedDateTime.of(2026, 4, 1, 0, 0, 0, 0, utc).toInstant().toEpochMilli()
        val months = expand(jan31, null, "FREQ=MONTHLY", emptyList(), jan31, end).map { Instant.ofEpochMilli(it.startMs).atZone(utc).monthValue }
        assertEquals(listOf(1, 3), months)
    }

    @Test fun yearlyStepsAYear() {
        val s = ZonedDateTime.of(2024, 2, 29, 12, 0, 0, 0, utc).toInstant().toEpochMilli()
        val end = ZonedDateTime.of(2029, 1, 1, 0, 0, 0, 0, utc).toInstant().toEpochMilli()
        val years = expand(s, null, "FREQ=YEARLY", emptyList(), s, end).map { Instant.ofEpochMilli(it.startMs).atZone(utc).year }
        assertEquals(listOf(2024, 2028), years)
    }

    /** Понедельник 01:00 по Москве — в UTC это воскресенье; дни недели считаются по местному времени. */
    @Test fun bydayUsesLocalWallClock() {
        val msk = ZoneId.of("Europe/Moscow")
        val s = ZonedDateTime.of(2026, 6, 8, 1, 0, 0, 0, msk).toInstant().toEpochMilli()
        val occ = expand(s, null, "FREQ=WEEKLY;BYDAY=MO", emptyList(), s, s + day(15), msk)
        assertEquals(3, occ.size)
        occ.forEach { assertEquals(DayOfWeek.MONDAY, Instant.ofEpochMilli(it.startMs).atZone(msk).dayOfWeek) }
    }

    /** Ежедневное в 09:00 остаётся в 09:00 по обе стороны перехода на летнее время. */
    @Test fun dailyKeepsWallClockAcrossDst() {
        val berlin = ZoneId.of("Europe/Berlin")
        val s = ZonedDateTime.of(2026, 3, 27, 9, 0, 0, 0, berlin).toInstant().toEpochMilli()
        val occ = expand(s, null, "FREQ=DAILY;COUNT=4", emptyList(), s, s + day(10), berlin)
        occ.forEach { assertEquals(9, Instant.ofEpochMilli(it.startMs).atZone(berlin).hour) }
    }

    @Test fun describeRule() {
        assertEquals("Каждую неделю: пн, ср", Recurrence.describe("FREQ=WEEKLY;BYDAY=WE,MO"))
        assertEquals("Каждые 2 дн.", Recurrence.describe("RRULE:FREQ=DAILY;INTERVAL=2"))
        assertEquals("", Recurrence.describe(""))
    }
}
