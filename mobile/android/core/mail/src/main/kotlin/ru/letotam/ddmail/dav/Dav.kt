package ru.letotam.ddmail.dav

import kotlinx.coroutines.suspendCancellableCoroutine
import okhttp3.Call
import okhttp3.Callback
import okhttp3.Credentials
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import org.w3c.dom.Element
import ru.letotam.ddmail.imap.MailException
import java.io.IOException
import java.net.URI
import javax.xml.parsers.DocumentBuilderFactory
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

/** Предусловие записи (`Precondition` десктопа). */
sealed interface Precondition {
    /** Только создать: `If-None-Match: *`. */
    data object IfNew : Precondition

    /** Только если не менялось: `If-Match`. */
    data class IfMatch(val etag: String) : Precondition

    data object None : Precondition
}

/** Ответ из multistatus: ресурс и его свойства (элементы `prop` со статусом 200). */
class DavResponse(val href: String, val props: List<Element>) {
    fun prop(local: String): Element? = props.firstOrNull { it.localName == local }
    fun text(local: String): String? = prop(local)?.textContent?.trim()?.takeIf { it.isNotEmpty() }

    /** href внутри свойства (`current-user-principal`, `calendar-home-set`). */
    fun hrefIn(local: String): String? = prop(local)?.let { p ->
        val nl = p.getElementsByTagNameNS("DAV:", "href")
        if (nl.length > 0) nl.item(0).textContent.trim() else null
    }

    /** `resourcetype` содержит элемент [local] (`calendar`, `addressbook`). */
    fun isType(local: String): Boolean {
        val rt = prop("resourcetype") ?: return false
        val nl = rt.childNodes
        return (0 until nl.length).any { (nl.item(it) as? Element)?.localName == local }
    }
}

/**
 * WebDAV-запросы CalDAV/CardDAV (порт `caldav_client.rs`/`carddav_client.rs`
 * десктопа): Basic-авторизация паролем учётки, ответы multistatus разбираются
 * DOM-парсером с пространствами имён, а не поиском подстрок. Ошибки —
 * [MailException]: 401 — «нужен вход», прочее — отказ действия или сеть.
 */
class DavClient(private val http: OkHttpClient, private val user: String, private val password: String) {
    private val auth = Credentials.basic(user, password, Charsets.UTF_8)

    suspend fun propfind(url: String, depth: Int, prop: String): List<DavResponse> {
        val body = """<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:CR="urn:ietf:params:xml:ns:carddav" xmlns:A="http://apple.com/ns/ical/"><D:prop>$prop</D:prop></D:propfind>"""
        return multistatus(send("PROPFIND", url, body, depth = depth).first)
    }

    suspend fun report(url: String, body: String): List<DavResponse> = multistatus(send("REPORT", url, body, depth = 1).first)

    /** GET: тело и ETag. */
    suspend fun get(url: String): Pair<String, String?> = send("GET", url, null)

    suspend fun put(url: String, body: String, contentType: String, pre: Precondition): String? =
        send("PUT", url, body, contentType = contentType, pre = pre).second

    suspend fun delete(url: String, ifMatch: String?) {
        try {
            send("DELETE", url, null, pre = ifMatch?.let { Precondition.IfMatch(it) } ?: Precondition.None)
        } catch (e: NotFound) {
            // Уже нет — то, чего и хотели.
        }
    }

    private class NotFound : MailException.Rejected("не найдено")

    private suspend fun send(
        method: String,
        url: String,
        body: String?,
        depth: Int? = null,
        contentType: String = "application/xml; charset=utf-8",
        pre: Precondition = Precondition.None,
    ): Pair<String, String?> {
        val rb = Request.Builder().url(url).header("Authorization", auth)
        depth?.let { rb.header("Depth", it.toString()) }
        when (pre) {
            Precondition.IfNew -> rb.header("If-None-Match", "*")
            is Precondition.IfMatch -> rb.header("If-Match", pre.etag)
            Precondition.None -> {}
        }
        rb.method(method, body?.toRequestBody(contentType.toMediaType()))
        val r = http.newCall(rb.build()).await()
        r.use {
            val text = it.body?.string().orEmpty()
            when {
                it.isSuccessful -> return text to it.header("ETag")
                it.code == 401 -> throw MailException.Auth("$method: неверный логин или пароль")
                it.code == 404 -> throw NotFound()
                it.code == 412 -> throw MailException.Rejected("Изменено на сервере — откройте заново и повторите")
                it.code == 403 -> throw MailException.Rejected("Сервер не разрешил изменение (только для чтения)")
                else -> throw MailException.Rejected("$method: сервер ответил ${it.code}")
            }
        }
    }

    companion object {
        /** Разбор multistatus: только свойства со статусом 200. */
        fun multistatus(xml: String): List<DavResponse> {
            val doc = try {
                val f = DocumentBuilderFactory.newInstance()
                f.isNamespaceAware = true
                runCatching { f.setFeature("http://apache.org/xml/features/disallow-doctype-decl", true) }
                f.newDocumentBuilder().parse(xml.byteInputStream())
            } catch (e: Exception) {
                throw MailException.Rejected("DAV: ответ не разобрался")
            }
            val out = mutableListOf<DavResponse>()
            val rs = doc.getElementsByTagNameNS("DAV:", "response")
            for (i in 0 until rs.length) {
                val r = rs.item(i) as Element
                val href = children(r, "href").firstOrNull()?.textContent?.trim() ?: continue
                val props = mutableListOf<Element>()
                for (ps in children(r, "propstat")) {
                    val status = children(ps, "status").firstOrNull()?.textContent.orEmpty()
                    if (status.isNotEmpty() && !status.contains(" 200")) continue
                    children(ps, "prop").forEach { p ->
                        val nl = p.childNodes
                        for (j in 0 until nl.length) (nl.item(j) as? Element)?.let(props::add)
                    }
                }
                out += DavResponse(href, props)
            }
            return out
        }

        private fun children(e: Element, local: String): List<Element> {
            val nl = e.childNodes
            return (0 until nl.length).mapNotNull { nl.item(it) as? Element }.filter { it.localName == local && it.namespaceURI == "DAV:" }
        }

        /** href относительно базы (`abs_url` десктопа, но через URI — с путями без `/`). */
        fun absUrl(base: String, href: String): String = try {
            URI(base).resolve(href.trim()).toString()
        } catch (e: Exception) {
            href
        }
    }
}

internal suspend fun Call.await(): Response = suspendCancellableCoroutine { cont ->
    cont.invokeOnCancellation { cancel() }
    enqueue(object : Callback {
        override fun onFailure(call: Call, e: IOException) {
            if (!cont.isCancelled) cont.resumeWithException(ru.letotam.ddmail.imap.Tls.networkError(e))
        }

        override fun onResponse(call: Call, response: Response) {
            cont.resume(response)
        }
    })
}
