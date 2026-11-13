package ru.letotam.ddmail.sync

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.api.Delivery
import ru.letotam.ddmail.api.MessageBody
import ru.letotam.ddmail.api.MessageRef
import ru.letotam.ddmail.imap.MailException
import ru.letotam.ddmail.sync.CacheRows.toDomain
import ru.letotam.ddmail.sync.CacheRows.toEntity

class DeliveryTest {
    private fun body(status: String, text: String? = "x") = MessageBody(uid = 7, text = text, deliveryStatus = status)
    private fun ref(status: String) = MessageRef("Sent", 7, "m@x", true, status)

    @Test
    fun cachedBodyStaleWhenStatusChanged() {
        assertFalse(bodyStale(ref(""), body("")))
        assertFalse(bodyStale(ref(Delivery.PENDING), body(Delivery.PENDING)))
        // Дельта принесла смену статуса, а событие WS до нас не дошло.
        assertTrue(bodyStale(ref(Delivery.FAILED), body(Delivery.PENDING)))
        assertTrue(bodyStale(ref(Delivery.DELIVERED), body(Delivery.PENDING)))
        // Пустое тело — сбойный фетч, как и раньше.
        assertTrue(bodyStale(ref(""), body("", text = null)))
    }

    @Test
    fun bodyDeliveryRoundTripsThroughCache() {
        val b = MessageBody(
            uid = 7, text = "x", deliveryStatus = Delivery.FAILED, deliveryReason = "unreachable",
            deliveryError = "Почтовый сервер получателя не отвечает.", deliveryFailed = listOf("a@x", "b@x"),
        )
        assertEquals(b, b.toEntity("acc").toDomain())
        // Строка кэша без статуса (до миграции 5) — письмо без пометки.
        val legacy = MessageBody(uid = 8, text = "y").toEntity("acc").copy(deliveryFailedJson = "[]")
        assertEquals("", legacy.toDomain().deliveryStatus)
        assertEquals(emptyList<String>(), legacy.toDomain().deliveryFailed)
    }

    @Test
    fun refStatusRoundTripsThroughCache() {
        val e = ref(Delivery.PENDING).toEntity("acc", "conv")
        assertEquals(Delivery.PENDING, e.deliveryStatus)
    }

    @Test
    fun smtpFailureReadsAsItsReason() {
        val e = SmtpFailed(MailException.Rejected("Сервер не принял письмо: 550 no such user"))
        assertTrue(e.isMailError())
        assertFalse(e.isAuthError())
        assertEquals("Сервер не принял письмо: 550 no such user", e.errorText())
        // Пароль SMTP отвергнут — это ещё и «нужен вход».
        assertTrue(SmtpFailed(MailException.Auth("535")).isAuthError())
        assertTrue(SmtpFailed(ApiException.Unauthorized("")).isAuthError())
    }
}
