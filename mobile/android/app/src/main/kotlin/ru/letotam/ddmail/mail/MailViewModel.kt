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
import ru.letotam.ddmail.conversation.ConversationController
import ru.letotam.ddmail.sync.Conversation
import ru.letotam.ddmail.sync.FoldersDoc
import ru.letotam.ddmail.sync.SyncEngine

private val Context.uiPrefs by preferencesDataStore(name = "ui")
private val ACTIVE_FOLDER = stringPreferencesKey("active_folder")

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

    /** Полный список (не только видимая папка): открытая беседа ищется в нём. */
    private var all: List<Conversation> = emptyList()

    val state: StateFlow<MailState> =
        combine(engine.conversations, engine.folders, activeId) { convs, doc, id -> build(convs, doc, id) }
            .stateIn(viewModelScope, SharingStarted.Eagerly, MailState())

    init {
        viewModelScope.launch {
            activeId.value = app.uiPrefs.data.map { it[ACTIVE_FOLDER] }.first()
        }
        viewModelScope.launch {
            engine.conversations.collect { list ->
                all = list
                selected.value?.let { id -> list.firstOrNull { it.id == id }?.let(conversation::updated) }
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

    fun open(id: String?) {
        if (id == selected.value) return
        selected.value = id
        conversation.open(id?.let { i -> all.firstOrNull { it.id == i } })
    }

    private fun build(convs: List<Conversation>, doc: FoldersDoc, id: String?): MailState {
        // Удалённая папка (или скрытая, на которую указывал старый выбор) — снова инбокс.
        val active = doc.folders.indexOfFirst { it.id == id && !it.hidden }.takeIf { it >= 0 }
        val (counts, inbox) = doc.unreadCounts(convs)
        val tabs = doc.folders.mapIndexedNotNull { i, f ->
            if (f.hidden) null else FolderTab(i, f.id, f.name, f.icon, counts[i])
        }
        return MailState(tabs, active, inbox, doc.filter(convs, active), loaded = true)
    }
}
