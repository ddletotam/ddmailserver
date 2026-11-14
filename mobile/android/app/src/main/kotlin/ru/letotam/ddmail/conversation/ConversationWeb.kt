package ru.letotam.ddmail.conversation

import android.annotation.SuppressLint
import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Handler
import android.os.Looper
import android.webkit.JavascriptInterface
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.webkit.WebViewAssetLoader
import kotlinx.coroutines.runBlocking
import okhttp3.OkHttpClient
import okhttp3.Request
import ru.letotam.ddmail.BuildConfig
import java.io.ByteArrayInputStream
import java.util.concurrent.TimeUnit

/**
 * Один WebView на ленту беседы (контракт §8). Создаётся один раз и
 * переиспользуется: при открытии беседы меняется только документ.
 *
 * Вся сеть документа — через [shouldInterceptRequest]: наш источник
 * (`appassets.androidplatform.net`) отдаёт документ, части `cid:` и
 * разрешённые внешние картинки через прокси; всё прочее — отказ. Что из
 * `/remote` пускать (у спама — только картинки по нажатию), решает [gate].
 */
@SuppressLint("SetJavaScriptEnabled")
class ConversationWeb(context: Context, private val proxyHttp: OkHttpClient) {

    interface Host {
        fun onReady()
        fun onRequestOlder()
        fun onOpenAttachment(uid: Long, index: Int)
        /** Меню пузыря; [anchor] — где он на экране (`null` — неизвестно, меню посередине). */
        fun onBubbleMenu(uid: Long, anchor: MenuAnchor?)
        fun onAttachmentMenu(uid: Long, index: Int, anchor: MenuAnchor?)
        fun onAllowSender(uid: Long)
        /** Документ создан заново после падения процесса отрисовки: показать текущую беседу снова. */
        fun onRestarted()
        /** Часть письма `cid:` — блокирующе, вызывается не с главного потока. */
        fun loadPart(uid: Long, cid: String): Pair<ByteArray, String>?
    }

    var host: Host? = null

    /**
     * Пропуск внешних картинок открытой беседы (его держит модель беседы).
     * Нет — `/remote` закрыт целиком: лучше без картинок, чем спам с ними.
     */
    @Volatile
    var gate: RemoteImageGate? = null
    private val main = Handler(Looper.getMainLooper())
    private var ready = false
    private val pending = ArrayDeque<String>()

    private val assets = WebViewAssetLoader.Builder()
        .addPathHandler("/assets/", WebViewAssetLoader.AssetsPathHandler(context))
        .build()

    /**
     * Сам WebView. Меняется, только если процесс отрисовки упал (память, ОС
     * выгрузила): тогда старый уничтожается, создаётся новый, а [generation]
     * растёт — экран вставляет новый вид.
     */
    var view: WebView = create(context)
        private set

    /** Поколение WebView: экран пересоздаёт `AndroidView`, когда оно меняется. */
    val generation = kotlinx.coroutines.flow.MutableStateFlow(0)

    /** Документ грузится после перезапуска — на его `ready` хозяин заново откроет беседу. */
    private var restarting = false

    private fun create(context: Context): WebView = WebView(context).apply {
        WebView.setWebContentsDebuggingEnabled(BuildConfig.DEBUG)
        settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = false
            allowFileAccess = false
            allowContentAccess = false
            mixedContentMode = WebSettings.MIXED_CONTENT_NEVER_ALLOW
            setSupportZoom(true)
            builtInZoomControls = true
            displayZoomControls = false
            javaScriptCanOpenWindowsAutomatically = false
            setSupportMultipleWindows(false)
        }
        addJavascriptInterface(Bridge(), "ddmNative")
        webViewClient = Client()
        setBackgroundColor(0xFFF6F7F9.toInt())
        loadUrl("https://$ORIGIN/assets/conversation.html")
    }

    /** Выполнить в документе; до готовности документа — в очередь. */
    fun eval(js: String) {
        main.post {
            if (ready) view.evaluateJavascript(js, null) else pending.addLast(js)
        }
    }

    private inner class Bridge {
        @JavascriptInterface
        fun ready() = main.post {
            ready = true
            while (pending.isNotEmpty()) view.evaluateJavascript(pending.removeFirst(), null)
            host?.onReady()
            if (restarting) {
                restarting = false
                host?.onRestarted()
            }
        }

        @JavascriptInterface
        fun openLink(url: String) = main.post { openExternal(view.context, url) }

        @JavascriptInterface
        fun requestOlder() = main.post { host?.onRequestOlder() }

        @JavascriptInterface
        fun openAttachment(uid: Long, index: Int) = main.post { host?.onOpenAttachment(uid, index) }

        // Меню у элемента: после id — его прямоугольник, точка нажатия и
        // ширина видимой области, всё в CSS px (`askMenu` в conversation.js).
        // Число аргументов — часть сигнатуры: менять вместе с JS.
        @JavascriptInterface
        fun bubbleMenu(uid: Long, x: Double, y: Double, w: Double, h: Double, px: Double, py: Double, vw: Double) =
            main.post { host?.onBubbleMenu(uid, anchor(x, y, w, h, px, py, vw)) }

        @JavascriptInterface
        fun attachmentMenu(uid: Long, index: Int, x: Double, y: Double, w: Double, h: Double, px: Double, py: Double, vw: Double) =
            main.post { host?.onAttachmentMenu(uid, index, anchor(x, y, w, h, px, py, vw)) }

        @JavascriptInterface
        fun allowSender(uid: Long) = main.post { host?.onAllowSender(uid) }

        /**
         * «Показать картинки» у письма спама. Синхронно, не через главный
         * поток: документ сразу после вызова перерисовывает письмо, и его
         * запросы картинок должны застать разрешение уже записанным.
         */
        @JavascriptInterface
        fun allowSpamImages(uid: Long) {
            gate?.allow(uid)
        }
    }

    /** Координаты документа → пиксели окна; на главном потоке (читает вид). */
    private fun anchor(x: Double, y: Double, w: Double, h: Double, px: Double, py: Double, vw: Double): MenuAnchor? {
        val loc = IntArray(2)
        view.getLocationInWindow(loc)
        return MenuAnchor.fromDocument(loc[0], loc[1], view.width, view.height, x, y, w, h, px, py, vw)
    }

    private inner class Client : WebViewClient() {
        /**
         * Процесс отрисовки упал или его выгрузила ОС. Без обработки Android
         * завершает всё приложение; здесь — новый WebView с тем же документом,
         * беседа открывается в нём заново ([Host.onRestarted]).
         */
        override fun onRenderProcessGone(view: WebView, detail: android.webkit.RenderProcessGoneDetail): Boolean {
            if (view !== this@ConversationWeb.view) return true
            (view.parent as? android.view.ViewGroup)?.removeView(view)
            view.destroy()
            ready = false
            pending.clear()
            restarting = true
            this@ConversationWeb.view = create(view.context)
            generation.value++
            return true
        }

        /** Документ никуда не уходит: переходы — только во внешний браузер. */
        override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
            openExternal(view.context, request.url.toString())
            return true
        }

        override fun shouldInterceptRequest(view: WebView, request: WebResourceRequest): WebResourceResponse {
            val url = request.url
            if (url.host != ORIGIN || url.scheme != "https") return denied()
            val path = url.path.orEmpty()
            return when {
                path.startsWith("/assets/") -> assets.shouldInterceptRequest(url) ?: denied()
                path.startsWith("/cid/") -> cid(path)
                path == "/remote" -> {
                    val ok = gate?.permits(url.getQueryParameter("uid")?.toLongOrNull(), request.requestHeaders.orEmpty()) == true
                    if (ok) remote(url.getQueryParameter("u")) else denied()
                }
                else -> denied()
            }
        }

        private fun cid(path: String): WebResourceResponse {
            val parts = path.removePrefix("/cid/").split('/', limit = 2)
            val uid = parts.getOrNull(0)?.toLongOrNull() ?: return denied()
            val cid = Uri.decode(parts.getOrNull(1) ?: return denied())
            val (bytes, type) = host?.loadPart(uid, cid) ?: return denied()
            return WebResourceResponse(type.substringBefore(';'), null, ByteArrayInputStream(bytes))
        }

        /**
         * Внешняя картинка, которую разрешили для письма: без наших заголовков
         * и кук, только картинки (тип `image/…`) и не больше [MAX_REMOTE] байт.
         */
        private fun remote(u: String?): WebResourceResponse {
            val target = u?.takeIf { it.startsWith("https://") || it.startsWith("http://") } ?: return denied()
            return try {
                proxyHttp.newCall(Request.Builder().url(target).build()).execute().use { r ->
                    val type = r.header("Content-Type").orEmpty()
                    val len = r.body?.contentLength() ?: -1
                    if (!r.isSuccessful || !type.startsWith("image/") || len > MAX_REMOTE) return denied()
                    val bytes = r.body?.byteStream()?.use { readCapped(it, MAX_REMOTE) } ?: return denied()
                    WebResourceResponse(type.substringBefore(';'), null, ByteArrayInputStream(bytes))
                }
            } catch (e: Exception) {
                denied()
            }
        }
    }

    companion object {
        const val ORIGIN = "appassets.androidplatform.net"
        private const val MAX_REMOTE = 10 shl 20

        private fun denied() = WebResourceResponse("text/plain", "utf-8", 403, "Forbidden", emptyMap(), ByteArrayInputStream(ByteArray(0)))

        fun proxyClient(base: OkHttpClient): OkHttpClient = base.newBuilder()
            .connectTimeout(5, TimeUnit.SECONDS)
            .callTimeout(20, TimeUnit.SECONDS)
            .followRedirects(true)
            .build()

        fun openExternal(context: Context, url: String) {
            val uri = Uri.parse(url)
            if (uri.scheme !in setOf("https", "http", "mailto", "tel")) return
            try {
                context.startActivity(Intent(Intent.ACTION_VIEW, uri).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
            } catch (_: ActivityNotFoundException) {
            }
        }
    }
}

/** Прочитать поток, но не больше [cap] байт; больше — `null`. */
private fun readCapped(input: java.io.InputStream, cap: Int): ByteArray? {
    val out = java.io.ByteArrayOutputStream()
    val buf = ByteArray(64 * 1024)
    while (true) {
        val n = input.read(buf)
        if (n < 0) return out.toByteArray()
        if (out.size() + n > cap) return null
        out.write(buf, 0, n)
    }
}

/** Блокирующий мост для перехватчика WebView (он и так не на главном потоке). */
internal fun <T> blocking(block: suspend () -> T): T? = try {
    runBlocking { block() }
} catch (e: Exception) {
    null
}
