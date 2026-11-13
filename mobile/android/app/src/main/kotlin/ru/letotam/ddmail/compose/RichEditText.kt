package ru.letotam.ddmail.compose

import android.content.Context
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.graphics.Typeface
import android.graphics.drawable.BitmapDrawable
import android.net.Uri
import android.os.Build
import android.text.Editable
import android.text.Spannable
import android.text.SpannableString
import android.text.Spanned
import android.text.TextWatcher
import android.text.style.ImageSpan
import android.text.style.StyleSpan
import android.text.style.URLSpan
import android.text.style.UnderlineSpan
import android.view.ActionMode
import android.view.KeyEvent
import android.view.Menu
import android.view.MenuItem
import android.widget.EditText
import androidx.core.view.ContentInfoCompat
import androidx.core.view.ViewCompat
import java.io.ByteArrayOutputStream

/**
 * Картинка тела письма в редакторе: свой блок, превью — уменьшенная копия,
 * в письмо уходят [bytes] inline-вложением с [cid] (десктоп §3а).
 */
class InlineImageSpan(
    drawable: android.graphics.drawable.Drawable,
    val cid: String,
    val mime: String,
    val bytes: ByteArray,
) : ImageSpan(drawable, ALIGN_BOTTOM)

/** Стиль, который переключают меню выделения и Ctrl+B/I/U. */
enum class RichStyle { Bold, Italic, Underline }

/**
 * Поле ввода композера — rich-text поверх `EditText` (у Compose-полей стили
 * не переживают правку). Как у десктопа (§3а), отдельной строки тулбара нет:
 *
 * - Ж / К / Ч / «Ссылка» — пункты системного меню выделения текста; с
 *   аппаратной клавиатуры — Ctrl+B / I / U / K (по коду клавиши, в любой
 *   раскладке);
 * - картинка — из буфера обмена («Вставить», клавиатура с картинками) и из
 *   галереи (пункт «Картинка» меню, которое открывается долгим нажатием на
 *   пустом месте поля); всегда своим блоком;
 * - URL в тексте становится ссылкой при сериализации ([RichBody]).
 *
 * Содержимое отдаётся кусками [segments] и картинками [images]; plain/HTML
 * строит [RichBody].
 */
class RichEditText(context: Context) : EditText(context) {
    /** Содержимое изменилось (текст, стиль, картинка). */
    var onChange: (() -> Unit)? = null

    /** «Ссылка»: спросить адрес для выделения [start]..[end]. */
    var onLinkRequest: ((start: Int, end: Int) -> Unit)? = null

    /** «Картинка»: открыть выбор из галереи. */
    var onPickImage: (() -> Unit)? = null

    /** Пришла картинка (буфер обмена, клавиатура) — прочитать и вставить. */
    var onImageUri: ((Uri, String) -> Unit)? = null

    /** Правка из черновика (сброс, возврат) — без [onChange]. */
    private var muted = false

    init {
        addTextChangedListener(object : TextWatcher {
            override fun beforeTextChanged(s: CharSequence?, start: Int, count: Int, after: Int) {}
            override fun onTextChanged(s: CharSequence?, start: Int, before: Int, count: Int) {}
            override fun afterTextChanged(s: Editable?) {
                if (!muted) onChange?.invoke()
            }
        })
        customSelectionActionModeCallback = object : ActionMode.Callback {
            override fun onCreateActionMode(mode: ActionMode, menu: Menu): Boolean {
                menu.add(Menu.NONE, ID_BOLD, 100, styled("Ж", StyleSpan(Typeface.BOLD)))
                menu.add(Menu.NONE, ID_ITALIC, 101, styled("К", StyleSpan(Typeface.ITALIC)))
                menu.add(Menu.NONE, ID_UNDERLINE, 102, styled("Ч", UnderlineSpan()))
                menu.add(Menu.NONE, ID_LINK, 103, "Ссылка")
                return true
            }
            override fun onPrepareActionMode(mode: ActionMode, menu: Menu) = false
            override fun onActionItemClicked(mode: ActionMode, item: MenuItem): Boolean = when (item.itemId) {
                ID_BOLD -> { toggle(RichStyle.Bold); true }
                ID_ITALIC -> { toggle(RichStyle.Italic); true }
                ID_UNDERLINE -> { toggle(RichStyle.Underline); true }
                ID_LINK -> {
                    onLinkRequest?.invoke(selectionStart.coerceAtMost(selectionEnd), selectionEnd.coerceAtLeast(selectionStart))
                    mode.finish()
                    true
                }
                else -> false
            }
            override fun onDestroyActionMode(mode: ActionMode) {}
        }
        // Меню на каретке («Вставить»): плюс «Картинка» из галереи.
        customInsertionActionModeCallback = object : ActionMode.Callback {
            override fun onCreateActionMode(mode: ActionMode, menu: Menu): Boolean {
                menu.add(Menu.NONE, ID_IMAGE, 100, "Картинка")
                return true
            }
            override fun onPrepareActionMode(mode: ActionMode, menu: Menu) = false
            override fun onActionItemClicked(mode: ActionMode, item: MenuItem): Boolean {
                if (item.itemId != ID_IMAGE) return false
                mode.finish()
                onPickImage?.invoke()
                return true
            }
            override fun onDestroyActionMode(mode: ActionMode) {}
        }
        // Картинки из буфера обмена и с клавиатуры; текст — как обычно.
        ViewCompat.setOnReceiveContentListener(this, arrayOf("image/*")) { _, payload ->
            val split = payload.partition { it.uri != null && (it.uri?.let { u -> context.contentResolver.getType(u) }?.startsWith("image/") == true) }
            val images = split.first
            if (images != null) {
                val clip = images.clip
                for (i in 0 until clip.itemCount) {
                    val uri = clip.getItemAt(i).uri ?: continue
                    onImageUri?.invoke(uri, context.contentResolver.getType(uri) ?: "image/png")
                }
            }
            split.second
        }
    }

    /**
     * Вставка до Android 12: платформа сама зовёт слушатель содержимого
     * только с API 31, раньше — передать ему буфер обмена руками.
     */
    override fun onTextContextMenuItem(id: Int): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.S && (id == android.R.id.paste || id == android.R.id.pasteAsPlainText)) {
            val cm = context.getSystemService(android.content.ClipboardManager::class.java)
            val clip = cm?.primaryClip
            if (clip != null && clip.itemCount > 0 && clip.getItemAt(0).uri != null) {
                val info = ContentInfoCompat.Builder(clip, ContentInfoCompat.SOURCE_CLIPBOARD).build()
                ViewCompat.performReceiveContent(this, info)
                return true
            }
        }
        return super.onTextContextMenuItem(id)
    }

    /** Ctrl+B/I/U/K с аппаратной клавиатуры — по коду клавиши, не по символу. */
    override fun onKeyShortcut(keyCode: Int, event: KeyEvent): Boolean {
        if (event.isCtrlPressed) {
            when (keyCode) {
                KeyEvent.KEYCODE_B -> { toggle(RichStyle.Bold); return true }
                KeyEvent.KEYCODE_I -> { toggle(RichStyle.Italic); return true }
                KeyEvent.KEYCODE_U -> { toggle(RichStyle.Underline); return true }
                KeyEvent.KEYCODE_K -> {
                    onLinkRequest?.invoke(selectionStart.coerceAtMost(selectionEnd), selectionEnd.coerceAtLeast(selectionStart))
                    return true
                }
            }
        }
        return super.onKeyShortcut(keyCode, event)
    }

    /** Заменить содержимое из черновика, не считая это правкой пользователя. */
    fun load(content: CharSequence) {
        muted = true
        try {
            setText(content)
            setSelection(text.length)
        } finally {
            muted = false
        }
    }

    /**
     * Ж/К/Ч на выделении: всё выделение уже в этом стиле — снять, иначе
     * наложить. Без выделения — ничего (меню выделения без него не
     * открывается).
     */
    fun toggle(style: RichStyle) {
        val e = text ?: return
        val s = minOf(selectionStart, selectionEnd)
        val t = maxOf(selectionStart, selectionEnd)
        if (s < 0 || s == t) return
        val spans = e.getSpans(s, t, Any::class.java).filter { it.isStyle(style) }
        val covered = (s until t).all { i -> spans.any { e.getSpanStart(it) <= i && e.getSpanEnd(it) > i } }
        if (covered) {
            // Снять стиль только с выделения: края спанов за его пределами остаются.
            for (sp in spans) {
                val a = e.getSpanStart(sp)
                val b = e.getSpanEnd(sp)
                e.removeSpan(sp)
                if (a < s) e.setSpan(newSpan(style), a, s, Spanned.SPAN_EXCLUSIVE_INCLUSIVE)
                if (b > t) e.setSpan(newSpan(style), t, b, Spanned.SPAN_EXCLUSIVE_INCLUSIVE)
            }
        } else {
            e.setSpan(newSpan(style), s, t, Spanned.SPAN_EXCLUSIVE_INCLUSIVE)
        }
        onChange?.invoke()
    }

    /** Ссылка на [start]..[end]; без выделения — вставить сам адрес ссылкой. */
    fun applyLink(start: Int, end: Int, url: String) {
        val e = text ?: return
        val href = RichBody.safeHref(url)
        val (s, t) = if (start == end) {
            e.insert(start, url.trim())
            start to start + url.trim().length
        } else {
            start.coerceIn(0, e.length) to end.coerceIn(0, e.length)
        }
        e.getSpans(s, t, URLSpan::class.java).forEach { e.removeSpan(it) }
        e.setSpan(URLSpan(href), s, t, Spanned.SPAN_EXCLUSIVE_EXCLUSIVE)
        setSelection(t)
        onChange?.invoke()
    }

    /** Картинка своим блоком в позиции каретки. */
    fun insertImage(bytes: ByteArray, mime: String, cid: String) {
        val e = text ?: return
        val preview = previewDrawable(bytes) ?: return
        var at = maxOf(selectionStart, selectionEnd).coerceIn(0, e.length)
        val pre = if (at > 0 && e[at - 1] != '\n') "\n" else ""
        val post = if (at < e.length && e[at] == '\n') "" else "\n"
        val obj = SpannableString(OBJ)
        obj.setSpan(InlineImageSpan(preview, cid, mime, bytes), 0, 1, Spanned.SPAN_EXCLUSIVE_EXCLUSIVE)
        e.insert(at, pre)
        at += pre.length
        e.insert(at, obj)
        e.insert(at + 1, post)
        setSelection((at + 1 + post.length).coerceAtMost(e.length))
        onChange?.invoke()
    }

    /** Превью картинки по ширине поля (не шире 240 dp). */
    private fun previewDrawable(bytes: ByteArray): BitmapDrawable? {
        val bmp = BitmapFactory.decodeByteArray(bytes, 0, bytes.size) ?: return null
        val maxW = (240 * resources.displayMetrics.density).toInt().coerceAtMost((width - paddingLeft - paddingRight).takeIf { it > 0 } ?: Int.MAX_VALUE)
        val scale = minOf(1f, maxW.toFloat() / bmp.width)
        val w = (bmp.width * scale).toInt().coerceAtLeast(1)
        val h = (bmp.height * scale).toInt().coerceAtLeast(1)
        val small = if (scale < 1f) Bitmap.createScaledBitmap(bmp, w, h, true) else bmp
        return BitmapDrawable(resources, small).apply { setBounds(0, 0, w, h) }
    }

    /** Содержимое кусками одного стиля; картинка — отдельным куском. */
    fun segments(): List<RichSeg> {
        val e = text ?: return emptyList()
        val out = mutableListOf<RichSeg>()
        var i = 0
        while (i < e.length) {
            val next = e.nextSpanTransition(i, e.length, Any::class.java)
            val spans = e.getSpans(i, next, Any::class.java)
            val img = spans.filterIsInstance<InlineImageSpan>().firstOrNull()
            if (img != null) {
                // Картинка — ровно её символ; текст рядом (если вдруг) — отдельно.
                val a = e.getSpanStart(img)
                val b = e.getSpanEnd(img)
                if (a > i) out += textSeg(e, i, a, spans)
                out += RichSeg.Image(img.cid)
                i = maxOf(b, i + 1)
                continue
            }
            out += textSeg(e, i, next, spans)
            i = next
        }
        return out
    }

    private fun textSeg(e: Spanned, a: Int, b: Int, spans: Array<Any>): RichSeg.Text {
        val text = e.subSequence(a, b).toString().replace(OBJ, "")
        return RichSeg.Text(
            text = text,
            bold = spans.any { it.isStyle(RichStyle.Bold) },
            italic = spans.any { it.isStyle(RichStyle.Italic) },
            underline = spans.any { it.isStyle(RichStyle.Underline) },
            link = spans.filterIsInstance<URLSpan>().firstOrNull()?.url,
        )
    }

    /** Картинки тела в порядке появления. */
    fun images(): List<InlineImageSpan> {
        val e = text ?: return emptyList()
        return e.getSpans(0, e.length, InlineImageSpan::class.java).sortedBy { e.getSpanStart(it) }
    }

    private fun Any.isStyle(style: RichStyle): Boolean = when (style) {
        RichStyle.Bold -> this is StyleSpan && (this.style == Typeface.BOLD || this.style == Typeface.BOLD_ITALIC)
        RichStyle.Italic -> this is StyleSpan && (this.style == Typeface.ITALIC || this.style == Typeface.BOLD_ITALIC)
        RichStyle.Underline -> this is UnderlineSpan && this !is URLSpan
    }

    private fun newSpan(style: RichStyle): Any = when (style) {
        RichStyle.Bold -> StyleSpan(Typeface.BOLD)
        RichStyle.Italic -> StyleSpan(Typeface.ITALIC)
        RichStyle.Underline -> UnderlineSpan()
    }

    private fun styled(t: String, span: Any): CharSequence =
        SpannableString(t).apply { setSpan(span, 0, t.length, Spannable.SPAN_EXCLUSIVE_EXCLUSIVE) }

    companion object {
        /** Символ объекта, на котором стоит картинка. */
        const val OBJ = "￼"
        private const val ID_BOLD = 0x7d01
        private const val ID_ITALIC = 0x7d02
        private const val ID_UNDERLINE = 0x7d03
        private const val ID_LINK = 0x7d04
        private const val ID_IMAGE = 0x7d05

        /**
         * Картинка для письма: снимок экрана или фото из галереи. Больше
         * 2000 px по длинной стороне или 1.5 МБ — уменьшается и пережимается
         * (PNG остаётся PNG — скриншоты с текстом; прочее — JPEG 85).
         */
        fun prepareImage(raw: ByteArray, mime: String): Pair<ByteArray, String>? {
            val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
            BitmapFactory.decodeByteArray(raw, 0, raw.size, bounds)
            if (bounds.outWidth <= 0 || bounds.outHeight <= 0) return null
            val longSide = maxOf(bounds.outWidth, bounds.outHeight)
            val okMime = mime == "image/png" || mime == "image/jpeg" || mime == "image/gif" || mime == "image/webp"
            if (longSide <= MAX_SIDE && raw.size <= MAX_BYTES && okMime) return raw to mime
            var sample = 1
            while (longSide / (sample * 2) >= MAX_SIDE) sample *= 2
            val bmp = BitmapFactory.decodeByteArray(raw, 0, raw.size, BitmapFactory.Options().apply { inSampleSize = sample }) ?: return null
            val scale = minOf(1f, MAX_SIDE.toFloat() / maxOf(bmp.width, bmp.height))
            val scaled = if (scale < 1f) Bitmap.createScaledBitmap(bmp, (bmp.width * scale).toInt().coerceAtLeast(1), (bmp.height * scale).toInt().coerceAtLeast(1), true) else bmp
            val png = mime == "image/png"
            val out = ByteArrayOutputStream()
            scaled.compress(if (png) Bitmap.CompressFormat.PNG else Bitmap.CompressFormat.JPEG, if (png) 100 else 85, out)
            return out.toByteArray() to (if (png) "image/png" else "image/jpeg")
        }

        private const val MAX_SIDE = 2000
        private const val MAX_BYTES = 1_500_000
    }
}
