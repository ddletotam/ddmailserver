package ru.letotam.ddmail.auth

import kotlinx.coroutines.delay
import kotlinx.coroutines.test.currentTime
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ServerDiscoveryTest {
    private fun discovery(
        alive: Map<String, Long>,
        mx: List<String> = emptyList(),
        mxDelay: Long = 10,
        deadDelay: Long = 1_000,
    ) = ServerDiscovery(
        probe = { host ->
            val d = alive[host]
            delay(d ?: deadDelay)
            d != null
        },
        mx = { delay(mxDelay); mx },
    )

    @Test
    fun mailSubdomainWinsAndDoesNotWaitForMx() = runTest {
        val seen = mutableListOf<DiscoveryProgress>()
        val url = discovery(mapOf("mail.example.org" to 50L), mxDelay = 5_000)
            .find("bob@example.org") { seen += it }
        assertEquals("https://mail.example.org", url)
        assertTrue("не ждали MX", currentTime < 1_000)
        assertEquals(ServerDiscovery.TOTAL, seen.last().done)
    }

    @Test
    fun laterCandidateWaitsForEarlierOnly() = runTest {
        // Сам домен отвечает быстро, но mail.<домен> стоит раньше: ждём его отказа.
        val url = discovery(mapOf("example.org" to 10L), deadDelay = 300).find("a@example.org") {}
        assertEquals("https://example.org", url)
        assertEquals(300L, currentTime)
    }

    @Test
    fun foundOnMxHost() = runTest {
        val url = discovery(
            mapOf("mx1.hosting.ru" to 20L),
            mx = listOf("mx1.hosting.ru", "mx2.hosting.ru", "mx3.hosting.ru"),
        ).find("a@corp.ru") {}
        assertEquals("https://mx1.hosting.ru", url)
    }

    @Test
    fun nothingFoundFillsProgress() = runTest {
        val seen = mutableListOf<DiscoveryProgress>()
        assertNull(discovery(emptyMap(), mx = listOf("mail.x.ru")).find("a@x.ru") { seen += it })
        assertEquals(ServerDiscovery.TOTAL, seen.last().done)
        assertTrue(seen.zipWithNext().all { (a, b) -> b.done >= a.done })
    }

    @Test
    fun notAnAddress() = runTest {
        assertNull(discovery(emptyMap()).find("bob") {})
    }
}
