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
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.api.Identity
import ru.letotam.ddmail.api.MessageBody
import ru.letotam.ddmail.api.Delivery
import ru.letotam.ddmail.conversation.ConversationController
import ru.letotam.ddmail.conversation.deliveryJson
import ru.letotam.ddmail.sync.SmtpFailed
import ru.letotam.ddmail.sync.Conversation
import ru.letotam.ddmail.sync.MailHub
import ru.letotam.ddmail.sync.errorText
import ru.letotam.ddmail.sync.isMailError

/** Отдельное окно письма — пересылка (новое пишется в новой беседе, из поиска). */
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
    /**
     * Новая беседа (из поиска): ушедшие письма — пузыри в пустой ленте,
     * пока беседа не пришла с сервера и не открылась вместо заготовки.
     */
    val pendingNew: List<PendingNew> = emptyList(),
)

/**
 * Пузырь письма в новой беседе: [sent] — сервер принял, ждём саму беседу;
 * [failed] — не ушло (SMTP IMAP-учётки отказал), причина словами: пузырь
 * остаётся с красной пометкой «Не доставлено», текст — снова в композере.
 */
data class PendingNew(val key: String, val text: String, val sent: Boolean = false, val failed: String? = null)

/**
 * Отправка (контракт §5, десктопный §1–2): ветки, optimistic-заглушка в
 * ленте, сверка с настоящим письмом, «✓ Отправлено», при ошибке — заглушка
 * убирается, текст возвращается в композер.
 *
 * Сервер ddmail кладёт копию в «Отправленные» при приёме письма (с 2026-11-13):
 * беседа есть сразу, после отправки хватает синка и WS `message_sent`;
 * доставку сервер сообщает статусом на самом письме (`delivery_status`).
 * IMAP-учётка шлёт через свой SMTP: принял — доставлено, отказал — заглушка
 * остаётся с пометкой «Не доставлено»; копия в «Отправленных» появляется
 * через 5–20 с (её кладёт сервер или мы), поэтому после отправки
 * синхронизируемся ещё несколько раз.
 */
class ComposeController(
    private val app: Context,
    private val hub: MailHub,
    private val conversation: ConversationController,
    private val scope: CoroutineScope,
    private val syncSoon: () -> Unit,
) {
    val ui = MutableStateFlow(ComposerUi())

    /** Свои адреса всех учёток с их владельцами (маршрутизация по From, §5). */
    private val owners: StateFlow<List<Pair<String, Identity>>> = hub.identities.stateIn(scope, SharingStarted.Eagerly, emptyList())

    /** Свои адреса для выбора «От кого» — без повторов. */
    val identities: StateFlow<List<Identity>> = hub.identities
        .map { l -> l.map { it.second }.distinctBy { it.email.lowercase() } }
        .stateIn(scope, SharingStarted.Eagerly, emptyList())

    /**
     * После нового письма — открыть беседу с этими адресатами, когда появится.
     * Поток, а не поле: беседа могла прийти (WS `message_sent` → синк) раньше
     * ответа на отправку — тогда переход делается по самой цели.
     */
    val sentTarget = MutableStateFlow<Set<String>?>(null)

    /** Заглушки ответов, которые не ушли (IMAP): уходят со следующей отправкой. */
    private val failedStubs = mutableListOf<String>()

    private var stubSeq = 0

    fun edit(f: (Draft) -> Draft) = ui.update { it.copy(draft = f(it.draft), error = null) }

    fun clearError() = ui.update { it.copy(error = null) }

    fun showError(msg: String) = ui.update { it.copy(error = msg) }
    fun toggleExpanded() = ui.update { it.copy(expanded = !it.expanded) }
    fun replyTo(b: MessageBody?) = ui.update { it.copy(replyTo = b) }

    /** Сменилась беседа — явный ответ и переопределения не переезжают в другую. */
    fun reset() = ui.update { ComposerUi(sheet = it.sheet) }

    fun openForward(b: MessageBody) = ui.update { it.copy(sheet = ComposeSheet(Branch.Forward(b, conversation.current?.accountKey.orEmpty()), Draft())) }

    fun editSheet(f: (Draft) -> Draft) = ui.update { u -> u.copy(sheet = u.sheet?.let { it.copy(draft = f(it.draft), error = null) }) }

    fun closeSheet() = ui.update { it.copy(sheet = null) }

    fun consumeTarget() {
        sentTarget.value = null
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
                // Новая попытка заменяет пузыри прежних неудач (их текст и так был в композере).
                failedStubs.forEach { conversation.js.trySend("ddm.unstub(\"$it\")") }
                failedStubs.clear()
                conversation.js.trySend("ddm.stub(${stubJson(key, draft, built.request.html)})")
                ui.update { ComposerUi(sheet = it.sheet) }
                scope.launch {
                    try {
                        val viaServer = hub.send(built.request, owners.value, prefer = c.accountKey)
                        ui.update { it.copy(sentFlash = true) }
                        afterSend(viaServer)
                        delay(2000)
                        ui.update { it.copy(sentFlash = false) }
                    } catch (e: Exception) {
                        if (!e.isMailError()) throw e
                        // SMTP своей учётки отказал — пузырь остаётся, красным
                        // «Не доставлено»; прочие ошибки (сервер ddmail не
                        // принял) — пузырь убирается, причина плашкой.
                        val smtp = e is SmtpFailed
                        if (smtp) {
                            failedStubs += key
                            conversation.js.trySend("ddm.stubDelivery(\"$key\",${deliveryJson(Delivery.FAILED, e.errorText(), emptyList())})")
                        } else {
                            conversation.js.trySend("ddm.unstub(\"$key\")")
                        }
                        // Текст возвращается, если композер пуст (не затираем набранное заново).
                        ui.update { cur ->
                            cur.copy(
                                draft = if (cur.draft.isBlank()) draft.reloaded() else cur.draft,
                                replyTo = cur.replyTo ?: u.replyTo,
                                error = if (smtp) null else sendError(e),
                            )
                        }
                    }
                }
            }
        }
    }

    /**
     * Новое письмо из новой беседы (ветка 1): адресат — [target], которого
     * ввели в поиск; непустое «Кому» под «@» его заменяет (§1 десктопа).
     * После отправки беседа с адресатами откроется сама, как только придёт.
     */
    fun sendNew(target: String) {
        val u = ui.value
        val draft = if (u.draft.to.isBlank()) u.draft.copy(to = target) else u.draft
        when (val built = build(Branch.New, draft, identities.value)) {
            is Built.Refused -> ui.update { it.copy(error = built.reason) }
            is Built.Ok -> {
                val text = u.draft.text.trim().ifBlank {
                    (u.draft.inline.map { "Картинка" } + u.draft.attachments.map { it.name }).joinToString(", ")
                }
                val key = "n${++stubSeq}"
                // Текст и вложения уходят, переопределения адресата/темы остаются;
                // пузыри прежних неудач заменяет эта попытка.
                ui.update { cur ->
                    cur.copy(
                        draft = cur.draft.bodyCleared(),
                        pendingNew = cur.pendingNew.filter { it.failed == null } + PendingNew(key, text),
                        error = null,
                    )
                }
                scope.launch {
                    try {
                        val viaServer = hub.send(built.request, owners.value)
                        // Беседа нового письма — набор адресатов: «Кому» и «Копия»
                        // (§4 десктопа: bcc в набор не входит).
                        sentTarget.value = targetOf(built.request)
                        ui.update { cur -> cur.copy(sentFlash = true, pendingNew = cur.pendingNew.map { if (it.key == key) it.copy(sent = true) else it }) }
                        afterSend(viaServer)
                        delay(2000)
                        ui.update { it.copy(sentFlash = false) }
                    } catch (e: Exception) {
                        if (!e.isMailError()) throw e
                        val smtp = e is SmtpFailed
                        ui.update { cur ->
                            cur.copy(
                                draft = if (cur.draft.isBlank()) u.draft.reloaded() else cur.draft,
                                pendingNew = if (smtp) {
                                    cur.pendingNew.map { if (it.key == key) it.copy(failed = e.errorText()) else it }
                                } else {
                                    cur.pendingNew.filter { it.key != key }
                                },
                                error = if (smtp) null else sendError(e),
                            )
                        }
                    }
                }
            }
        }
    }

    /** Пересылка (ветка 0) — из отдельного окна. */
    fun sendSheet() {
        val sheet = ui.value.sheet ?: return
        if (sheet.busy) return
        ui.update { it.copy(sheet = sheet.copy(busy = true, error = null)) }
        scope.launch {
            val fwdBranch = sheet.branch as? Branch.Forward
            val fwd = fwdBranch?.original
            val (fwdText, fwdHtml) = fwd?.let(::forwardBlock) ?: ("" to "")
            // Пересылка несёт вложения исходного письма.
            val extra = try {
                fwd?.attachments.orEmpty().map { a ->
                    val bin = hub.attachment(fwdBranch!!.accountKey, fwd!!.uid, a.index)
                    PendingAttachment(a.filename, a.mimeType.ifBlank { bin.contentType }, bin.bytes)
                }
            } catch (e: Exception) {
                if (!e.isMailError()) throw e
                ui.update { u -> u.copy(sheet = u.sheet?.copy(busy = false, error = "Не удалось забрать вложения пересылаемого письма: ${sendError(e)}")) }
                return@launch
            }
            when (val built = build(sheet.branch, sheet.draft, identities.value, fwdHtml, fwdText, extra)) {
                is Built.Refused -> ui.update { u -> u.copy(sheet = u.sheet?.copy(busy = false, error = built.reason)) }
                is Built.Ok -> try {
                    val viaServer = hub.send(built.request, owners.value, prefer = fwdBranch?.accountKey)
                    ui.update { it.copy(sheet = null, sentFlash = true) }
                    afterSend(viaServer)
                    delay(2000)
                    ui.update { it.copy(sentFlash = false) }
                } catch (e: Exception) {
                    if (!e.isMailError()) throw e
                    ui.update { u -> u.copy(sheet = u.sheet?.copy(busy = false, error = sendError(e))) }
                }
            }
        }
    }

    /**
     * Подтянуть копию в «Отправленных» (и беседу нового письма). Сервер ddmail
     * кладёт её при приёме ([viaServer]): она есть уже к ответу на `/send`, и
     * хватает синка сразу; основной сигнал — WS `message_sent` (его ловит
     * `LiveUpdates`), пара повторов — подстраховка без сокета. IMAP-учётке
     * копию кладёт её сервер или мы через 5–20 с — там повторы с затухающими
     * паузами, как `schedule_post_send_refetch` десктопа (2.5 с → 7 → 15 → 30 → 60).
     */
    private fun afterSend(viaServer: Boolean) {
        syncSoon()
        scope.launch {
            for (d in if (viaServer) SERVER_REFETCH_MS else POST_SEND_REFETCH_MS) {
                delay(d)
                syncSoon()
            }
        }
    }

    private fun sendError(e: Exception): String = when (e) {
        // SMTP IMAP-учётки отказал — тем же словом, что пометка на пузыре.
        is SmtpFailed -> "Не доставлено: ${e.errorText()}."
        is ApiException.Server -> "Не отправлено: ${if (e.code == 413) "вложения больше лимита сервера" else "сервер ответил ошибкой ${e.code}"}."
        else -> "Не отправлено: ${e.errorText()}."
    }

    /**
     * Заглушка выглядит как письмо (десктоп §2): тот же HTML, что уходит на
     * сервер, картинки тела вшиты `data:`-URI — `cid:` у заглушки не к чему
     * разрешать.
     */
    private fun stubJson(key: String, d: Draft, html: String) = JsonObject(
        mapOf(
            "key" to JsonPrimitive(key),
            "text" to JsonPrimitive(d.text),
            "html" to JsonPrimitive(stubHtml(html, d.inline)),
            "attachments" to JsonArray(
                d.attachments.map { JsonObject(mapOf("filename" to JsonPrimitive(it.name), "size" to JsonPrimitive(it.size))) },
            ),
        ),
    ).toString()

    private companion object {
        /** Лимит сервера — 30 МБ на запрос в base64; сырых байт — около 22. */
        const val MAX_TOTAL = 22L shl 20

        /** Паузы между повторными синками после отправки через IMAP-учётку (накопительно ~2 мин). */
        val POST_SEND_REFETCH_MS = listOf(2_500L, 7_000L, 15_000L, 30_000L, 60_000L)

        /** После приёма сервером ddmail: копия уже есть — повторы только на случай без сокета. */
        val SERVER_REFETCH_MS = listOf(2_500L, 7_000L)
    }
}

/** Набор адресатов нового письма — по нему узнаём его беседу (§2 десктопа). */
internal fun targetOf(req: ru.letotam.ddmail.api.SendRequest): Set<String> =
    (req.to + req.cc).map(::bareAddress).filter { it.isNotEmpty() }.toSet()

/** HTML тела для заглушки: `cid:` картинок → `data:`-URI. */
internal fun stubHtml(html: String, inline: List<PendingAttachment>): String =
    inline.fold(html) { acc, a ->
        val cid = a.cid ?: return@fold acc
        acc.replace("cid:$cid", "data:${a.mime};base64," + java.util.Base64.getEncoder().encodeToString(a.bytes))
    }
