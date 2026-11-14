package ru.letotam.ddmail.conversation

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test
import ru.letotam.ddmail.api.Delivery
import ru.letotam.ddmail.api.DeliveryEvent

/**
 * Заглушка отправляемого письма снимает «Отправляется…» по событию доставки,
 * не дожидаясь, пока копия придёт дельтой бесед (0.0.42: раньше — 4–20 с).
 */
class StubDeliveryTest {
    private fun ev(id: String, status: String, error: String = "") =
        DeliveryEvent(messageId = 9, rfcMessageId = id, status = status, error = error)

    private val delivered = """{"status":"delivered","error":"","failed":[]}"""

    @Test
    fun eventAfterAckMarksTheStub() {
        val s = StubDelivery()
        assertNull(s.sent("s1", "<a@x>"))
        val (key, d) = s.event(ev("<a@x>", Delivery.DELIVERED))!!
        assertEquals("s1", key)
        assertEquals(delivered, d.toString())
    }

    /** Письмо себе доставлено раньше, чем пришёл ответ `/send`. */
    @Test
    fun eventBeforeAckIsKept() {
        val s = StubDelivery()
        assertNull(s.event(ev("<a@x>", Delivery.DELIVERED)))
        assertEquals(delivered, s.sent("s1", "<a@x>").toString())
    }

    @Test
    fun plainPendingChangesNothing() {
        val s = StubDelivery()
        s.sent("s1", "<a@x>")
        assertNull(s.event(ev("<a@x>", Delivery.PENDING)))
        // С причиной — янтарная пометка «повторяем».
        assertEquals("s1", s.event(ev("<a@x>", Delivery.PENDING, "сервер не отвечает"))!!.first)
    }

    @Test
    fun otherMessagesAndOldServersAreIgnored() {
        val s = StubDelivery()
        s.sent("s1", "<a@x>")
        assertNull(s.event(ev("<b@x>", Delivery.DELIVERED)))
        // Старый сервер не вернул Message-ID — заглушка ждёт дельту, как раньше.
        assertNull(s.sent("s2", ""))
        assertNull(s.event(ev("", Delivery.DELIVERED)))
    }

    @Test
    fun keepsOnlyRecentEvents() {
        val s = StubDelivery()
        repeat(100) { s.event(ev("<old$it@x>", Delivery.DELIVERED)) }
        assertNull(s.sent("s1", "<old0@x>"))
        assertEquals(delivered, s.sent("s2", "<old99@x>").toString())
    }
}
