package ru.letotam.ddmail.conversation

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class MenuAnchorTest {
    // WebView 1080×1800 px с отступом (0, 200) в окне 1080×2400; видимая область — 360 CSS px (density 3).
    private fun anchor(x: Double, y: Double, w: Double, h: Double, px: Double = x + w / 2, py: Double = y + h / 2, vw: Double = 360.0) =
        MenuAnchor.fromDocument(0, 200, 1080, 1800, x, y, w, h, px, py, vw)!!

    @Test
    fun scalesByViewportWidth() {
        val a = anchor(10.0, 100.0, 200.0, 50.0)
        assertEquals(MenuAnchor(30, 500, 630, 650, 330, 575, 0, 200, 1080, 2000), a)
        // Щипок ×2: видимая область 180 CSS px — та же точка вдвое дальше.
        val z = anchor(10.0, 100.0, 20.0, 10.0, vw = 180.0)
        assertEquals(60, z.left)
        assertEquals(800, z.top)
    }

    @Test
    fun rejectsBadInput() {
        assertNull(MenuAnchor.fromDocument(0, 0, 1080, 1800, 0.0, 0.0, 1.0, 1.0, 0.0, 0.0, 0.0))
        assertNull(MenuAnchor.fromDocument(0, 0, 0, 1800, 0.0, 0.0, 1.0, 1.0, 0.0, 0.0, 360.0))
        assertNull(MenuAnchor.fromDocument(0, 0, 1080, 1800, Double.NaN, 0.0, 1.0, 1.0, 0.0, 0.0, 360.0))
    }

    private fun place(a: MenuAnchor, w: Int = 600, h: Int = 700) = menuPlacement(a, w, h, 1080, 2400, margin = 24, gap = 12)

    @Test
    fun belowIncomingBubbleAlignedLeft() {
        val a = anchor(10.0, 100.0, 200.0, 50.0) // слева, низ на 650
        assertEquals(30 to 662, place(a))
    }

    @Test
    fun outgoingBubbleAlignedRight() {
        val a = anchor(150.0, 100.0, 200.0, 50.0) // справа: right = 1050
        assertEquals(450 to 662, place(a))
    }

    @Test
    fun aboveWhenNoRoomBelow() {
        val a = anchor(10.0, 500.0, 200.0, 50.0) // верх 1700, низ 1850: снизу 700 не влезает
        assertEquals(30 to 1700 - 12 - 700, place(a))
    }

    @Test
    fun tallBubbleUsesTouchPoint() {
        // Пузырь выше ленты: ни верх, ни низ не видны — меню у пальца.
        val a = anchor(10.0, -200.0, 340.0, 1200.0, px = 100.0, py = 300.0)
        assertEquals(30 to 1100, place(a))
    }

    @Test
    fun clampedToWindow() {
        // Палец у самого низа, меню выше остатка — прижато к нижнему полю.
        val a = anchor(10.0, -200.0, 340.0, 1200.0, px = 100.0, py = 590.0)
        assertEquals(2400 - 24 - 700, place(a).second)
        // Меню выше окна — от верхнего поля.
        assertEquals(24, place(a, h = 3000).second)
        // Шире окна — от левого поля.
        assertEquals(24, place(a, w = 2000).first)
    }
}
