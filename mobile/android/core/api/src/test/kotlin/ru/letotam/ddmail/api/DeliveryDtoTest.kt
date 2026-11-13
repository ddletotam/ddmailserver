package ru.letotam.ddmail.api

import kotlinx.serialization.builtins.ListSerializer
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/** Статус доставки исходящих: поля тел, бесед и событие WS (контракт Android §5). */
class DeliveryDtoTest {
    private val json = DdmailApi.json

    @Test
    fun bodyCarriesDeliveryFields() {
        // Форма ответа прод-сервера для письма на мёртвый домен.
        val text = """[{"uid":587975,"folder":"Sent","subject":"","from":"lucky <info@example.ru>",
            "from_addr":"info@example.ru","to":["loko@dead.example"],"cc":[],"date_ts":1794591005,
            "text":"test","attachments":[],"is_outgoing":true,"message_id":"1.e7@example.ru",
            "in_reply_to":"","references":[],"delivery_status":"failed","delivery_reason":"unreachable",
            "delivery_error":"Почтовый сервер получателя не отвечает.","delivery_failed":["loko@dead.example"]}]"""
        val b = json.decodeFromString(ListSerializer(MessageBody.serializer()), text).single()
        assertEquals(Delivery.FAILED, b.deliveryStatus)
        assertEquals("unreachable", b.deliveryReason)
        assertEquals("Почтовый сервер получателя не отвечает.", b.deliveryError)
        assertEquals(listOf("loko@dead.example"), b.deliveryFailed)
    }

    @Test
    fun bodyWithoutDeliveryIsUnknown() {
        // Старый сервер и входящие: полей нет или статус пустой, отказ — null (nil-срез Go).
        val b = json.decodeFromString(
            MessageBody.serializer(),
            """{"uid":1,"delivery_status":"","delivery_failed":null}""",
        )
        assertEquals("", b.deliveryStatus)
        assertEquals("", b.deliveryError)
        assertEquals(emptyList<String>(), b.deliveryFailed)
        val old = json.decodeFromString(MessageBody.serializer(), """{"uid":2}""")
        assertEquals("", old.deliveryStatus)
    }

    @Test
    fun conversationRowAndRefsCarryStatus() {
        val c = json.decodeFromString(
            ConversationDto.serializer(),
            """{"id":"a|b","last_delivery_status":"pending","messages":[
                {"folder":"INBOX","uid":1,"message_id":"in@x","seen":true},
                {"folder":"Sent","uid":2,"message_id":"out@x","seen":true,"delivery_status":"pending"}]}""",
        )
        assertEquals(Delivery.PENDING, c.lastDeliveryStatus)
        assertEquals(listOf("", Delivery.PENDING), c.messages.map { it.deliveryStatus })
        assertEquals("", json.decodeFromString(ConversationDto.serializer(), """{"id":"x"}""").lastDeliveryStatus)
    }

    @Test
    fun refWithoutStatusIsSentAsBefore() {
        // Ссылки уходят в /conversations/messages: пустой статус в запрос не попадает.
        val s = json.encodeToString(MessageRef.serializer(), MessageRef("Sent", 2, "m@x", true))
        assertFalse(s, s.contains("delivery_status"))
    }

    @Test
    fun deliveryStatusEventParsed() {
        val (type, frame) = wsFrame(
            """{"type":"delivery_status","user_id":7,"message_id":587975,"rfc_message_id":"1.e7@example.ru",
                "delivery_status":"failed","delivery_reason":"domain_not_found",
                "delivery_error":"Домен получателя не найден.","delivery_failed":["a@dead.example","b@dead.example"]}""",
        )!!
        assertEquals("delivery_status", type)
        val ev = deliveryEvent(frame)
        assertEquals(
            DeliveryEvent(587975, "1.e7@example.ru", Delivery.FAILED, "domain_not_found", "Домен получателя не найден.", listOf("a@dead.example", "b@dead.example")),
            ev,
        )
    }

    @Test
    fun deliveryEventPendingWithoutReason() {
        // pending → delivered: причины и адресов нет (omitempty).
        val ev = deliveryEvent(wsFrame("""{"type":"delivery_status","message_id":5,"delivery_status":"delivered"}""")!!.second)
        assertNotNull(ev)
        assertEquals(Delivery.DELIVERED, ev!!.status)
        assertEquals("", ev.error)
        assertTrue(ev.failed.isEmpty())
    }

    @Test
    fun otherFramesAreNotDeliveryEvents() {
        // message_sent: тот же message_id, но это не смена статуса.
        val sent = wsFrame("""{"type":"message_sent","message_id":5,"rfc_message_id":"m@x"}""")!!
        assertEquals("message_sent", sent.first)
        assertNull(deliveryEvent(sent.second))
        // Без письма или без статуса — применять нечего.
        assertNull(deliveryEvent(wsFrame("""{"type":"delivery_status","delivery_status":"failed"}""")!!.second))
        assertNull(deliveryEvent(wsFrame("""{"type":"delivery_status","message_id":5}""")!!.second))
        assertNull(wsFrame("not json"))
        assertNull(wsFrame("""["delivery_status"]"""))
        assertNull(wsFrame("""{"user_id":1}"""))
    }
}
