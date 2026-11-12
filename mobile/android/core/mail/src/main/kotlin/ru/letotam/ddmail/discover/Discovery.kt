package ru.letotam.ddmail.discover

import kotlinx.coroutines.Deferred
import kotlinx.coroutines.async
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.selects.select
import kotlinx.coroutines.withTimeoutOrNull
import ru.letotam.ddmail.api.emailDomain
import ru.letotam.ddmail.imap.Security
import ru.letotam.ddmail.imap.imapSecurity
import ru.letotam.ddmail.imap.smtpSecurity
import java.util.concurrent.atomic.AtomicInteger
import javax.xml.parsers.DocumentBuilderFactory

data class Endpoint(val host: String, val port: Int)

/** Известная служба (таблица [MailPresets]) — первой в очереди кандидатов. */
data class Hint(val imapHost: String, val smtpHost: String, val smtpPort: Int)

/** Что нашлось. Всё `null` — не нашлось ничего, пусть вводит руками. */
data class Found(
    /** `https://<хост>` сервера ddmail — остальное тогда не ищется. */
    val ddmailUrl: String? = null,
    val imap: Endpoint? = null,
    val smtp: Endpoint? = null,
    /** Исходящий объявил AUTH. Нет — внутренний релей без авторизации. */
    val smtpAuth: Boolean = false,
    /** Логин из автонастройки, если он не email. */
    val login: String? = null,
    /** Название службы из автонастройки. */
    val provider: String? = null,
)

data class Progress(val done: Int, val total: Int, val stage: String)

/** Разбор Thunderbird autoconfig: серверы, логин, название. */
data class Autoconf(val imap: Endpoint? = null, val smtp: Endpoint? = null, val login: String? = null, val name: String? = null)

/**
 * Автонастройка по адресу почты — порт `desktop/core/src/discover.rs`
 * (контракт mobile-external-accounts §2). По фазам; внутри фазы проверки идут
 * разом, а победитель — первый успешный **по порядку доверия**:
 *
 * 1. MX домена, ddmail на `mail.<домен>` и `<домен>`, autoconfig (домен,
 *    его `.well-known`, ISPDB Thunderbird);
 * 2. ddmail на MX-хостах — нашёлся, дальше не ищем;
 * 3. IMAP: 993 (TLS) и 143 (STARTTLS);
 * 4. SMTP: 465, 587, затем 25 (не на MX).
 *
 * Пароль не участвует. Открытый текст не принимается. Отмена — отменой
 * корутины, проверяется между фазами. Прогресс — от верхней границы числа
 * проверок; несостоявшиеся засчитываются сразу.
 *
 * Сеть внедряется ([Net]) — тесты подменяют её.
 */
class Discovery(private val net: Net) {
    interface Net {
        suspend fun mx(domain: String): List<String>
        suspend fun isDdmail(host: String): Boolean
        /** GET по https: тело при 2xx, иначе `null`. */
        suspend fun get(url: String): String?
        /** Живой IMAP с рабочим TLS. */
        suspend fun probeImap(host: String, port: Int): Boolean
        /** Живой SMTP с TLS: `true`/`false` — объявлен ли AUTH; `null` — нет. */
        suspend fun probeSmtp(host: String, port: Int): Boolean?
    }

    private class Meter(val cb: (Progress) -> Unit) {
        val done = AtomicInteger(0)
        @Volatile var stage = ""
        fun emit() = cb(Progress(minOf(done.get(), TOTAL), TOTAL, stage))
        fun stage(s: String) {
            stage = s
            emit()
        }
        fun tick(n: Int) {
            if (n > 0) {
                done.addAndGet(n)
                emit()
            }
        }
        fun finish() {
            done.set(TOTAL)
            emit()
        }
    }

    suspend fun discover(email: String, hint: Hint?, onProgress: (Progress) -> Unit): Found = coroutineScope {
        val meter = Meter(onProgress)
        val domain = emailDomain(email) ?: run {
            meter.finish()
            return@coroutineScope Found()
        }
        val addr = email.trim()

        // ── 1. MX, ddmail на домене, автонастройка ──
        meter.stage("Ищем сервер для $domain…")
        val direct = listOf("mail.$domain", domain)
        val mxJob = async { withTimeoutOrNull(PROBE_MS) { net.mx(domain) }.orEmpty().filter { it.isNotEmpty() }.distinct().take(MAX_MX).also { meter.tick(1) } }
        val autoJob = async { autoconfig(addr, domain, meter) }
        val dd = firstInOrder(direct.map { h -> suspend { if (probe { net.isDdmail(h) } == true) Unit else null } }, meter)
        if (dd != null) {
            mxJob.cancel()
            autoJob.cancel()
            meter.finish()
            return@coroutineScope Found(ddmailUrl = "https://${direct[dd.first]}")
        }
        val mx = mxJob.await()
        val auto = autoJob.await()
        ensureActive()

        // ── 2. ddmail на MX ──
        val mxNew = mx.filter { it !in direct }
        meter.tick(MAX_MX - mxNew.size)
        if (mxNew.isNotEmpty()) {
            meter.stage("Проверяем почтовые серверы домена…")
            val hit = firstInOrder(mxNew.map { h -> suspend { if (probe { net.isDdmail(h) } == true) Unit else null } }, meter)
            if (hit != null) {
                meter.finish()
                return@coroutineScope Found(ddmailUrl = "https://${mxNew[hit.first]}")
            }
        }
        ensureActive()

        // ── 3. IMAP ──
        meter.stage("Ищем входящий сервер (IMAP)…")
        val imapC = imapCandidates(domain, hint, auto, mx)
        meter.tick(MAX_IMAP - imapC.size)
        val imapHit = firstInOrder(imapC.map { e -> suspend { if (probe { net.probeImap(e.host, e.port) } == true) Unit else null } }, meter)
        val imap = imapHit?.let { imapC[it.first] }
        ensureActive()

        // ── 4. SMTP ──
        meter.stage("Ищем исходящий сервер (SMTP)…")
        val smtpC = smtpCandidates(domain, hint, auto, mx, imap)
        meter.tick(MAX_SMTP - smtpC.size)
        val smtpHit = firstInOrder(smtpC.map { e -> suspend { probe { net.probeSmtp(e.host, e.port) } } }, meter)
        meter.finish()
        Found(
            imap = imap,
            smtp = smtpHit?.let { smtpC[it.first] },
            smtpAuth = smtpHit?.second ?: false,
            login = auto?.login,
            provider = auto?.name,
        )
    }

    private suspend fun <T> probe(block: suspend () -> T?): T? =
        try {
            withTimeoutOrNull(PROBE_MS) { block() }
        } catch (e: kotlinx.coroutines.CancellationException) {
            throw e
        } catch (e: Exception) {
            null
        }

    private suspend fun autoconfig(email: String, domain: String, meter: Meter): Autoconf? {
        val enc = java.net.URLEncoder.encode(email, "UTF-8")
        val urls = listOf(
            "https://autoconfig.$domain/mail/config-v1.1.xml?emailaddress=$enc",
            "https://$domain/.well-known/autoconfig/mail/config-v1.1.xml",
            "https://autoconfig.thunderbird.net/v1.1/$domain",
        )
        return firstInOrder(
            urls.map { u ->
                suspend {
                    probe { net.get(u) }?.let { parseAutoconfig(it, email) }?.takeIf { it.imap != null || it.smtp != null }
                }
            },
            meter,
        )?.second
    }

    companion object {
        const val PROBE_MS = 8_000L
        const val CONNECT_MS = 3_000
        const val MAX_MX = 2
        const val MAX_IMAP = 16
        const val MAX_SMTP = 27
        const val AUTOCONFIG_URLS = 3
        const val TOTAL = 1 + 2 + AUTOCONFIG_URLS + MAX_MX + MAX_IMAP + MAX_SMTP

        /**
         * Проверки разом, победитель — первый успешный по порядку: ждём только
         * тех, кто раньше него, остальных бросаем (их шаги засчитываются сразу).
         */
        internal suspend fun <T : Any> firstInOrderRaw(jobs: List<suspend () -> T?>, tick: (Int) -> Unit): Pair<Int, T>? = coroutineScope {
            val n = jobs.size
            if (n == 0) return@coroutineScope null
            val deferred: List<Deferred<T?>> = jobs.map { j -> async { j() } }
            val results = arrayOfNulls<Any?>(n)
            val finished = BooleanArray(n)
            var finishedCount = 0
            try {
                while (finishedCount < n) {
                    val (i, r) = select<Pair<Int, T?>> {
                        deferred.forEachIndexed { i, d -> if (!finished[i]) d.onAwait { i to it } }
                    }
                    finished[i] = true
                    results[i] = r
                    finishedCount++
                    tick(1)
                    for (j in 0 until n) {
                        if (!finished[j]) break
                        @Suppress("UNCHECKED_CAST")
                        val v = results[j] as T?
                        if (v != null) {
                            tick(n - finishedCount)
                            return@coroutineScope j to v
                        }
                    }
                }
                null
            } finally {
                deferred.forEach { it.cancel() }
            }
        }

        private suspend fun <T : Any> firstInOrder(jobs: List<suspend () -> T?>, meter: Meter) = firstInOrderRaw(jobs, meter::tick)

        private fun MutableList<Endpoint>.pushUnique(e: Endpoint) {
            if (e.host.isNotEmpty() && e !in this) add(e)
        }

        /** Известная служба по MX: домен на Яндекс 360 / VK WorkSpace / Google — их серверы. */
        fun mxProvider(mx: String): Pair<String, String>? {
            val m = mx.lowercase()
            return when {
                m.endsWith(".yandex.net") || m.endsWith(".yandex.ru") -> "imap.yandex.ru" to "smtp.yandex.ru"
                m.endsWith(".mail.ru") -> "imap.mail.ru" to "smtp.mail.ru"
                m.endsWith(".google.com") || m.endsWith(".googlemail.com") -> "imap.gmail.com" to "smtp.gmail.com"
                else -> null
            }
        }

        fun imapCandidates(domain: String, hint: Hint?, auto: Autoconf?, mx: List<String>): List<Endpoint> {
            val v = mutableListOf<Endpoint>()
            auto?.imap?.let { v.pushUnique(it) }
            val hosts = mutableListOf<String>()
            hint?.let { hosts += it.imapHost }
            hosts += mx.mapNotNull { mxProvider(it)?.first }
            hosts += "imap.$domain"
            hosts += "mail.$domain"
            hosts += domain
            hosts += mx
            for (h in hosts) {
                v.pushUnique(Endpoint(h, 993))
                v.pushUnique(Endpoint(h, 143))
            }
            return v.take(MAX_IMAP)
        }

        /** 465 и 587 на всех хостах раньше любого 25; 25 на MX не пробуем вовсе. */
        fun smtpCandidates(domain: String, hint: Hint?, auto: Autoconf?, mx: List<String>, imap: Endpoint?): List<Endpoint> {
            val v = mutableListOf<Endpoint>()
            auto?.smtp?.let { v.pushUnique(it) }
            hint?.let { v.pushUnique(Endpoint(it.smtpHost, it.smtpPort)) }
            val hosts = mutableListOf<String>()
            hosts += mx.mapNotNull { mxProvider(it)?.second }
            hosts += "smtp.$domain"
            imap?.let { hosts += it.host }
            hosts += "mail.$domain"
            hosts += domain
            hosts += mx
            val uniq = hosts.distinct()
            for (h in uniq) {
                v.pushUnique(Endpoint(h, 465))
                v.pushUnique(Endpoint(h, 587))
            }
            for (h in uniq.filter { it !in mx }) v.pushUnique(Endpoint(h, 25))
            return v.take(MAX_SMTP)
        }

        private fun expand(tpl: String, email: String): String {
            val local = email.substringBeforeLast('@', email)
            val domain = email.substringAfterLast('@', "")
            return tpl.replace("%EMAILADDRESS%", email).replace("%EMAILLOCALPART%", local).replace("%EMAILDOMAIN%", domain)
        }

        /**
         * Сервер годится, если его шифрование совпадает с тем, что выберет
         * подключение по порту; `socketType=plain` отвергается.
         */
        fun parseAutoconfig(xml: String, email: String): Autoconf? {
            val doc = try {
                val f = DocumentBuilderFactory.newInstance()
                runCatching { f.setFeature("http://apache.org/xml/features/disallow-doctype-decl", true) }
                runCatching { f.setFeature("http://xml.org/sax/features/external-general-entities", false) }
                runCatching { f.isExpandEntityReferences = false }
                f.newDocumentBuilder().parse(xml.byteInputStream())
            } catch (e: Exception) {
                return null
            }
            fun elements(tag: String): List<org.w3c.dom.Element> {
                val nl = doc.getElementsByTagName(tag)
                return (0 until nl.length).mapNotNull { nl.item(it) as? org.w3c.dom.Element }
            }
            fun child(e: org.w3c.dom.Element, tag: String): String? {
                val nl = e.childNodes
                for (i in 0 until nl.length) {
                    val c = nl.item(i)
                    if (c is org.w3c.dom.Element && c.tagName == tag) return c.textContent?.trim()
                }
                return null
            }
            fun pick(tag: String, kind: String, rule: (Int) -> Security): Pair<Endpoint, String?>? =
                elements(tag).filter { it.getAttribute("type") == kind }.firstNotNullOfOrNull { n ->
                    val host = expand(child(n, "hostname") ?: return@firstNotNullOfOrNull null, email).lowercase()
                    val port = child(n, "port")?.toIntOrNull() ?: return@firstNotNullOfOrNull null
                    val sec = when (child(n, "socketType")?.uppercase()) {
                        "SSL", "TLS" -> Security.Tls
                        "STARTTLS" -> Security.StartTls
                        else -> return@firstNotNullOfOrNull null
                    }
                    if (rule(port) == sec && host.isNotEmpty()) Endpoint(host, port) to child(n, "username") else null
                }
            val imap = pick("incomingServer", "imap", ::imapSecurity)
            val smtp = pick("outgoingServer", "smtp", ::smtpSecurity)
            val login = imap?.second?.let { expand(it, email) }?.takeIf { it.isNotEmpty() && it != email }
            val name = elements("*").firstOrNull { it.tagName == "displayShortName" || it.tagName == "displayName" }
                ?.textContent?.trim()?.takeIf { it.isNotEmpty() }
            return Autoconf(imap?.first, smtp?.first, login, name)
        }
    }
}
