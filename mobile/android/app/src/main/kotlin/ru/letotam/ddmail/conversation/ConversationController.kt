package ru.letotam.ddmail.conversation

import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import android.webkit.MimeTypeMap
import androidx.core.content.FileProvider
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.api.DeliveryEvent
import ru.letotam.ddmail.api.MessageBody
import ru.letotam.ddmail.api.MessageRef
import ru.letotam.ddmail.sync.Conversation
import ru.letotam.ddmail.sync.MailHub
import ru.letotam.ddmail.sync.isAuthError
import ru.letotam.ddmail.sync.isMailError
import java.io.File

/**
 * Открытая беседа (контракт §8): окно отрисовки по [PAGE] писем — сначала
 * последние, старые порциями по просьбе документа; новые письма открытой
 * беседы дописываются снизу. Команды документу — строками JS в [js]
 * (WebView живёт в UI, модель его не держит).
 *
 * Порядок «последние» — по uid (id строки на сервере растёт с приходом
 * письма): у ссылок нет дат, а качать тела всей беседы ради сортировки
 * значило бы тянуть сотни писем на открытии.
 */
class ConversationController(
    private val app: Context,
    private val settings: ru.letotam.ddmail.settings.AppSettings,
    private val hub: MailHub,
    private val scope: CoroutineScope,
) {
    val js = Channel<String>(Channel.UNLIMITED)

    /** Пропуск внешних картинок для перехватчика WebView ([ConversationWeb.gate]). */
    val gate = RemoteImageGate()

    private val lock = Mutex()
    private var job: Job? = null
    private var conv: Conversation? = null
    private var pages: List<MessageRef> = emptyList()
    private var loaded = 0
    private val shown = mutableSetOf<Long>()

    /** Тела показанных писем — для веток ответа и меню пузыря. */
    private val loadedBodies = java.util.concurrent.ConcurrentHashMap<Long, MessageBody>()

    fun body(uid: Long): MessageBody? = loadedBodies[uid]

    /** Последнее входящее среди показанных: на него отвечает неявный ответ. */
    fun lastIncoming(): MessageBody? = loadedBodies.values.filter { !it.isOutgoing }.maxByOrNull { it.dateTs }

    fun last(): MessageBody? = loadedBodies.values.maxByOrNull { it.dateTs }

    val current: Conversation? get() = conv

    fun open(c: Conversation?) {
        job?.cancel()
        conv = c
        shown.clear()
        loadedBodies.clear()
        // До документа новой беседы: прежние разрешения ей не достаются.
        gate.open(c?.spam == true, c?.messages.orEmpty().map { it.uid })
        if (c == null) return
        js.trySend("ddm.set({messages:[],hasOlder:false})")
        job = scope.launch {
            lock.withLock {
                pages = newestFirst(c.messages)
                loaded = minOf(PAGE, pages.size)
                senders = settings.imageSenders()
                val bodies = load(pages.take(loaded))
                // Первое непрочитанное — к нему и прокрутить (как у десктопа).
                val firstUnseen = pages.take(loaded).lastOrNull { !it.seen }?.uid
                js.send("ddm.set(${batch(c, bodies, firstUnseen)})")
            }
            if (c.unreadCount > 0 || c.messages.any { !it.seen }) {
                runCatching { hub.markRead(c) }
            }
        }
    }

    /** Документ дошёл до верха — следующая порция старых писем. */
    fun older() {
        val c = conv ?: return
        scope.launch {
            lock.withLock {
                if (conv?.ref != c.ref) return@launch
                val next = pages.drop(loaded).take(PAGE)
                loaded += next.size
                val bodies = load(next)
                js.send("ddm.prepend(${batch(c, bodies, null)})")
            }
        }
    }

    /**
     * Беседа обновилась дельтой: новые письма — снизу, без перерисовки; у
     * показанных своих писем, чей статус доставки сменился, — пометка на
     * месте (`ddm.delivery`; тело перечитывается — в нём причина словами).
     */
    fun updated(c: Conversation) {
        if (conv?.ref != c.ref) return
        conv = c
        scope.launch {
            lock.withLock {
                if (conv?.ref != c.ref) return@launch
                val before = pages.associateBy { it.uid }
                val fresh = c.messages.filter { it.uid !in before }
                val changed = c.messages.filter { r -> r.uid in shown && before[r.uid]?.let { it.deliveryStatus != r.deliveryStatus } == true }
                if (fresh.isEmpty() && changed.isEmpty()) return@launch
                pages = newestFirst(c.messages)
                for (b in load(changed)) js.send("ddm.delivery(${b.uid},${deliveryJson(b)})")
                if (fresh.isEmpty()) return@launch
                gate.extend(fresh.map { it.uid })
                loaded += fresh.size
                val bodies = load(fresh)
                js.send("ddm.append(${batch(c, bodies, null)})")
            }
            if (c.messages.any { !it.seen }) runCatching { hub.markRead(c) }
        }
    }

    /**
     * Событие `delivery_status` (WS): пузырь письма в открытой беседе — сразу,
     * без запроса; в событии те же поля, что в теле. Письма нет на экране —
     * нечего делать: кэш уже поправлен, откроют — покажется верно.
     */
    fun delivery(ev: DeliveryEvent) {
        val c = conv ?: return
        if (c.accountKey != hub.ddmail?.accountKey) return
        val uid = ev.messageId.takeIf { it > 0 }
            ?: loadedBodies.values.firstOrNull { it.messageId == ev.rfcMessageId && ev.rfcMessageId.isNotEmpty() }?.uid
            ?: return
        val b = loadedBodies[uid] ?: return
        val now = b.copy(deliveryStatus = ev.status, deliveryReason = ev.reason, deliveryError = ev.error, deliveryFailed = ev.failed)
        loadedBodies[uid] = now
        js.trySend("ddm.delivery($uid,${deliveryJson(now)})")
    }

    /** Часть `cid:` для документа — блокирующе, с потока перехватчика WebView. */
    fun part(uid: Long, cid: String): Pair<ByteArray, String>? =
        conv?.accountKey?.let { key -> blocking { hub.part(key, uid, cid) } }?.let { it.bytes to it.contentType }

    /**
     * Вложение: скачать в кэш и открыть системным обработчиком через
     * content:// (файлы приложения наружу напрямую не отдаются).
     */
    fun openAttachment(uid: Long, index: Int, filename: String?) {
        scope.launch {
            val key = conv?.accountKey ?: return@launch
            val bin = try {
                hub.attachment(key, uid, index)
            } catch (e: Exception) {
                if (e is kotlinx.coroutines.CancellationException) throw e
                return@launch
            }
            val file = withContext(Dispatchers.IO) {
                val dir = File(app.cacheDir, "attachments/$uid").apply { mkdirs() }
                File(dir, safeName(filename ?: "attachment-$index")).apply { writeBytes(bin.bytes) }
            }
            val type = bin.contentType.substringBefore(';').trim().ifEmpty {
                MimeTypeMap.getSingleton().getMimeTypeFromExtension(file.extension) ?: "application/octet-stream"
            }
            val uri = FileProvider.getUriForFile(app, "${app.packageName}.files", file)
            val intent = Intent(Intent.ACTION_VIEW).setDataAndType(uri, type)
                .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_ACTIVITY_NEW_TASK)
            try {
                app.startActivity(Intent.createChooser(intent, null).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
            } catch (_: ActivityNotFoundException) {
            }
        }
    }

    private var senders: Set<String> = emptySet()

    /**
     * «Всегда показывать картинки от отправителя» этого письма. У спама такой
     * кнопки нет, и сохранять разрешение по спаму нельзя: его отправитель —
     * кто угодно, кто подделал адрес.
     */
    fun allowSender(uid: Long) {
        if (conv?.spam == true) return
        val addr = body(uid)?.fromAddr?.takeIf { it.isNotBlank() } ?: return
        senders = senders + addr.lowercase()
        scope.launch { settings.allowImagesFrom(addr) }
    }

    /**
     * Сохранить вложение в «Загрузки» (MediaStore — на Android 10+ без
     * разрешений). Возвращает имя файла или текст ошибки через [onDone].
     */
    fun saveAttachment(uid: Long, index: Int, onDone: (String) -> Unit) {
        val name = safeName(names[uid to index] ?: "attachment-$index")
        scope.launch {
            val key = conv?.accountKey ?: return@launch
            val bin = try {
                hub.attachment(key, uid, index)
            } catch (e: Exception) {
                if (e is kotlinx.coroutines.CancellationException) throw e
                onDone("Не удалось скачать «$name».")
                return@launch
            }
            val ok = withContext(Dispatchers.IO) {
                runCatching {
                    val values = android.content.ContentValues().apply {
                        put(android.provider.MediaStore.Downloads.DISPLAY_NAME, name)
                        put(android.provider.MediaStore.Downloads.MIME_TYPE, bin.contentType.substringBefore(';').trim().ifEmpty { "application/octet-stream" })
                        put(android.provider.MediaStore.Downloads.IS_PENDING, 1)
                    }
                    val cr = app.contentResolver
                    val uri = cr.insert(android.provider.MediaStore.Downloads.EXTERNAL_CONTENT_URI, values) ?: error("insert")
                    cr.openOutputStream(uri)?.use { it.write(bin.bytes) } ?: error("open")
                    values.clear()
                    values.put(android.provider.MediaStore.Downloads.IS_PENDING, 0)
                    cr.update(uri, values, null, null)
                }.isSuccess
            }
            onDone(if (ok) "Сохранено в «Загрузки»: $name" else "Не удалось сохранить «$name».")
        }
    }

    /** Имя вложения по uid/индексу — из уже показанных тел. */
    fun attachmentName(uid: Long, index: Int): String? = names[uid to index]

    private val names = mutableMapOf<Pair<Long, Int>, String>()

    private suspend fun load(refs: List<MessageRef>): List<MessageBody> = try {
        val key = conv?.accountKey ?: return emptyList()
        hub.bodies(key, refs).also { list -> list.forEach { loadedBodies[it.uid] = it } }
    } catch (e: Exception) {
        if (!e.isMailError()) throw e
        emptyList()
    }

    private fun batch(c: Conversation, bodies: List<MessageBody>, focusUid: Long?): String {
        val msgs = bodies.filter { shown.add(it.uid) }.map { b ->
            b.attachments.forEach { names[b.uid to it.index] = it.filename }
            JsonObject(
                buildMap {
                    put("uid", JsonPrimitive(b.uid))
                    put("outgoing", JsonPrimitive(b.isOutgoing))
                    put("fromName", JsonPrimitive(b.from))
                    put("fromAddr", JsonPrimitive(b.fromAddr))
                    // Спам: сохранённые разрешения по отправителю не действуют —
                    // картинки только по нажатию и только на этот показ.
                    put("imagesAllowed", JsonPrimitive(!c.spam && b.fromAddr.lowercase() in senders))
                    put("spam", JsonPrimitive(c.spam))
                    put("dateTs", JsonPrimitive(b.dateTs))
                    b.html?.takeIf { it.isNotBlank() }?.let { put("html", JsonPrimitive(it)) }
                    b.text?.let { put("text", JsonPrimitive(it)) }
                    put(
                        "attachments",
                        JsonArray(
                            b.attachments.map {
                                JsonObject(
                                    mapOf(
                                        "index" to JsonPrimitive(it.index),
                                        "filename" to JsonPrimitive(it.filename),
                                        "size" to JsonPrimitive(it.size),
                                    ),
                                )
                            },
                        ),
                    )
                    // В группе подписываем входящие именем отправителя.
                    put("showFrom", JsonPrimitive(c.isGroup))
                    // Статус доставки своего письма; нет статуса — нет и поля.
                    if (b.deliveryStatus.isNotEmpty()) put("delivery", deliveryJson(b))
                },
            )
        }
        val obj = buildMap {
            put("messages", JsonArray(msgs))
            put("hasOlder", JsonPrimitive(loaded < pages.size))
            focusUid?.let { put("focusUid", JsonPrimitive(it)) }
        }
        return JsonObject(obj).toString()
    }

    private companion object {
        const val PAGE = 20

        /**
         * От нового к старому. У ddmail id письма растёт с его приходом —
         * порядок по uid (§8). У IMAP uid упакован с ролью папки и между
         * папками несравним — берётся порядок беседы (источник кладёт письма
         * по дате).
         */
        fun newestFirst(refs: List<MessageRef>): List<MessageRef> =
            if (refs.any { (it.uid ushr 32) != 0L }) refs.reversed() else refs.sortedByDescending { it.uid }

        /** Имя файла без разделителей пути и управляющих символов. */
        fun safeName(name: String): String =
            name.replace(Regex("[\\\\/:*?\"<>|\\p{Cntrl}]"), "_").trim().trimEnd('.').ifEmpty { "attachment" }
    }
}

/**
 * Поле `delivery` письма в документе ленты (общий JS API с iOS):
 * `{status, error, failed}` — статус (`pending` / `delivered` / `failed`),
 * причина словами (пусто — нет) и кому не доставлено.
 */
internal fun deliveryJson(status: String, error: String, failed: List<String>): JsonObject = JsonObject(
    mapOf(
        "status" to JsonPrimitive(status),
        "error" to JsonPrimitive(error),
        "failed" to JsonArray(failed.map { JsonPrimitive(it) }),
    ),
)

internal fun deliveryJson(b: MessageBody): JsonObject = deliveryJson(b.deliveryStatus, b.deliveryError, b.deliveryFailed)
