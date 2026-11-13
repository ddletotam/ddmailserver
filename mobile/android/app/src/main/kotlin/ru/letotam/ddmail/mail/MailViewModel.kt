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
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.debounce
import kotlinx.coroutines.flow.distinctUntilChanged
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
import ru.letotam.ddmail.sync.MailSource
import ru.letotam.ddmail.sync.SpamOutcome
import ru.letotam.ddmail.sync.errorText
import ru.letotam.ddmail.sync.isMailError

private val Context.uiPrefs by preferencesDataStore(name = "ui")
private val ACTIVE_FOLDER = stringPreferencesKey("active_folder")
private val LAST_CONV = stringPreferencesKey("last_conversation")

/**
 * Таб полосы папок: индекс в документе, название, иконка, счётчик.
 * [hidden] — скрытая папка, показанная по «Показать скрытые» (бледнее);
 * [system] — «Спам»: без «Настроить…» и «Удалить».
 */
data class FolderTab(
    val index: Int,
    val id: String,
    val name: String,
    val icon: String,
    val unread: Int,
    val hidden: Boolean = false,
    val system: Boolean = false,
)

data class MailState(
    val tabs: List<FolderTab> = emptyList(),
    /** Индекс выбранной папки в документе; `null` — инбокс. */
    val active: Int? = null,
    val inboxUnread: Int = 0,
    val conversations: List<Conversation> = emptyList(),
    val loaded: Boolean = false,
    /** Выбрана папка «Спам»: смотреть и «Не спам», других действий нет. */
    val inSpam: Boolean = false,
    /** Скрытых папок (кроме «Спама» — он табом всегда) — для «Показать скрытые (N)». */
    val hiddenCount: Int = 0,
    /** «Показать скрытые» включено (на сессию). */
    val showHidden: Boolean = false,
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

    /** Строка поиска (первая строка экрана); непустая — открыта выпадашка. */
    val query = MutableStateFlow("")
    val syncing = MutableStateFlow(false)

    /** Открытая беседа — по `ref`, не по позиции: список меняется дельтами. */
    val selected = MutableStateFlow<String?>(null)

    /**
     * Новая беседа (compose-режим десктопа): адресат, введённый в поиск.
     * Пустая лента с композером внизу; `null` — не открыта.
     */
    val newChat = MutableStateFlow<String?>(null)

    val conversation = ConversationController(app, app.settings, hub, viewModelScope)

    /** Вложение, на котором открыто меню (uid письма, индекс). */
    val attachmentMenu = MutableStateFlow<Pair<Long, Int>?>(null)

    val compose = ComposeController(app, hub, conversation, viewModelScope, ::sync)

    val live = LiveUpdates(
        hub, viewModelScope, ::sync,
        onCalendar = { app.calendarChanged.tryEmit(Unit) },
        onDelivery = conversation::delivery,
    )
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

    /** «Показать скрытые» — на сессию, как у десктопа (не запоминается). */
    private val showHidden = MutableStateFlow(false)

    /** Документ папок — для раздела «Папки» настроек и редактора папки. */
    val folders: StateFlow<FoldersDoc> = hub.folders.stateIn(viewModelScope, SharingStarted.Eagerly, FoldersDoc())

    /** «Показывать папки» (локальный выключатель). */
    val foldersOn: StateFlow<Boolean> = app.settings.foldersOn.stateIn(viewModelScope, SharingStarted.Eagerly, true)

    val state: StateFlow<MailState> =
        combine(hub.conversations, hub.folders, activeId, showHidden, foldersOn) { convs, doc, id, hidden, on -> build(convs, doc, id, hidden, on) }
            .stateIn(viewModelScope, SharingStarted.Eagerly, MailState())

    /** Адресная книга — для секции «Контакты» поиска (кладёт оболочка). */
    private val addressBook = MutableStateFlow<List<ru.letotam.ddmail.api.ContactDto>>(emptyList())

    fun setAddressBook(list: List<ru.letotam.ddmail.api.ContactDto>) {
        addressBook.value = list
    }

    /** Серверная часть поиска («Сообщения») — по последней строке, с паузой на набор. */
    private val serverHits = MutableStateFlow<Pair<String, List<ru.letotam.ddmail.sync.MessageHit>>?>(null)

    /**
     * Выпадашка поиска: «Диалоги» и «Контакты» — локально на каждое нажатие,
     * «Сообщения» — с источников (у кого поиск есть), когда ответят.
     */
    val search: StateFlow<SearchResults> =
        combine(query, hub.conversations, addressBook, serverHits) { q, convs, book, server ->
            val t = q.trim()
            if (t.isEmpty()) return@combine SearchResults()
            // Спам в поиск не попадает: он виден только в своей папке.
            val ham = convs.filter { !it.spam }
            val dialogs = searchConvHits(ham, t).take(MAX_CONV_HITS)
            val hitAddrs = dialogs.flatMap { h -> h.conv.counterparts.map { it.addr.lowercase() } }.toSet()
            val fresh = server?.first == t
            SearchResults(
                query = t,
                composeEmail = parseEmailLike(t).orEmpty(),
                convs = dialogs,
                contacts = searchContacts(book, hitAddrs, t),
                messages = if (fresh) mapMessageHits(server!!.second, ham) else emptyList(),
                loading = !fresh,
            )
        }.stateIn(viewModelScope, SharingStarted.Eagerly, SearchResults())

    init {
        viewModelScope.launch {
            val p = app.uiPrefs.data.first()
            activeId.value = p[ACTIVE_FOLDER]
            lastConv = p[LAST_CONV]
            tryRestore()
        }
        viewModelScope.launch { hub.folders.collect { doc = it } }
        // Серверный поиск — после паузы в наборе; новая строка отменяет прошлый запрос.
        viewModelScope.launch {
            @OptIn(kotlinx.coroutines.FlowPreview::class)
            query.map { it.trim() }.distinctUntilChanged().debounce(350).collectLatest { q ->
                if (q.length < 2) {
                    serverHits.value = q to emptyList()
                    return@collectLatest
                }
                serverHits.value = q to hub.searchMessages(q, MAX_MSG_HITS)
            }
        }
        // Добавили учётку (или поправили настройки) — сразу её первый синк.
        viewModelScope.launch {
            var known = emptySet<MailSource>()
            hub.sources.collect { list ->
                val fresh = list.filter { it !in known }
                if (known.isNotEmpty() && fresh.isNotEmpty()) sync()
                known = list.toSet()
            }
        }
        viewModelScope.launch {
            hub.conversations.collect { list ->
                all = list
                tryRestore()
                selected.value?.let { ref -> list.firstOrNull { it.ref == ref }?.let(conversation::updated) }
                openSentTarget()
            }
        }
        // Цель могла появиться позже беседы: сервер ddmail кладёт копию при
        // приёме, и синк по WS `message_sent` иногда успевает до ответа `/send`.
        viewModelScope.launch { compose.sentTarget.collect { if (it != null) openSentTarget() } }
        sync()
    }

    /** Отправили новое письмо — перейти в его беседу, как только она есть в списке. */
    private fun openSentTarget() {
        val target = compose.sentTarget.value ?: return
        val own = compose.identities.value.map { it.email.lowercase() }.toSet()
        all.firstOrNull { c -> matchesTarget(c, target, own) }?.let { c ->
            compose.consumeTarget()
            open(c.ref)
        }
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

    // ── Поиск и новая беседа (docs/mobile-layout.md «Новое письмо») ──

    fun clearSearch() {
        query.value = ""
    }

    /**
     * Новая беседа с [email] — «Написать по адресу …» и «Написать» из
     * карточки контакта: пустая лента, композер внизу (как compose-режим
     * десктопа). Открытая беседа закрывается, набранное не переезжает.
     */
    fun startNew(email: String) {
        val addr = email.trim().lowercase()
        if (addr.isEmpty()) return
        clearSearch()
        clearPicks()
        open(null)
        compose.reset()
        newChat.value = addr
    }

    fun closeNew() {
        if (newChat.value == null) return
        newChat.value = null
        compose.reset()
    }

    fun pickSearchConv(ref: String) {
        clearSearch()
        open(ref)
    }

    /**
     * Контакт: есть беседа один на один с ним — открыть самую свежую (в
     * любой папке); нет — новая беседа с его адресом. Как у десктопа.
     */
    fun pickSearchContact(email: String) {
        val target = email.lowercase()
        val best = all.filter { !it.spam && it.counterparts.firstOrNull()?.addr?.lowercase() == target }.maxByOrNull { it.lastDateTs }
        if (best != null) pickSearchConv(best.ref) else startNew(email)
    }

    /**
     * Enter в поле поиска: адрес — новая беседа с ним (первая строка
     * выпадашки), иначе — первая найденная строка.
     */
    fun submitSearch() {
        val r = search.value
        when {
            r.composeEmail.isNotEmpty() -> startNew(r.composeEmail)
            r.convs.isNotEmpty() -> pickSearchConv(r.convs.first().conv.ref)
            r.contacts.isNotEmpty() -> pickSearchContact(r.contacts.first().email)
            r.messages.isNotEmpty() -> pickSearchConv(r.messages.first().ref)
        }
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
        // Открыли настоящую беседу — заготовка новой уходит.
        if (id != null) newChat.value = null
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

    private fun build(convs: List<Conversation>, doc: FoldersDoc, id: String?, showHidden: Boolean, on: Boolean): MailState {
        // Папки выключены («Показывать папки») или их нет — список ровно как
        // без них: всё, кроме спама (десктоп `visible_view`).
        if (!on || doc.folders.isEmpty()) {
            return MailState(conversations = convs.filter { !it.spam }, loaded = true)
        }
        // Удалённая папка — снова инбокс. Скрытая видна табом, пока включено
        // «Показать скрытые» или в неё привели (открытие беседы из неё).
        // «Спам» — таб полосы всегда (mobile-layout.md): меню, через которое в
        // него входили, больше нет.
        val active = doc.folders.indexOfFirst { it.id == id }.takeIf { it >= 0 }
        val (counts, inbox) = doc.unreadCounts(convs)
        val tabs = doc.folders.mapIndexedNotNull { i, f ->
            val spam = f.system == SYSTEM_SPAM
            // У «Спама» счётчика нет: непрочитанный спам — не повод на него смотреть.
            if (f.hidden && !spam && !showHidden && i != active) null
            else FolderTab(i, f.id, f.name, f.icon, if (spam) 0 else counts[i], hidden = f.hidden && !spam, system = f.isSystem)
        }
        val inSpam = active != null && active == doc.spamIndex
        val hiddenCount = doc.folders.count { it.hidden && !it.isSystem }
        return MailState(tabs, active, inbox, doc.filter(convs, active), loaded = true, inSpam = inSpam, hiddenCount = hiddenCount, showHidden = showHidden)
    }

    // ── Папки: меню полосы табов и настройки (десктоп §4 «Папки диалогов») ──

    fun toggleShowHidden() = showHidden.update { !it }

    /** Открыт редактор папки (id); `null` — закрыт. Рисует его оболочка поверх всего. */
    val folderEditor = MutableStateFlow<String?>(null)

    fun setFoldersOn(v: Boolean) {
        viewModelScope.launch { app.settings.setFoldersOn(v) }
    }

    /**
     * «Все прочитаны»: всё непрочитанное в диалогах папки — локально сразу,
     * на сервере следом (по учётке диалога).
     */
    fun markFolderRead(id: String) {
        val d = folders.value
        val i = d.indexOf(id) ?: return
        val convs = all.filter { it.unreadCount > 0 && d.folderOf(it) == i }
        if (convs.isEmpty()) return
        viewModelScope.launch {
            val r = runCatching { convs.forEach { hub.markRead(it) } }
            notice.value = r.fold({ "Прочитано бесед: ${convs.size}" }, { "Не удалось: ${it.errorText()}" })
        }
    }

    /** «Новая папка»: создаётся и сразу открывается редактор — назвать её. */
    fun newFolder(): String {
        val id = folders.value.newId()
        editFolders { it.added(id) }
        return id
    }

    fun deleteFolder(id: String) {
        if (activeId.value == id) selectFolder(null)
        editFolders { it.removed(id) }
    }

    fun saveFolder(f: ru.letotam.ddmail.sync.DialogFolder) = editFolders { d -> d.edited(f.id) { f } }

    fun moveFolder(id: String, toId: String) = editFolders { it.moved(id, toId) }

    /** Правка документа папок; отказ (сеть, правка с другого устройства) — сообщением. */
    private fun editFolders(transform: (FoldersDoc) -> FoldersDoc) {
        viewModelScope.launch {
            runCatching { hub.editFolders(transform) }.onFailure { e ->
                if (e is kotlinx.coroutines.CancellationException) throw e
                notice.value = if (e is ru.letotam.ddmail.sync.FoldersConflict) e.message else "Папки не сохранены: ${e.errorText()}"
            }
        }
    }
}

/** Строк «Диалоги» в выпадашке — больше на телефоне не просмотреть. */
private const val MAX_CONV_HITS = 30

/** Писем в «Сообщениях» — как `limit: 12` у десктопа. */
private const val MAX_MSG_HITS = 12

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

internal fun matchesTarget(c: Conversation, target: Set<String>, own: Set<String> = emptySet()): Boolean {
    if (c.spam) return false
    val cps = c.counterparts.map { it.addr.lowercase() }.filter { it.isNotEmpty() }.toSet()
    // Свои адреса среди адресатов (письмо себе и ещё кому-то) в собеседники не
    // входят — сверяем без них.
    // Ровно адресаты — да; без своих адресов (себе в копию) — тоже. Письмо
    // себе сервер отдаёт беседой, где собеседник — свой же адрес: там
    // вычитать свои нельзя, поэтому проверяются оба варианта.
    if (cps.isNotEmpty()) return cps == target || cps == target - own
    // Беседа без собеседников — письмо себе: сверяем по её id.
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
