package ru.letotam.ddmail.api

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ServerUrlTest {
    @Test
    fun schemeIsAddedAndTailDropped() {
        assertEquals("https://mail.example.ru", normalizeServerUrl(" mail.example.ru/ ").getOrThrow())
        assertEquals("https://mail.example.ru", normalizeServerUrl("HTTPS://Mail.Example.RU/path").getOrThrow())
        assertEquals("https://mail.example.ru:8443", normalizeServerUrl("mail.example.ru:8443").getOrThrow())
        assertEquals("https://mail.example.ru", normalizeServerUrl("https://mail.example.ru:443").getOrThrow())
    }

    @Test
    fun httpIsRefused() {
        assertTrue(normalizeServerUrl("http://mail.example.ru").isFailure)
    }

    @Test
    fun junkIsRefused() {
        for (bad in listOf("", "   ", "localhost", "https://")) {
            assertTrue(bad, normalizeServerUrl(bad).isFailure)
        }
    }

    @Test
    fun idnBecomesPunycode() {
        assertEquals("https://xn--80a1acny.xn--p1ai", normalizeServerUrl("почта.рф").getOrThrow())
        assertEquals("xn--80a1acny.xn--p1ai", emailDomain("Я@Почта.РФ"))
    }

    @Test
    fun emailDomainNeedsRealAddress() {
        assertEquals("example.org", emailDomain(" Bob@Example.ORG "))
        for (bad in listOf("", "bob", "bob@", "@x.ru", "bob@localhost", "bob@.ru")) {
            assertNull(bad, emailDomain(bad))
        }
    }
}
