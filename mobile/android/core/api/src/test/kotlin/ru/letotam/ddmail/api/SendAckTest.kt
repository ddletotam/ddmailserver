package ru.letotam.ddmail.api

import kotlinx.coroutines.test.runTest
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Test

/** Ответ `/send`: Message-ID копии в «Отправленных» — для пометки заглушки. */
class SendAckTest {
    private val server = MockWebServer().apply { start() }

    private object Tokens : TokenSource {
        override suspend fun current() = "jwt"
        override suspend fun update(token: String) {}
    }

    private val api = SessionApi(server.url("/").toString().trimEnd('/'), OkHttpClient(), Tokens)

    private val req = SendRequest(from = "me@x", to = listOf("me@x"), subject = "", html = "<div>x</div>", text = "x")

    @After
    fun stop() = server.shutdown()

    @Test
    fun ackCarriesTheCopysMessageId() = runTest {
        server.enqueue(
            MockResponse().setBody(
                """{"status":"queued","message_id":1519,"rfc_message_id":"<abc@mail.letotam.ru>","delivery_status":"pending"}""",
            ),
        )
        val ack = api.send(req)
        assertEquals(SendAck(1519, "<abc@mail.letotam.ru>", "pending"), ack)
        assertEquals("/api/desktop/v1/send", server.takeRequest().path)
    }

    /** Ответ без полей — письмо всё равно принято. */
    @Test
    fun bareAckIsStillAccepted() = runTest {
        server.enqueue(MockResponse().setBody("""{"status":"queued"}"""))
        assertEquals(SendAck(), api.send(req))
        server.enqueue(MockResponse().setBody(""))
        assertEquals(SendAck(), api.send(req))
    }
}
