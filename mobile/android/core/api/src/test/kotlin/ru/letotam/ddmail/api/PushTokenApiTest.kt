package ru.letotam.ddmail.api

import kotlinx.coroutines.test.runTest
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** Регистрация устройства для push: POST/DELETE /push/token (контракт §7). */
class PushTokenApiTest {
    private val server = MockWebServer().apply { start() }

    private object Tokens : TokenSource {
        override suspend fun current() = "jwt"
        override suspend fun update(token: String) {}
    }

    private val api = SessionApi(server.url("/").toString().trimEnd('/'), OkHttpClient(), Tokens)

    @After
    fun stop() = server.shutdown()

    @Test
    fun registerPostsKindTokenTopic() = runTest {
        server.enqueue(MockResponse().setBody("""{"kind":"rustore","delivery":true}"""))
        assertTrue(api.registerPushToken("rustore", "tok-1", "ru.letotam.ddmail"))
        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/api/desktop/v1/push/token", req.path)
        assertEquals("Bearer jwt", req.getHeader("Authorization"))
        val o = Json.parseToJsonElement(req.body.readUtf8()).jsonObject
        assertEquals("rustore", o["kind"]!!.jsonPrimitive.content)
        assertEquals("tok-1", o["token"]!!.jsonPrimitive.content)
        assertEquals("ru.letotam.ddmail", o["topic"]!!.jsonPrimitive.content)
        // Окружение — только у APNs: сервер отверг бы его у Android.
        assertFalse("environment" in o)
    }

    @Test
    fun registerWithoutDeliveryOnServer() = runTest {
        server.enqueue(MockResponse().setBody("""{"kind":"hms","delivery":false}"""))
        assertFalse(api.registerPushToken("hms", "t", "ru.letotam.ddmail"))
    }

    @Test
    fun deleteSendsKindAndToken() = runTest {
        server.enqueue(MockResponse().setBody("""{"deleted":true}"""))
        api.deletePushToken("hms", "tok-2")
        val req = server.takeRequest()
        assertEquals("DELETE", req.method)
        assertEquals("/api/desktop/v1/push/token", req.path)
        val o = Json.parseToJsonElement(req.body.readUtf8()).jsonObject
        assertEquals("hms", o["kind"]!!.jsonPrimitive.content)
        assertEquals("tok-2", o["token"]!!.jsonPrimitive.content)
    }
}
