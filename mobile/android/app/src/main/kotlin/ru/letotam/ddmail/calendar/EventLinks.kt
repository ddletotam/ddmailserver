package ru.letotam.ddmail.calendar

import ru.letotam.ddmail.api.EventDto

/**
 * Ссылки события (§5в десктопа, упрощённо): созвон лежит в LOCATION, в
 * CONFERENCE/X-…-CONFERENCE/URL или в описании, часто голым хостом без схемы.
 * Голый хост признаём, только когда он занимает поле целиком или начинается
 * с `www.` — иначе `report.docx` и адреса почты стали бы ссылками.
 */
object EventLinks {
    private val URL = Regex("""https?://[^\s<>"']+""", RegexOption.IGNORE_CASE)
    private val BARE = Regex("""^(www\.)?[a-z0-9-]+(\.[a-z0-9-]+)*\.[a-z]{2,}(:\d+)?(/\S*)?$""", RegexOption.IGNORE_CASE)
    private val WWW = Regex("""\bwww\.[a-z0-9-]+(\.[a-z0-9-]+)+(/[^\s<>"']*)?""", RegexOption.IGNORE_CASE)
    private val FILE_EXT = setOf("doc", "docx", "xls", "xlsx", "pdf", "txt", "zip", "png", "jpg", "ppt", "pptx", "csv")

    fun trimTail(u: String): String = u.trimEnd('.', ',', ';', ':', ')', ']', '!', '?', '»', '"', '\'')

    /** Ссылки из текста: со схемой и `www.`. */
    fun inText(text: String): List<String> {
        val out = URL.findAll(text).map { trimTail(it.value) }.toMutableList()
        WWW.findAll(text).forEach { m ->
            val v = trimTail(m.value)
            if (out.none { it.contains(v, ignoreCase = true) }) out += "https://$v"
        }
        return out
    }

    /** Поле целиком — голый хост (`telemost.yandex.ru/j/123`)? Тогда ссылка с https. */
    fun bareField(value: String): String? {
        val v = value.trim()
        if (v.contains('@') || v.contains(' ') || !BARE.matches(v)) return null
        val tld = v.substringBefore('/').substringBefore(':').substringAfterLast('.').lowercase()
        if (tld in FILE_EXT) return null
        return "https://$v"
    }

    fun target(value: String): String? = when {
        URL.matches(value.trim()) -> trimTail(value.trim())
        else -> bareField(value) ?: inText(value).firstOrNull()
    }

    /** Все ссылки события без повторов (сравнение по нормализованному виду). */
    fun of(ev: EventDto): List<String> {
        val found = mutableListOf<String>()
        ev.extras.filter { it.name == "URL" || it.name.endsWith("CONFERENCE") }.forEach { e -> target(e.value)?.let(found::add) }
        target(ev.location)?.let(found::add)
        found += inText(ev.description)
        return found.distinctBy { it.lowercase().removePrefix("https://").removePrefix("http://").trimEnd('/') }
    }
}
