package ru.letotam.ddmail.api

import kotlinx.coroutines.test.runTest
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Before
import org.junit.Test

class DdmailApiTest {
    private lateinit var server: MockWebServer
    private val http = OkHttpClient()

    @Before
    fun start() {
        server = MockWebServer().apply { start() }
    }

    @After
    fun stop() = server.shutdown()

    private fun api() = DdmailApi(server.url("/").toString().trimEnd('/'), http)

    @Test
    fun loginSendsCredentialsAndParsesToken() = runTest {
        server.enqueue(MockResponse().setBody("""{"token":"t1","user_id":7,"username":"lucky","extra":1}"""))
        val r = api().login("lucky", "secret")
        assertEquals(LoginResponse("t1", 7, "lucky"), r)
        val req = server.takeRequest()
        assertEquals("/api/desktop/v1/auth/login", req.path)
        val body = req.body.readUtf8()
        assertTrue(body, body.contains("\"username\":\"lucky\"") && body.contains("\"password\":\"secret\""))
    }

    @Test
    fun wrongPasswordIsUnauthorized() = runTest {
        server.enqueue(MockResponse().setResponseCode(401).setBody("""{"error":"invalid credentials"}"""))
        try {
            api().login("lucky", "bad")
            fail("должно было упасть")
        } catch (e: ApiException.Unauthorized) {
            assertEquals("invalid credentials", e.serverMessage)
        }
    }

    @Test
    fun serverErrorIsNotUnauthorized() = runTest {
        server.enqueue(MockResponse().setResponseCode(500).setBody("boom"))
        try {
            api().refresh("t")
            fail("должно было упасть")
        } catch (e: ApiException.Server) {
            assertEquals(500, e.code)
        }
        assertEquals("Bearer t", server.takeRequest().getHeader("Authorization"))
    }
}

class SessionApiTest {
    private val server = MockWebServer().apply { start() }
    private val http = OkHttpClient()

    private class Tokens(var t: String) : TokenSource {
        override suspend fun current() = t
        override suspend fun update(token: String) { t = token }
    }

    @After
    fun stop() = server.shutdown()

    @Test
    fun expiredTokenIsRefreshedOnceAndRetried() = runTest {
        server.enqueue(MockResponse().setResponseCode(401).setBody("""{"error":"invalid token"}"""))
        server.enqueue(MockResponse().setBody("""{"token":"new","user_id":1,"username":"u"}"""))
        server.enqueue(MockResponse().setBody("""{"server_now_ms":5,"conversations":[{"id":"a|b","messages":null,"counterparts":null}]}"""))
        val tokens = Tokens("old")
        val api = SessionApi(server.url("/").toString().trimEnd('/'), http, tokens)
        val d = api.conversationsDelta(0)
        assertEquals(5L, d.serverNowMs)
        assertEquals(emptyList<MessageRef>(), d.conversations.single().messages)
        assertEquals("new", tokens.t)
        assertEquals("Bearer old", server.takeRequest().getHeader("Authorization"))
        assertEquals("/api/desktop/v1/auth/refresh", server.takeRequest().path)
        assertEquals("Bearer new", server.takeRequest().getHeader("Authorization"))
    }

    @Test
    fun deadSessionSurfacesUnauthorized() = runTest {
        server.enqueue(MockResponse().setResponseCode(401))
        server.enqueue(MockResponse().setResponseCode(401).setBody("""{"error":"token too old to refresh, please log in again"}"""))
        val api = SessionApi(server.url("/").toString().trimEnd('/'), http, Tokens("old"))
        try {
            api.changes(10)
            fail("должно было упасть")
        } catch (e: ApiException.Unauthorized) {
            assertTrue(e.serverMessage.contains("too old"))
        }
    }
}
