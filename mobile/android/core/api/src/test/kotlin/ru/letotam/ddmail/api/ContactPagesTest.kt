package ru.letotam.ddmail.api

import kotlinx.coroutines.test.runTest
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.Dispatcher
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.RecordedRequest
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Книга страницами (0.0.40): до этого клиент брал одну страницу
 * `/contacts?limit=5000`, и книга в 15 000 карточек обрывалась на 5000.
 */
class ContactPagesTest {
    private val server = MockWebServer().apply { start() }
    private val http = OkHttpClient()

    private object Tokens : TokenSource {
        override suspend fun current() = "t"
        override suspend fun update(token: String) {}
    }

    @After
    fun stop() = server.shutdown()

    private fun api() = SessionApi(server.url("/").toString().trimEnd('/'), http, Tokens)

    private fun page(ids: IntRange) = ids.joinToString(",", "[", "]") { """{"id":$it,"full_name":"К$it"}""" }

    /** Сервер с `offset`, как `HandleDesktopContacts`: книга из [total] карточек, id с 1. */
    private fun book(total: Int) {
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse {
                val url = request.requestUrl!!
                if (url.encodedPath != "/api/desktop/v1/contacts") return MockResponse().setResponseCode(404)
                val limit = url.queryParameter("limit")!!.toInt()
                val offset = url.queryParameter("offset")?.toInt() ?: 0
                val from = offset + 1
                val to = minOf(offset + limit, total)
                return MockResponse().setBody(if (from > to) "[]" else page(from..to))
            }
        }
    }

    @Test
    fun wholeBookComesInPagesUntilShortOne() = runTest {
        val total = SessionApi.CONTACTS_PAGE * 3 + 7 // ~15 000 — книга владельца
        book(total)
        val got = api().contactPages()
        assertEquals(total, got.contacts.size)
        assertFalse(got.truncated)
        assertEquals((1..total).map { it.toLong() }, got.contacts.map { it.id })
        assertEquals(4, server.requestCount)
        val paths = List(4) { server.takeRequest().requestUrl!! }
        assertEquals(listOf("0", "5000", "10000", "15000"), paths.map { it.queryParameter("offset") })
        assertTrue(paths.all { it.queryParameter("limit") == "${SessionApi.CONTACTS_PAGE}" })
    }

    @Test
    fun exactMultipleOfPageEndsOnEmptyPage() = runTest {
        book(SessionApi.CONTACTS_PAGE * 2)
        val got = api().contacts()
        assertEquals(SessionApi.CONTACTS_PAGE * 2, got.size)
        assertEquals(3, server.requestCount)
    }

    @Test
    fun smallBookIsOneRequest() = runTest {
        book(3)
        assertEquals(listOf(1L, 2L, 3L), api().contacts().map { it.id })
        assertEquals(1, server.requestCount)
    }

    @Test
    fun limitStopsEarly() = runTest {
        book(SessionApi.CONTACTS_PAGE * 3)
        assertEquals(SessionApi.CONTACTS_PAGE + 10, api().contacts(SessionApi.CONTACTS_PAGE + 10).size)
        assertEquals(2, server.requestCount)
    }

    /** Сервер без `offset` отдаёт ту же первую страницу — не зацикливаемся и не дублируем. */
    @Test
    fun oldServerWithoutOffsetStopsOnRepeat() = runTest {
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest) = MockResponse().setBody(page(1..SessionApi.CONTACTS_PAGE))
        }
        val got = api().contactPages()
        assertEquals(SessionApi.CONTACTS_PAGE, got.contacts.size)
        assertEquals(SessionApi.CONTACTS_PAGE, got.contacts.map { it.id }.toSet().size)
        assertTrue(got.truncated)
        assertEquals(2, server.requestCount)
    }

    @Test
    fun errorOnLaterPageSurfaces() = runTest {
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse =
                if (request.requestUrl!!.queryParameter("offset") == "0") MockResponse().setBody(page(1..SessionApi.CONTACTS_PAGE))
                else MockResponse().setResponseCode(500).setBody("""{"error":"boom"}""")
        }
        try {
            api().contacts()
            org.junit.Assert.fail("неполная книга не должна выдаваться за целую")
        } catch (e: ApiException.Server) {
            assertEquals(500, e.code)
        }
    }

    @Test
    fun searchGoesToServerAndEncodesQuery() = runTest {
        server.enqueue(MockResponse().setBody("""[{"id":20001,"full_name":"Яяя Аудит"}]"""))
        assertEquals(listOf(20001L), api().searchContacts("аудит контакт", 100).map { it.id })
        val url = server.takeRequest().requestUrl!!
        assertEquals("/api/desktop/v1/contacts/search", url.encodedPath)
        assertEquals("аудит контакт", url.queryParameter("q"))
        assertEquals("100", url.queryParameter("limit"))
    }

    @Test
    fun createReturnsServerId() = runTest {
        server.enqueue(MockResponse().setResponseCode(201).setBody("""{"id":42,"uid":"u"}"""))
        server.enqueue(MockResponse().setResponseCode(201).setBody(""))
        assertEquals(42L, api().createContact(ContactWriteRequest("X", emptyList(), emptyList(), addressBookId = 1)))
        // Старый ответ без id — не ошибка, просто без мгновенного показа.
        assertEquals(null, api().createContact(ContactWriteRequest("Y", emptyList(), emptyList(), addressBookId = 1)))
    }
}
