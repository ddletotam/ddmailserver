package ru.letotam.ddmail.calendar

import org.junit.Assert.assertEquals
import org.junit.Test

class MonthChipsShownTest {
    private val chip = 16
    private val more = 14

    @Test
    fun allFit() {
        assertEquals(3, monthChipsShown(List(3) { chip }, more, 48))
        assertEquals(0, monthChipsShown(emptyList(), more, 0))
    }

    @Test
    fun overflowLeavesRoomForMore() {
        // Пять плашек по 16 в 60: без подписи влезло бы три (48), с подписью — две (32 + 14 = 46).
        assertEquals(2, monthChipsShown(List(5) { chip }, more, 60))
        // Ровно впритык: 3 × 16 + 14 = 62.
        assertEquals(3, monthChipsShown(List(5) { chip }, more, 62))
    }

    @Test
    fun moreWinsOverLastChip() {
        // Две плашки ровно в 32 — показываются обе; третья — уже «ещё 2» и одна плашка.
        assertEquals(2, monthChipsShown(List(2) { chip }, more, 32))
        assertEquals(1, monthChipsShown(List(3) { chip }, more, 32))
    }

    @Test
    fun tinyCellShowsOnlyMore() {
        assertEquals(0, monthChipsShown(List(4) { chip }, more, 20))
        assertEquals(0, monthChipsShown(List(4) { chip }, more, 5))
    }
}
