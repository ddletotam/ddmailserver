package ru.letotam.ddmail.sync

import kotlinx.coroutines.async
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.Dispatcher
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.RecordedRequest
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import ru.letotam.ddmail.api.SessionApi
import ru.letotam.ddmail.api.TokenSource
import java.util.Collections
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

/**
 * Отправленное письмо — в ленту за один запрос дельты (0.0.42): после
 * `/send` нужен только шаг бесед, и он не ждёт хвоста прохода (спам, адреса,
 * папки), который идёт секунды. Раньше «Отправляется…» висело 10–20 с.
 */
class PostSendRefreshTest {
    private object Tokens : TokenSource {
        override suspend fun current() = "t"
        override suspend fun update(token: String) {}
    }

    private val paths: MutableList<String> = Collections.synchronizedList(mutableListOf())

    /** Хвост прохода (спам) стоит, пока тест его не отпустит. */
    private val releaseSpam = CountDownLatch(1)

    private val server = MockWebServer().apply {
        dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse {
                val p = request.path.orEmpty().removePrefix("/api/desktop/v1")
                paths += p
                return when {
                    p.startsWith("/changes") -> json("""{"entries":[],"latest_seq":5}""")
                    p.startsWith("/conversations?spam=1") -> {
                        releaseSpam.await(10, TimeUnit.SECONDS)
                        json("[]")
                    }
                    p.startsWith("/conversations") -> json("""{"server_now_ms":1000,"conversations":[]}""")
                    p.startsWith("/identities") -> json("[]")
                    p.startsWith("/identity-groups") -> json("""{"version":1}""")
                    p.startsWith("/dialog-folders") -> json("""{"version":1}""")
                    else -> MockResponse().setResponseCode(404)
                }
            }
        }
        start()
    }

    private fun json(s: String) = MockResponse().setHeader("Content-Type", "application/json").setBody(s)

    private val engine = SyncEngine(
        "ddmail",
        SessionApi(server.url("/").toString().trimEnd('/'), OkHttpClient(), Tokens),
        FakeCacheDao(),
    )

    @After
    fun stop() {
        releaseSpam.countDown()
        server.shutdown()
    }

    @Test
    fun refreshAfterSendAsksOnlyForConversations() = runBlocking {
        releaseSpam.countDown()
        engine.syncConversations()
        assertEquals(listOf("/changes", "/conversations"), paths.map { it.substringBefore('?') })
        assertTrue(paths[1], paths[1].contains("since="))
    }

    @Test
    fun fullPassStillFetchesSpamIdentitiesAndFolders() = runBlocking {
        releaseSpam.countDown()
        engine.sync()
        val asked = paths.map { it.substringBefore('?') }
        assertTrue(asked.toString(), asked.containsAll(listOf("/changes", "/conversations", "/identities", "/identity-groups", "/dialog-folders")))
        assertTrue(paths.toString(), paths.any { it.startsWith("/conversations?spam=1") })
    }

    /** Полный проход застрял в хвосте — беседы после отправки его не ждут. */
    @Test
    fun conversationsDoNotWaitForTheTailOfAFullPass() = runBlocking {
        val full = async(kotlinx.coroutines.Dispatchers.IO) { engine.sync() }
        // Полный проход дошёл до спама и стоит.
        withTimeout(5_000) { while (paths.none { it.startsWith("/conversations?spam=1") }) kotlinx.coroutines.delay(10) }
        val before = paths.size
        withTimeout(5_000) { engine.syncConversations() }
        val after = paths.drop(before).map { it.substringBefore('?') }
        assertEquals(listOf("/changes", "/conversations"), after)
        releaseSpam.countDown()
        full.await()
    }
}
