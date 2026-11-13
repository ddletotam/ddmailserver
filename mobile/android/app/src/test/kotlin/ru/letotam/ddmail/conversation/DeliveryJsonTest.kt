package ru.letotam.ddmail.conversation

import org.junit.Assert.assertEquals
import org.junit.Test
import ru.letotam.ddmail.api.Delivery
import ru.letotam.ddmail.api.MessageBody

/** Поле `delivery` письма в документе ленты — общий с iOS формат `{status, error, failed}`. */
class DeliveryJsonTest {
    @Test
    fun failedWithReasonAndAddresses() {
        val b = MessageBody(
            uid = 1, deliveryStatus = Delivery.FAILED, deliveryReason = "unreachable",
            deliveryError = "Почтовый сервер получателя не отвечает.", deliveryFailed = listOf("a@x", "b@x"),
        )
        assertEquals(
            """{"status":"failed","error":"Почтовый сервер получателя не отвечает.","failed":["a@x","b@x"]}""",
            deliveryJson(b).toString(),
        )
    }

    @Test
    fun pendingWithoutReason() {
        assertEquals(
            """{"status":"pending","error":"","failed":[]}""",
            deliveryJson(MessageBody(uid = 1, deliveryStatus = Delivery.PENDING)).toString(),
        )
    }
}
