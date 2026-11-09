package ru.letotam.ddmail.compose

import android.content.Context
import android.net.Uri
import android.provider.OpenableColumns
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.api.Identity
import ru.letotam.ddmail.api.MessageBody
import ru.letotam.ddmail.conversation.ConversationController
import ru.letotam.ddmail.sync.Conversation
import ru.letotam.ddmail.sync.SyncEngine

/** Отдельное окно письма: новое или пересылка. */
data class ComposeSheet(val branch: Branch, val draft: Draft, val error: String? = null, val busy: Boolean = false)

data class ComposerUi(
    /** Черновик ответа в открытой беседе. */
    val draft: Draft = Draft(),
    /** Явный ответ на конкретное письмо (лента «Ответ на …»). */
    val replyTo: MessageBody? = null,
    val expanded: Boolean = false,
    /** «✓ Отправлено» ~2 с — без тостов (§2 десктопа). */
    val sentFlash: Boolean = false,
    val error: String? = null,
    val sheet: ComposeSheet? = null,
)

/**
 * Отправка (контракт §5, десктопный §1–2): ветки, optimistic-заглушка в
 * ленте, сверка с настоящим письмом, «✓ Отправлено», при ошибке — заглушка
 * убирается, текст возвращается в композер.
 *
 * Отправка асинхронная: сервер ставит письмо в очередь, копия в
 * «Отправленных» (и сама беседа) появляется после доставки — поэтому после
 * отправки синхронизируемся ещё несколько раз, пока нет WebSocket-пуша.
 */
class ComposeController(
    private val app: Context,
    private val engine: SyncEngine,
    private val conversation: ConversationController,
    private val scope: CoroutineScope,
    private val syncSoon: () -> Unit,
    private val onAuthDead: () -> Unit,
) {
    val ui = MutableStateFlow(ComposerUi())
    val identities: StateFlow<List<Identity>> = engine.identities.stateIn(scope, SharingStarted.Eagerly, emptyList())

    /** После нового письма — открыть беседу с этими адресатами, когда появится. */
    var sentTarget: Set<String>? = null
        private set

    private var stubSeq = 0

    fun edit(f: (Draft) -> Draft) = ui.update { it.copy(draft = f(it.draft), error = null) }
    fun toggleExpanded() = ui.update { it.copy(expanded = !it.expanded) }
    fun replyTo(b: MessageBody?) = ui.update { it.copy(replyTo = b) }

    /** Сменилась беседа — явный ответ и переопределения не переезжают в другую. */
    fun reset() = ui.update { ComposerUi(sheet = it.sheet) }

    fun openNew() = ui.update { it.copy(sheet = ComposeSheet(Branch.New, Draft())) }

    /** Новое письмо на адрес — из карточки контакта. */
    fun openNewTo(addr: String) = ui.update { it.copy(sheet = ComposeSheet(Branch.New, Draft(to = addr))) }

    fun openForward(b: MessageBody) = ui.update { it.copy(sheet = ComposeSheet(Branch.Forward(b), Draft())) }

    fun editSheet(f: (Draft) -> Draft) = ui.update { u -> u.copy(sheet = u.sheet?.let { it.copy(draft = f(it.draft), error = null) }) }

    fun closeSheet() = ui.update { it.copy(sheet = null) }

    fun consumeTarget() {
        sentTarget = null
    }

    /** Файлы из системного выбора; общий размер — в пределах лимита сервера. */
    fun attach(uris: List<Uri>, toSheet: Boolean) {
        scope.launch {
            val picked = withContext(Dispatchers.IO) { uris.mapNotNull(::read) }
            val cur = if (toSheet) ui.value.sheet?.draft?.attachments.orEmpty() else ui.value.draft.attachments
            val total = (cur + picked).sumOf { it.size.toLong() }
            if (total > MAX_TOTAL) {
                val msg = "Вложения больше ${MAX_TOTAL shr 20} МБ — сервер такое не примет."
                if (toSheet) ui.update { u -> u.copy(sheet = u.sheet?.copy(error = msg)) } else ui.update { it.copy(error = msg) }
                return@launch
            }
            if (toSheet) editSheet { it.copy(attachments = it.attachments + picked) } else edit { it.copy(attachments = it.attachments + picked) }
        }
    }

    private fun read(uri: Uri): PendingAttachment? = runCatching {
        val cr = app.contentResolver
        val name = cr.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)?.use { c ->
            if (c.moveToFirst()) c.getString(0) else null
        } ?: uri.lastPathSegment ?: "file"
        val mime = cr.getType(uri) ?: "application/octet-stream"
        val bytes = cr.openInputStream(uri)?.use { it.readBytes() } ?: return null
        PendingAttachment(name, mime, bytes)
    }.getOrNull()

    /** Ответ в открытой беседе (ветки 2–3). */
    fun sendReply(c: Conversation) {
        val u = ui.value
        val branch = u.replyTo?.let { Branch.Reply(it, c.isGroup) }
            ?: Branch.Implicit(c, conversation.lastIncoming(), conversation.last())
        when (val built = build(branch, u.draft, identities.value)) {
            is Built.Refused -> ui.update { it.copy(error = built.reason) }
            is Built.Ok -> {
                val draft = u.draft
                val key = "s${++stubSeq}"
                conversation.js.trySend("ddm.stub(${stubJson(key, draft)})")
                ui.update { ComposerUi(sheet = it.sheet) }
                scope.launch {
                    try {
                        engine.send(built.request)
                        ui.update { it.copy(sentFlash = true) }
                        afterSend()
                        delay(2000)
                        ui.update { it.copy(sentFlash = false) }
                    } catch (e: ApiException) {
                        if (e is ApiException.Unauthorized) onAuthDead()
                        conversation.js.trySend("ddm.unstub(\"$key\")")
                        // Текст возвращается, если композер пуст (не затираем набранное заново).
                        ui.update { cur ->
                            cur.copy(
                                draft = if (cur.draft.text.isEmpty() && cur.draft.attachments.isEmpty()) draft else cur.draft,
                                replyTo = cur.replyTo ?: u.replyTo,
                                error = sendError(e),
                            )
                        }
                    }
                }
            }
        }
    }

    /** Новое письмо или пересылка (ветки 0–1) — из отдельного окна. */
    fun sendSheet() {
        val sheet = ui.value.sheet ?: return
        if (sheet.busy) return
        ui.update { it.copy(sheet = sheet.copy(busy = true, error = null)) }
        scope.launch {
            val fwd = (sheet.branch as? Branch.Forward)?.original
            val (fwdText, fwdHtml) = fwd?.let(::forwardBlock) ?: ("" to "")
            // Пересылка несёт вложения исходного письма.
            val extra = try {
                fwd?.attachments.orEmpty().map { a ->
                    val bin = engine.attachment(fwd!!.uid, a.index)
                    PendingAttachment(a.filename, a.mimeType.ifBlank { bin.contentType }, bin.bytes)
                }
            } catch (e: ApiException) {
                ui.update { u -> u.copy(sheet = u.sheet?.copy(busy = false, error = "Не удалось забрать вложения пересылаемого письма: ${sendError(e)}")) }
                return@launch
            }
            when (val built = build(sheet.branch, sheet.draft, identities.value, fwdHtml, fwdText, extra)) {
                is Built.Refused -> ui.update { u -> u.copy(sheet = u.sheet?.copy(busy = false, error = built.reason)) }
                is Built.Ok -> try {
                    engine.send(built.request)
                    if (sheet.branch is Branch.New) sentTarget = built.to.map { it.lowercase() }.toSet()
                    ui.update { it.copy(sheet = null, sentFlash = true) }
                    afterSend()
                    delay(2000)
                    ui.update { it.copy(sentFlash = false) }
                } catch (e: ApiException) {
                    if (e is ApiException.Unauthorized) onAuthDead()
                    ui.update { u -> u.copy(sheet = u.sheet?.copy(busy = false, error = sendError(e))) }
                }
            }
        }
    }

    /** Копия в «Отправленных» появится после доставки — подтягиваем её. */
    private fun afterSend() {
        scope.launch {
            for (d in listOf(2_000L, 6_000L, 15_000L, 40_000L)) {
                delay(d)
                syncSoon()
            }
        }
    }

    private fun sendError(e: ApiException): String = when (e) {
        is ApiException.Unauthorized -> "сессия истекла — войдите заново"
        is ApiException.Network -> "нет соединения с сервером"
        is ApiException.Server -> if (e.code == 413) "вложения больше лимита сервера" else "сервер ответил ошибкой ${e.code}"
    }.let { "Не отправлено: $it." }

    private fun stubJson(key: String, d: Draft) = JsonObject(
        mapOf(
            "key" to JsonPrimitive(key),
            "text" to JsonPrimitive(d.text),
            "attachments" to JsonArray(
                d.attachments.map { JsonObject(mapOf("filename" to JsonPrimitive(it.name), "size" to JsonPrimitive(it.size))) },
            ),
        ),
    ).toString()

    private companion object {
        /** Лимит сервера — 30 МБ на запрос в base64; сырых байт — около 22. */
        const val MAX_TOTAL = 22L shl 20
    }
}
