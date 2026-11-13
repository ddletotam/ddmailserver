package ru.letotam.ddmail.compose

/**
 * Тело письма из композера — порт сериализации `desktop/native/src/richtext.rs`
 * (десктопный контракт §3а). Редактор (`RichEditText`) отдаёт содержимое
 * кусками [RichSeg]; здесь из них получаются обе версии тела:
 *
 * - `text/plain` — абзацы через `\n`, картинка — пустой абзац (`plain_text`);
 * - `text/html` — `<div>`-абзацы, `<b>/<i>/<u>/<a>`, пустой абзац —
 *   `<div><br></div>`, картинка — свой блок `<img src="cid:…">` (`html`);
 *   URL в тексте становятся ссылками (автоссылки).
 *
 * Чистый Kotlin без Android — покрыт тестами без устройства.
 */
sealed interface RichSeg {
    /** Текст одного стиля; может содержать `\n` — границы абзацев. */
    data class Text(
        val text: String,
        val bold: Boolean = false,
        val italic: Boolean = false,
        val underline: Boolean = false,
        val link: String? = null,
    ) : RichSeg

    /** Картинка — всегда свой блок; [cid] — Content-ID inline-вложения. */
    data class Image(val cid: String) : RichSeg
}

private sealed interface Block {
    class Para(val runs: MutableList<RichSeg.Text> = mutableListOf()) : Block
    class Img(val cid: String) : Block
}

object RichBody {
    /**
     * Куски → блоки. Картинка в редакторе стоит своей строкой
     * (`…\n￼\n…`): перевод строки сразу после неё — граница блока, а не
     * пустой абзац; пустой абзац перед ней тоже не нужен.
     */
    private fun blocks(segs: List<RichSeg>): List<Block> {
        val out = mutableListOf<Block>()
        var cur = Block.Para()
        var afterImage = false
        for (s in segs) {
            when (s) {
                is RichSeg.Image -> {
                    if (cur.runs.any { it.text.isNotEmpty() }) out += cur
                    out += Block.Img(s.cid)
                    cur = Block.Para()
                    afterImage = true
                }
                is RichSeg.Text -> {
                    val parts = s.text.replace("\r\n", "\n").replace('\r', '\n').split('\n')
                    parts.forEachIndexed { i, part ->
                        if (i > 0) {
                            // Перевод строки сразу за картинкой — граница её блока.
                            if (afterImage && cur.runs.all { it.text.isEmpty() }) {
                                afterImage = false
                            } else {
                                out += cur
                                cur = Block.Para()
                            }
                        }
                        if (part.isNotEmpty()) {
                            cur.runs += s.copy(text = part)
                            afterImage = false
                        }
                    }
                }
            }
        }
        // Пустая строка сразу за картинкой в конце (там стоит каретка после
        // вставки) — не абзац; пустой документ — один пустой абзац.
        if (!(cur.runs.isEmpty() && out.lastOrNull() is Block.Img)) out += cur
        return out
    }

    /** `text/plain`: абзацы через `\n`, картинка — пустая строка. */
    fun plain(segs: List<RichSeg>): String = blocks(segs).joinToString("\n") { b ->
        when (b) {
            is Block.Para -> b.runs.joinToString("") { it.text }
            is Block.Img -> ""
        }
    }

    /** `text/html` — фрагмент без `<html>`, как `Editor::html` десктопа. */
    fun html(segs: List<RichSeg>): String {
        val out = StringBuilder()
        for (b in blocks(segs)) {
            when (b) {
                is Block.Img -> out.append("<div><img src=\"cid:").append(escape(b.cid)).append("\" style=\"max-width:100%\"></div>")
                is Block.Para -> {
                    if (b.runs.all { it.text.isEmpty() }) {
                        out.append("<div><br></div>")
                        continue
                    }
                    out.append("<div>")
                    for (r in b.runs) {
                        if (r.text.isEmpty()) continue
                        val open = StringBuilder()
                        val close = StringBuilder()
                        r.link?.let { href ->
                            open.append("<a href=\"").append(escape(safeHref(href))).append("\">")
                            close.insert(0, "</a>")
                        }
                        if (r.bold) { open.append("<b>"); close.insert(0, "</b>") }
                        if (r.italic) { open.append("<i>"); close.insert(0, "</i>") }
                        if (r.underline) { open.append("<u>"); close.insert(0, "</u>") }
                        out.append(open).append(runInner(r)).append(close)
                    }
                    out.append("</div>")
                }
            }
        }
        return out.toString()
    }

    /**
     * Адрес ссылки: http(s) и mailto — как есть, без схемы — `http://`.
     * Остальные схемы (`javascript:` и т.п.) в письмо не уходят.
     */
    fun safeHref(raw: String): String {
        val t = raw.trim()
        val lower = t.lowercase()
        return when {
            lower.startsWith("http://") || lower.startsWith("https://") || lower.startsWith("mailto:") -> t
            Regex("^[a-z][a-z0-9+.-]*:").containsMatchIn(lower) -> "http://" + t.substringAfter(':').trimStart('/')
            '@' in t && ' ' !in t && '/' !in t -> "mailto:$t"
            else -> "http://$t"
        }
    }

    /** Текст прогона: экранирование + автоссылки; уже-ссылка второй раз не оборачивается. */
    private fun runInner(r: RichSeg.Text): String {
        if (r.link != null) return escape(r.text)
        val out = StringBuilder()
        var rest = r.text
        while (true) {
            val (s, e) = findUrl(rest) ?: break
            out.append(escape(rest.substring(0, s)))
            val url = rest.substring(s, e)
            val href = if (url.startsWith("www.")) "http://$url" else url
            out.append("<a href=\"").append(escape(href)).append("\">").append(escape(url)).append("</a>")
            rest = rest.substring(e)
        }
        out.append(escape(rest))
        return out.toString()
    }

    /** Первый URL в тексте — как `find_url` десктопа: не с середины слова, без хвостовой пунктуации. */
    internal fun findUrl(text: String): Pair<Int, Int>? {
        var best: Pair<Int, Int>? = null
        for (p in listOf("https://", "http://", "www.")) {
            var from = 0
            while (true) {
                val start = text.indexOf(p, from)
                if (start < 0) break
                val ok = start == 0 || !text[start - 1].isLetterOrDigit()
                if (ok) {
                    var end = start
                    while (end < text.length && !text[end].isWhitespace() && text[end] != '<' && text[end] != '"') end++
                    while (end > start && text[end - 1] in ".,);:!?") end--
                    if (end > start && (best == null || start < best.first)) best = start to end
                }
                from = start + p.length
            }
        }
        return best
    }

    fun escape(s: String): String = s
        .replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;").replace("\"", "&quot;")
}
