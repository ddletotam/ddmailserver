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
import kotlinx.coroutines.launch
import ru.letotam.ddmail.DdmailApp
import ru.letotam.ddmail.SessionStatus
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.api.MessageBody
import ru.letotam.ddmail.compose.ComposeController
import ru.letotam.ddmail.conversation.ConversationController
import ru.letotam.ddmail.sync.Conversation
import ru.letotam.ddmail.sync.FoldersDoc
import ru.letotam.ddmail.sync.SyncEngine

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
)

/**
 * Список бесед учётки: кэш Room → видимая часть выбранной папки. Сеть только
 * пополняет кэш ([SyncEngine]); экран рисует из кэша сразу.
 */
class MailViewModel(
    private val app: DdmailApp,
    private val engine: SyncEngine,
    private val onStatus: (SessionStatus) -> Unit,
) : ViewModel() {
    private val activeId = MutableStateFlow<String?>(null)
    val syncing = MutableStateFlow(false)

    /** Открытая беседа — по id, не по позиции: список меняется дельтами. */
    val selected = MutableStateFlow<String?>(null)

    val conversation = ConversationController(app, engine, viewModelScope) { onStatus(SessionStatus.Dead) }

    val compose = ComposeController(app, engine, conversation, viewModelScope, ::sync) { onStatus(SessionStatus.Dead) }

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
        combine(engine.conversations, engine.folders, activeId) { convs, doc, id -> build(convs, doc, id) }
            .stateIn(viewModelScope, SharingStarted.Eagerly, MailState())

    init {
        viewModelScope.launch {
            val p = app.uiPrefs.data.first()
            activeId.value = p[ACTIVE_FOLDER]
            lastConv = p[LAST_CONV]
        }
        viewModelScope.launch { engine.folders.collect { doc = it } }
        viewModelScope.launch {
            engine.conversations.collect { list ->
                all = list
                selected.value?.let { id -> list.firstOrNull { it.id == id }?.let(conversation::updated) }
                // Отправили новое письмо — перейти в его беседу, как только она появилась.
                compose.sentTarget?.let { target ->
                    list.firstOrNull { c -> matchesTarget(c, target) }?.let { c ->
                        compose.consumeTarget()
                        open(c.id)
                    }
                }
            }
        }
        sync()
    }

    fun sync() {
        if (syncing.value) return
        syncing.value = true
        viewModelScope.launch {
            val status = try {
                engine.sync()
                SessionStatus.Ok
            } catch (e: ApiException.Unauthorized) {
                SessionStatus.Dead
            } catch (e: ApiException) {
                SessionStatus.Offline(e.message ?: "")
            } finally {
                syncing.value = false
            }
            onStatus(status)
        }
    }

    /** `null` — «Входящие». Выбор помнится между запусками (контракт §5). */
    fun selectFolder(id: String?) {
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
        val c = id?.let { i -> all.firstOrNull { it.id == i } }
        if (c != null) {
            val f = doc.folderOf(c)
            val fid = f?.let { doc.folders[it].id }
            if (fid != activeId.value) selectFolder(fid)
        }
        selected.value = id
        compose.reset()
        conversation.open(c)
        id?.let { v -> viewModelScope.launch { app.uiPrefs.edit { it[LAST_CONV] = v } } }
    }

    /** Раскладка, где беседа видна сразу, — открыть ту, на которой закончили (§5). */
    fun restoreLast() {
        val id = lastConv ?: return
        if (selected.value != null) return
        if (all.any { it.id == id }) {
            lastConv = null
            open(id)
        }
    }

    private fun build(convs: List<Conversation>, doc: FoldersDoc, id: String?): MailState {
        // Удалённая папка — снова инбокс. Скрытая видна табом, только пока
        // в неё привели (открытие беседы из неё).
        val active = doc.folders.indexOfFirst { it.id == id }.takeIf { it >= 0 }
        val (counts, inbox) = doc.unreadCounts(convs)
        val tabs = doc.folders.mapIndexedNotNull { i, f ->
            if (f.hidden && i != active) null else FolderTab(i, f.id, f.name, f.icon, counts[i])
        }
        return MailState(tabs, active, inbox, doc.filter(convs, active), loaded = true)
    }
}

/**
 * Беседа нового письма: набор собеседников совпал с адресатами. Письмо себе
 * сервер отдаёт беседой без собеседников — тогда сверяем по её id.
 */
internal fun matchesTarget(c: Conversation, target: Set<String>): Boolean {
    val cps = c.counterparts.map { it.addr.lowercase() }.filter { it.isNotEmpty() }.toSet()
    if (cps.isNotEmpty()) return cps == target
    val id = c.id.lowercase()
    return target.all { id.contains(it) }
}
