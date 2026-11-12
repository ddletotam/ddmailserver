package ru.letotam.ddmail.discover

import kotlinx.coroutines.delay
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import ru.letotam.ddmail.api.emailDomain
import ru.letotam.ddmail.imap.Security
import ru.letotam.ddmail.imap.imapSecurity
import ru.letotam.ddmail.imap.smtpSecurity

/** Порт тестов `discover.rs` и `mail_presets.rs` десктопа. */
class DiscoveryTest {
    @Test
    fun domainOfAddress() {
        assertEquals("example.org", emailDomain(" Me@Example.ORG "))
        assertEquals("xn--80a1acny.xn--p1ai", emailDomain("a@почта.рф"))
        for (bad in listOf("", "a", "a@", "@x.ru", "a@localhost")) assertNull(bad, emailDomain(bad))
    }

    @Test
    fun securityFollowsPort() {
        assertEquals(Security.StartTls, imapSecurity(143))
        assertEquals(Security.Tls, imapSecurity(993))
        assertEquals(Security.StartTls, smtpSecurity(587))
        assertEquals(Security.StartTls, smtpSecurity(25))
        assertEquals(Security.Tls, smtpSecurity(465))
    }

    private val xml = """<?xml version="1.0"?>
<clientConfig version="1.1">
  <emailProvider id="example.org">
    <domain>example.org</domain>
    <displayName>Example Mail</displayName>
    <incomingServer type="pop3">
      <hostname>pop.example.org</hostname><port>995</port><socketType>SSL</socketType>
    </incomingServer>
    <incomingServer type="imap">
      <hostname>plain.example.org</hostname><port>143</port><socketType>plain</socketType>
    </incomingServer>
    <incomingServer type="imap">
      <hostname>imap.%EMAILDOMAIN%</hostname><port>993</port><socketType>SSL</socketType>
      <username>%EMAILLOCALPART%</username>
    </incomingServer>
    <outgoingServer type="smtp">
      <hostname>smtp.example.org</hostname><port>587</port><socketType>STARTTLS</socketType>
      <username>%EMAILADDRESS%</username>
    </outgoingServer>
  </emailProvider>
</clientConfig>"""

    @Test
    fun autoconfigTakesEncryptedImapAndExpandsPlaceholders() {
        val a = Discovery.parseAutoconfig(xml, "bob@example.org")!!
        assertEquals(Endpoint("imap.example.org", 993), a.imap)
        assertEquals(Endpoint("smtp.example.org", 587), a.smtp)
        assertEquals("bob", a.login)
        assertEquals("Example Mail", a.name)
    }

    @Test
    fun autoconfigRejectsSecurityThatPortWouldNotUse() {
        val bad = xml.replace("<port>587</port><socketType>STARTTLS", "<port>587</port><socketType>SSL")
        assertNull(Discovery.parseAutoconfig(bad, "bob@example.org")!!.smtp)
        assertNull(Discovery.parseAutoconfig("<not xml", "a@b.c"))
    }

    @Test
    fun imapCandidatesPutKnownFirstAndCap() {
        val auto = Autoconf(imap = Endpoint("in.example.org", 993))
        val mx = listOf("mx.yandex.net", "mx2.example.org")
        val v = Discovery.imapCandidates("example.org", null, auto, mx)
        assertEquals(Endpoint("in.example.org", 993), v[0])
        assertEquals(Endpoint("imap.yandex.ru", 993), v[1])
        assertEquals(Endpoint("imap.yandex.ru", 143), v[2])
        assertEquals(Endpoint("imap.example.org", 993), v[3])
        assertTrue(Endpoint("mx2.example.org", 143) in v)
        assertTrue(v.size <= Discovery.MAX_IMAP)
    }

    @Test
    fun smtpPort25ComesLastAndNeverOnMx() {
        val mx = listOf("mail.example.org")
        val v = Discovery.smtpCandidates("example.org", null, null, mx, null)
        val first25 = v.indexOfFirst { it.port == 25 }
        assertTrue(v.subList(0, first25).all { it.port != 25 })
        assertTrue(v.subList(first25, v.size).all { it.port == 25 })
        assertTrue(Endpoint("mail.example.org", 25) !in v)
        assertTrue(Endpoint("mail.example.org", 465) in v)
        assertTrue(v.size <= Discovery.MAX_SMTP)
    }

    @Test
    fun worstCaseCandidatesFitTheBudget() {
        val hint = Hint("i.x.ru", "s.x.ru", 465)
        val auto = Autoconf(imap = Endpoint("a.x.ru", 993), smtp = Endpoint("b.x.ru", 587))
        val mx = listOf("mx.yandex.net", "aspmx.l.google.com")
        val imap = Discovery.imapCandidates("x.ru", hint, auto, mx)
        val smtp = Discovery.smtpCandidates("x.ru", hint, auto, mx, Endpoint("q.x.ru", 993))
        assertTrue(imap.size <= Discovery.MAX_IMAP && smtp.size <= Discovery.MAX_SMTP)
        assertEquals("a.x.ru", imap[0].host)
        assertEquals("b.x.ru", smtp[0].host)
        assertEquals("s.x.ru", smtp[1].host)
    }

    @Test
    fun firstInOrderWaitsOnlyForEarlierCandidates() = runBlocking {
        var ticks = 0
        fun slow(ms: Long, ok: Boolean): suspend () -> Long? = {
            delay(ms)
            if (ok) ms else null
        }
        val t = System.currentTimeMillis()
        val r = Discovery.firstInOrderRaw(listOf(slow(50, false), slow(10, true), slow(5_000, true))) { ticks += it }
        assertEquals(1 to 10L, r)
        assertTrue(System.currentTimeMillis() - t < 1000)
        assertEquals(3, ticks)
        assertEquals(0 to 30L, Discovery.firstInOrderRaw(listOf(slow(30, true), slow(1, true))) {})
    }

    @Test
    fun fullDiscoveryFindsImapAndSmtp() = runBlocking {
        val net = object : Discovery.Net {
            override suspend fun mx(domain: String) = listOf("mx.example.org")
            override suspend fun isDdmail(host: String) = false
            override suspend fun get(url: String): String? = null
            override suspend fun probeImap(host: String, port: Int) = host == "imap.example.org" && port == 993
            override suspend fun probeSmtp(host: String, port: Int): Boolean? = if (host == "smtp.example.org" && port == 587) true else null
        }
        var last: Progress? = null
        val f = Discovery(net).discover("bob@example.org", null) { last = it }
        assertEquals(Endpoint("imap.example.org", 993), f.imap)
        assertEquals(Endpoint("smtp.example.org", 587), f.smtp)
        assertTrue(f.smtpAuth)
        assertEquals(last!!.total, last!!.done)
    }

    @Test
    fun ddmailOnDomainStopsSearch() = runBlocking {
        val net = object : Discovery.Net {
            override suspend fun mx(domain: String): List<String> {
                delay(5_000)
                return emptyList()
            }
            override suspend fun isDdmail(host: String) = host == "mail.example.org"
            override suspend fun get(url: String): String? {
                delay(5_000)
                return null
            }
            override suspend fun probeImap(host: String, port: Int) = error("не должно")
            override suspend fun probeSmtp(host: String, port: Int): Boolean? = error("не должно")
        }
        val f = Discovery(net).discover("bob@example.org", null) {}
        assertEquals("https://mail.example.org", f.ddmailUrl)
    }

    @Test
    fun presets() {
        val s = MailPresets.guess("someone@bk.ru")!!
        assertEquals("imap.mail.ru", s.imapHost)
        assertEquals("smtp.mail.ru", s.smtpHost)
        assertEquals(993 to 465, s.imapPort to s.smtpPort)
        assertEquals("Mail.ru", s.provider)
        assertTrue(s.passwordHint.isNotEmpty())
        assertEquals("imap.gmx.net", MailPresets.guess("  Someone@GMX.NET ")!!.imapHost)
        assertEquals(587, MailPresets.guess("a@icloud.com")!!.smtpPort)
        val g = MailPresets.guess("a@example.org")!!
        assertEquals("imap.example.org", g.imapHost)
        assertNull(g.provider)
        for (e in listOf("", "user", "user@", "user@mail", "user@mail.r", "user@.ru")) assertNull(e, MailPresets.guess(e))
    }

    @Test
    fun smtpMultilineReplyAndCaps() {
        val r = "250-mx.example.org\r\n250-AUTH PLAIN\r\n250 SIZE 100\r\n".byteInputStream().bufferedReader()
        assertEquals(250 to listOf("mx.example.org", "AUTH PLAIN", "SIZE 100"), Probes.smtpReply(r))
        assertTrue(Probes.smtpHasAuth(listOf("mx.example.org", "AUTH PLAIN LOGIN")))
        assertTrue(!Probes.smtpHasAuth(listOf("mx.example.org", "STARTTLS")))
    }
}
