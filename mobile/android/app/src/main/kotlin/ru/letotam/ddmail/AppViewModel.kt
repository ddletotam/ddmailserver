package ru.letotam.ddmail

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import ru.letotam.ddmail.account.ImapAccount
import ru.letotam.ddmail.auth.Session
import ru.letotam.ddmail.connect.ConnectState
import ru.letotam.ddmail.connect.ConnectStep
import ru.letotam.ddmail.sync.AccountStatus

sealed interface AppState {
    data object Loading : AppState

    /**
     * Окно подключения: первый запуск и «Добавить учётку» — одно и то же
     * (mobile-external-accounts §1). [cancellable] — учётки уже есть, можно
     * вернуться к почте.
     */
    data class Connect(val prefill: ConnectState, val generation: Int, val cancellable: Boolean) : AppState

    /** Есть хотя бы одна учётка. [session] — учётка ddmail, если она есть. */
    data class Ready(val session: Session?, val imap: List<ImapAccount>) : AppState
}

class AppViewModel(private val app: DdmailApp) : ViewModel() {
    private val _state = MutableStateFlow<AppState>(AppState.Loading)
    val state: StateFlow<AppState> = _state
    private var generation = 0

    init {
        viewModelScope.launch { reload(connectIfEmpty = true) }
    }

    private suspend fun reload(connectIfEmpty: Boolean) {
        val s = app.sessions.load()
        val imap = app.accounts.list()
        app.applyAccounts(s, imap)
        if (s == null && imap.isEmpty()) {
            if (connectIfEmpty) toConnect(ConnectState(), cancellable = false)
        } else {
            _state.value = AppState.Ready(s, imap)
        }
    }

    /** Вошли в ddmail. */
    fun connected(session: Session) {
        // Новый вход — новый токен: API прошлой сессии держал бы старый.
        app.forgetSessionApi()
        viewModelScope.launch {
            reload(connectIfEmpty = false)
            app.hub.setStatus(session.accountKey, AccountStatus.Ok)
        }
    }

    /** Добавили или поправили внешнюю учётку (она уже сохранена). */
    fun imapSaved(key: String) {
        viewModelScope.launch {
            reload(connectIfEmpty = false)
            app.hub.setStatus(key, AccountStatus.Ok)
        }
    }

    fun addAccount() = toConnect(ConnectState(), cancellable = true)

    /** Окно подключения закрыли без новой учётки. */
    fun cancelConnect() {
        viewModelScope.launch { reload(connectIfEmpty = true) }
    }

    /** Повторный вход в ddmail: тот же сервер и логин, осталось ввести пароль. */
    fun relogin(session: Session) = toConnect(
        ConnectState(step = ConnectStep.Found, server = session.serverUrl, username = session.username, email = session.username),
        cancellable = true,
    )

    /**
     * Удалить учётку: её секреты и кэш уходят с телефона (контракт §1).
     * Последняя — на телефоне не остаётся ничего, снова окно подключения.
     */
    fun removeAccount(key: String) {
        viewModelScope.launch {
            val ddmail = app.ddmailSession()
            app.hub.source(key)?.forget()
            if (ddmail?.accountKey == key) {
                app.sessions.clear()
                app.forgetSessionApi()
                app.reminders.cancelAll()
                app.calendarPrefs.clear()
            } else {
                app.accounts.remove(key)
            }
            app.settings.clearAccountState(key)
            val left = app.sessions.load() != null || app.accounts.list().isNotEmpty()
            if (!left) {
                app.db.dao().clearAll()
                ru.letotam.ddmail.background.SyncWorker.cancel(app)
                androidx.core.app.NotificationManagerCompat.from(app).cancelAll()
            }
            reload(connectIfEmpty = true)
        }
    }

    /** Правка внешней учётки — то же окно, что добавление, с заполненными полями. */
    fun editImap(key: String) {
        viewModelScope.launch {
            val a = app.accounts.list().firstOrNull { it.key == key } ?: return@launch
            toConnect(
                ConnectState(
                    step = ConnectStep.Imap,
                    showServers = true,
                    editingKey = key,
                    email = a.email,
                    name = a.name,
                    imapHost = a.imapHost,
                    imapPort = a.imapPort.toString(),
                    smtpHost = a.smtpHost,
                    smtpPort = a.smtpPort.toString(),
                    smtpAuth = a.smtpAuth,
                    imapLogin = a.login,
                    extraAddresses = a.extraAddresses.joinToString(", "),
                    caldavUrl = a.caldavUrl,
                    carddavUrl = a.carddavUrl,
                ),
                cancellable = true,
            )
        }
    }

    /** «Войти» у учётки: ddmail — вход на сервер, IMAP — форма учётки с паролем. */
    fun reloginAccount(key: String) {
        val s = app.ddmailSession()
        if (s != null && s.accountKey == key) relogin(s) else editImap(key)
    }

    private fun toConnect(prefill: ConnectState, cancellable: Boolean) {
        generation++
        _state.value = AppState.Connect(prefill, generation, cancellable)
    }
}
