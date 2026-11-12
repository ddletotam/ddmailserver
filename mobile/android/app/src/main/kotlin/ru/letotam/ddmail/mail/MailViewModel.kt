package ru.letotam.ddmail.mail

import android.content.Context
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ru.letotam.ddmail.DdmailApp
import ru.letotam.ddmail.api.MessageBody
import ru.letotam.ddmail.compose.ComposeController
import ru.letotam.ddmail.conversation.ConversationController
import ru.letotam.ddmail.sync.Conversation
import ru.letotam.ddmail.sync.FoldersDoc
import ru.letotam.ddmail.sync.SYSTEM_SPAM
import ru.letotam.ddmail.sync.MailHub
import ru.letotam.ddmail.sync.SpamOutcome
import ru.letotam.ddmail.sync.errorText
import ru.letotam.ddmail.sync.isMailError

private val Context.uiPrefs by preferencesDataStore(name = "ui")
private val ACTIVE_FOLDER = stringPreferencesKey("active_folder")
private val LAST_CONV = stringPreferencesKey("last_conversation")

/** Таб полосы папок: индекс в документе, название, иконка, счётчик. */
data class FolderTab(val index: Int, val id: String, val name: String, val icon: String, val unread: Int)

data class MailState(
    val tabs: List<FolderTab> = emptyList(),
    /** Индекс выбранной папки в документе; `null` — инбокс. */
    val active: Int? = null,
    val inboxUnread: Int = 0,
    val conversations: List<Conversation> = emptyList(),
    val loaded: Boolean = false,
    /** Выбрана папка «Спам»: смотреть и «Не спам», других действий нет. */
    val inSpam: Boolean = false,
)

/** Подтверждение «В спам»: беседы и что умеют их учётки. */
data class SpamAsk(val refs: List<String>, val rules: Boolean)

/**
 * Список бесед всех учёток: кэш Room → видимая часть выбранной папки. Сеть
 * только пополняет кэш (источники [MailHub]); экран рисует из кэша сразу и не
 * знает, из какой учётки беседа (mobile-external-accounts §1). Беседы
 * опознаются по `Conversation.ref` (учётка + id).
 */
class MailViewModel(
    private val app: DdmailApp,
    private val hub: MailHub,
) : ViewModel() {
    private val activeId = MutableStateFlow<String?>(null)

    /** Строка поиска; `null` — поиск закрыт. */
    val query = MutableStateFlow<String?>(null)
    val syncing = MutableStateFlow(false)

    /** Открытая беседа — по `ref`, не по позиции: список меняется дельтами. */
    val selected = MutableStateFlow<String?>(null)

    val conversation = ConversationController(app, app.settings, hub, viewModelScope)

    /** Вложение, на котором открыто меню (uid письма, индекс). */
    val attachmentMenu = MutableStateFlow<Pair<Long, Int>?>(null)

    val compose = ComposeController(app, hub, conversation, viewModelScope, ::sync)

    val live = LiveUpdates(hub, viewModelScope, ::sync) { app.calendarChanged.tryEmit(Unit) }
    val avatars = Avatars(app, hub)

    /** Учётки и их состояния — для шапки и плашек «нужен вход». */
    val sources = hub.sources
    val statuses = hub.statuses

    /** Цвета строк по группам адресов (как у десктопа). */
    val identityColors: StateFlow<ru.letotam.ddmail.sync.IdentityColors> =
        hub.identityColors.stateIn(viewModelScope, SharingStarted.Eagerly, ru.letotam.ddmail.sync.IdentityColors())

    /** Письмо, на котором открыто меню пузыря. */
    val bubbleMenu = MutableStateFlow<MessageBody?>(null)

    fun showBubbleMenu(uid: Long) {
        bubbleMenu.value = conversation.body(uid)
    }

    /** Полный список (не только видимая папка): открытая беседа ищется в нём. */
    private var all: List<Conversation> = emptyList()
    private var doc: FoldersDoc = FoldersDoc()
    private var lastConv: String? = null

    val state: StateFlow<MailState> =
        combine(hub.conversations, hub.folders, activeId, query) { convs, doc, id, q -> build(convs, doc, id, q) }
            .stateIn(viewModelScope, SharingStarted.Eagerly, MailState())

    init {
        viewModelScope.launch {
            val p = app.uiPrefs.data.first()
            activeId.value = p[ACTIVE_FOLDER]
            lastConv = p[LAST_CONV]
            tryRestore()
        }
        viewModelScope.launch { hub.folders.collect { doc = it } }
        viewModelScope.launch {
            hub.conversations.collect { list ->
                all = list
                tryRestore()
                selected.value?.let { ref -> list.firstOrNull { it.ref == ref }?.let(conversation::updated) }
                // Отправили новое письмо — перейти в его беседу, как только она появилась.
                compose.sentTarget?.let { target ->
                    list.firstOrNull { c -> matchesTarget(c, target) }?.let { c ->
                        compose.consumeTarget()
                        open(c.ref)
                    }
                }
            }
        }
        sync()
    }

    /** Обновление жестом «потянуть»: индикатор — только для него. */
    val pulling = MutableStateFlow(false)

    fun pullRefresh() {
        pulling.value = true
        sync()
    }

    /** Синхронизацию попросили, пока шла другая — повторить по её окончании. */
    private var again = false

    fun sync() {
        if (syncing.value) {
            again = true
            return
        }
        syncing.value = true
        viewModelScope.launch {
            try {
                hub.syncAll()
                afterSync()
            } finally {
                syncing.value = false
                pulling.value = false
            }
            if (again) {
                again = false
                sync()
            }
        }
    }

    /** После синка: уведомления о новом и докачка тел (если пользователь разрешил). */
    private suspend fun afterSync() {
        app.notifier.check(all, doc, selected.value)
        if (prefetching) return
        if (app.settings.prefetchAllowedNow()) {
            prefetching = true
            viewModelScope.launch {
                try {
                    for (s in hub.sources.value) {
                        try {
                            s.prefetchBodies(allowed = { app.settings.prefetchAllowedNow() })
                        } catch (e: Exception) {
                            if (!e.isMailError()) throw e
                            hub.noteError(s.accountKey, e)
                        }
                    }
                } finally {
                    prefetching = false
                }
            }
        }
    }

    private var prefetching = false

    override fun onCleared() {
        live.stop()
    }

    // ── Множественный выбор (контракт §5) ──

    /** Выбранные беседы — по `ref`, не по позиции: дельта не сбивает выбор. */
    val picked = MutableStateFlow<Set<String>>(emptySet())

    val snackHost = androidx.compose.material3.SnackbarHostState()

    /** Сообщение об итоге действия над выбором (Snackbar). */
    val notice = MutableStateFlow<String?>(null)

    fun togglePick(id: String) = picked.update { if (id in it) it - id else it + id }

    fun pickAll() = picked.update { state.value.conversations.map { it.ref }.toSet() }

    fun clearPicks() {
        picked.value = emptySet()
    }

    private fun pickedConvs(): List<Conversation> {
        val ids = picked.value
        return all.filter { it.ref in ids }
    }

    fun markPicked(read: Boolean) {
        val convs = pickedConvs()
        clearPicks()
        viewModelScope.launch {
            val r = runCatching { convs.forEach { if (read) hub.markRead(it) else hub.markUnread(it) } }
            notice.value = r.fold(
                { if (read) "Прочитано: ${convs.size}" else "Непрочитано: ${convs.size}" },
                { "Не удалось: ${it.errorText()}" },
            )
        }
    }

    /** Свайп вправо: прочитано ⇄ непрочитано. */
    fun toggleRead(ref: String) {
        val c = all.firstOrNull { it.ref == ref } ?: return
        viewModelScope.launch {
            runCatching { if (c.unreadCount > 0) hub.markRead(c) else hub.markUnread(c) }
                .onFailure { notice.value = "Не удалось: ${it.errorText()}" }
        }
    }

    /** В папку (`null` — во «Входящие») — одной правкой документа на весь выбор. */
    fun movePicked(folderId: String?) {
        val convs = pickedConvs()
        clearPicks()
        viewModelScope.launch {
            val r = runCatching { hub.assignFolder(convs, folderId) }
            val where = folderId?.let { id -> doc.folders.firstOrNull { it.id == id }?.name } ?: "Входящие"
            notice.value = r.fold({ "Перенесено в «$where»: ${convs.size}" }, { "Не удалось перенести: ${it.errorText()}" })
        }
    }

    /** Сколько бесед и писем удалит «Удалить» — для подтверждения. */
    fun pickedSummary(): Pair<Int, Int> = pickedConvs().let { c -> c.size to c.sumOf { it.messages.size } }

    fun deletePicked() {
        val convs = pickedConvs()
        clearPicks()
        if (convs.any { it.ref == selected.value }) open(null)
        viewModelScope.launch {
            val r = runCatching { hub.deleteConversations(convs) }
            notice.value = r.fold({ "Удалено бесед: ${convs.size}" }, { "Не удалось удалить: ${it.errorText()}" })
            sync()
        }
    }

    /**
     * Беседы, ждущие подтверждения «В спам»: выбранные или открытая (кнопка в
     * шапке беседы). `null` — диалог закрыт.
     */
    val spamAsk = MutableStateFlow<SpamAsk?>(null)

    fun askSpamPicked() = ask(picked.value.toList())

    fun askSpam(ref: String) = ask(listOf(ref))

    /**
     * «В спам» есть, если у всех учёток выбора есть куда (у IMAP — папка Junk);
     * «адрес/домен» — только если все они ddmail (правила держит только сервер).
     */
    private fun ask(refs: List<String>) {
        viewModelScope.launch {
            val (can, rules) = hub.spamAbility(all.filter { it.ref in refs && !it.spam })
            if (!can) {
                notice.value = "В ящике нет папки «Спам» — некуда переносить."
                return@launch
            }
            spamAsk.value = SpamAsk(refs, rules)
        }
    }

    /** Сколько бесед и писем уйдёт в спам — для подтверждения. */
    fun spamSummary(ids: Collection<String>): Pair<Int, Int> =
        all.filter { it.ref in ids && !it.spam }.let { c -> c.size to c.sumOf { it.messages.size } }

    /**
     * «В спам» (контракт §5): письма бесед удаляются, отправители блокируются —
     * по адресу или по домену ([scope] `address` / `domain`), как на десктопе.
     */
    fun spamConfirmed(scope: String) {
        val ids = spamAsk.value?.refs ?: return
        spamAsk.value = null
        val convs = all.filter { it.ref in ids && !it.spam }
        clearPicks()
        if (convs.isEmpty()) return
        if (convs.any { it.ref == selected.value }) open(null)
        viewModelScope.launch {
            val r = runCatching { hub.spam(convs, scope) }
            notice.value = r.fold(
                { list -> list.joinToString("; ") { spamText(it) } },
                { "Не удалось отправить в спам: ${it.errorText()}" },
            )
            sync()
        }
    }

    /**
     * «Не спам» выбранным беседам: письма — во «Входящие» на сервере.
     * [trust] — «Не спам и доверять отправителю»: сервер заводит allow-правило
     * на отправителя и возвращает из спама всё остальное от него.
     */
    fun notSpamPicked(trust: Boolean = false) {
        val convs = pickedConvs().filter { it.spam }
        clearPicks()
        if (convs.isEmpty()) return
        if (convs.any { it.ref == selected.value }) open(null)
        viewModelScope.launch {
            val r = runCatching { hub.restoreSpam(convs, trust) }
            val ok = if (trust) "Во «Входящие», отправителям доверяем: бесед ${convs.size}" else "Во «Входящие»: бесед ${convs.size}"
            notice.value = r.fold({ ok }, { "Не удалось вернуть из спама: ${it.errorText()}" })
            sync()
        }
    }

    /** Папка «Спам» скрыта всегда; попасть в неё — из меню шапки. */
    fun openSpam() {
        doc.spamIndex?.let { selectFolder(doc.folders[it].id) }
    }

    /**
     * Папки для «В папку…» — все, включая скрытые (как `folder-targets`
     * десктопа), кроме системных: в «Спам» руками не кладут.
     */
    fun folderTargets(): List<Pair<String, String>> = doc.folders.filter { !it.isSystem }.map { it.id to (if (it.icon.isBlank()) it.name else "${it.icon} ${it.name}") }

    /** `null` — «Входящие». Выбор помнится между запусками (контракт §5). */
    fun selectFolder(id: String?) {
        // Другой таб — другой набор строк: выбор снимается.
        clearPicks()
        activeId.value = id
        viewModelScope.launch { app.uiPrefs.edit { p -> if (id == null) p.remove(ACTIVE_FOLDER) else p[ACTIVE_FOLDER] = id } }
    }

    /**
     * Открыть беседу. Беседа в другой папке — переключаемся на её папку (как
     * `folders_ui::reveal` десктопа): иначе открытие из уведомления или после
     * отправки молча ничего бы не показало.
     */
    fun open(id: String?) {
        if (id == selected.value) return
        val c = id?.let { i -> all.firstOrNull { it.ref == i } }
        if (c != null) {
            val f = doc.folderOf(c)
            val fid = f?.let { doc.folders[it].id }
            if (fid != activeId.value) selectFolder(fid)
        }
        selected.value = id
        app.openConversation = id
        id?.let { ru.letotam.ddmail.background.MailNotifier.cancel(app, it) }
        compose.reset()
        conversation.open(c)
        id?.let { v -> viewModelScope.launch { app.uiPrefs.edit { it[LAST_CONV] = v } } }
    }

    private var wide = false

    /** Раскладка сменилась; широкая — беседа видна сразу. */
    fun setWide(v: Boolean) {
        wide = v
        tryRestore()
    }

    /**
     * На раскладке, где беседа видна сразу, — открыть ту, на которой
     * закончили (§5). Один раз: когда прочитаны настройки и пришёл список.
     */
    private fun tryRestore() {
        val id = lastConv ?: return
        if (!wide || selected.value != null || all.isEmpty()) return
        lastConv = null
        if (all.any { it.ref == id }) open(id)
    }

    private fun build(convs: List<Conversation>, doc: FoldersDoc, id: String?, q: String?): MailState {
        // Поиск — по всем папкам (как «Диалоги» десктопа, §5).
        // Спам в поиск не попадает: он виден только в своей папке.
        if (!q.isNullOrBlank()) return MailState(emptyList(), null, 0, searchConversations(convs.filter { !it.spam }, q), loaded = true)
        // Удалённая папка — снова инбокс. Скрытая видна табом, только пока
        // в неё привели (открытие беседы из неё).
        val active = doc.folders.indexOfFirst { it.id == id }.takeIf { it >= 0 }
        val (counts, inbox) = doc.unreadCounts(convs)
        val tabs = doc.folders.mapIndexedNotNull { i, f ->
            // У «Спама» счётчика нет: непрочитанный спам — не повод на него смотреть.
            if (f.hidden && i != active) null else FolderTab(i, f.id, f.name, f.icon, if (f.system == SYSTEM_SPAM) 0 else counts[i])
        }
        val inSpam = active != null && active == doc.spamIndex
        return MailState(tabs, active, inbox, doc.filter(convs, active), loaded = true, inSpam = inSpam)
    }
}

/**
 * Беседа нового письма: набор собеседников совпал с адресатами. Письмо себе
 * сервер отдаёт беседой без собеседников — тогда сверяем по её id.
 */
/** Итог «В спам» словами: у ddmail — правило, у IMAP — перенос в «Спам». */
internal fun spamText(o: SpamOutcome): String = when (o) {
    is SpamOutcome.Rule -> {
        val r = o.outcome
        val what = (if (r.ruleType == "domain") "домен " else "") + r.ruleValue
        val more = if (r.ruleCount > 1) " и ещё ${r.ruleCount - 1}" else ""
        "✓ В спам: $what$more, удалено писем: ${r.deleted}"
    }
    is SpamOutcome.Moved -> "✓ В спам: перенесено писем: ${o.messages}"
}

internal fun matchesTarget(c: Conversation, target: Set<String>): Boolean {
    val cps = c.counterparts.map { it.addr.lowercase() }.filter { it.isNotEmpty() }.toSet()
    if (cps.isNotEmpty()) return cps == target
    val id = c.id.lowercase()
    return target.all { id.contains(it) }
}

/**
 * Локальный поиск по диалогам (контракт §5): имя, адрес любого участника,
 * тема последнего письма; порядок — сначала совпавшие по имени, затем по
 * адресу, затем по теме, внутри — как в списке. Регистр складывается в
 * Kotlin (кириллица складывается правильно).
 */
internal fun searchConversations(convs: List<Conversation>, q: String): List<Conversation> {
    val needle = q.trim().lowercase()
    if (needle.isEmpty()) return convs
    val byName = convs.filter { it.label.lowercase().contains(needle) }
    val byAddr = convs.filter { c -> c !in byName && (c.counterparts.any { it.addr.lowercase().contains(needle) || it.name.lowercase().contains(needle) } || c.receivedBy.lowercase().contains(needle)) }
    val bySubj = convs.filter { c -> c !in byName && c !in byAddr && c.lastSubject.lowercase().contains(needle) }
    return byName + byAddr + bySubj
}
