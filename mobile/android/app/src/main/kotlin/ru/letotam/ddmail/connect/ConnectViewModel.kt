package ru.letotam.ddmail.connect

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ru.letotam.ddmail.DdmailApp
import ru.letotam.ddmail.R
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.api.emailDomain
import ru.letotam.ddmail.api.normalizeServerUrl
import ru.letotam.ddmail.auth.DiscoveryProgress
import ru.letotam.ddmail.auth.Session
import ru.letotam.ddmail.ui.Msg

/** Шаги подключения (контракт §3). */
enum class ConnectStep { Email, Searching, Found, Manual }

data class ConnectState(
    val step: ConnectStep = ConnectStep.Email,
    val email: String = "",
    val server: String = "",
    val username: String = "",
    val password: String = "",
    val progress: DiscoveryProgress? = null,
    /** Нейтральное сообщение (не ошибка): «сервер не найден, введите вручную». */
    val notice: Msg? = null,
    val error: Msg? = null,
    val busy: Boolean = false,
)

/**
 * Подключение: адрес почты → поиск сервера ddmail → вход. Ничего не
 * сохраняется, пока вход не прошёл; пароль не сохраняется вовсе.
 */
class ConnectViewModel(
    private val app: DdmailApp,
    initial: ConnectState = ConnectState(),
    private val onConnected: (Session) -> Unit,
) : ViewModel() {
    private val _state = MutableStateFlow(initial)
    val state: StateFlow<ConnectState> = _state

    private var search: Job? = null

    fun setEmail(v: String) = _state.update { it.copy(email = v, error = null) }
    fun setServer(v: String) = _state.update { it.copy(server = v, error = null) }
    fun setUsername(v: String) = _state.update { it.copy(username = v, error = null) }
    fun setPassword(v: String) = _state.update { it.copy(password = v, error = null) }

    fun findServer() {
        val email = _state.value.email.trim()
        val domain = emailDomain(email)
        if (domain == null) {
            _state.update { it.copy(error = Msg(R.string.err_email)) }
            return
        }
        _state.update {
            it.copy(step = ConnectStep.Searching, email = email, progress = null, error = null, notice = null)
        }
        search?.cancel()
        search = viewModelScope.launch {
            val url = app.discovery.find(email) { p -> _state.update { it.copy(progress = p) } }
            val local = email.substringBeforeLast('@')
            _state.update {
                if (url != null) {
                    it.copy(
                        step = ConnectStep.Found,
                        server = url,
                        // Сервер входит по имени пользователя; чаще всего это часть до @.
                        username = it.username.ifEmpty { local },
                    )
                } else {
                    it.copy(
                        step = ConnectStep.Manual,
                        username = it.username.ifEmpty { local },
                        notice = Msg(R.string.not_found, listOf(domain)),
                    )
                }
            }
        }
    }

    fun manual() = _state.update { it.copy(step = ConnectStep.Manual, error = null, notice = null) }

    /** «Назад»: из поиска — с отменой поиска; с остальных шагов — к адресу почты. */
    fun back(): Boolean {
        val s = _state.value
        if (s.busy || s.step == ConnectStep.Email) return false
        search?.cancel()
        _state.update { it.copy(step = ConnectStep.Email, error = null, notice = null, progress = null) }
        return true
    }

    fun login() {
        val s = _state.value
        if (s.busy) return
        val username = s.username.trim()
        val server = normalizeServerUrl(s.server)
        if (username.isEmpty() || s.password.isEmpty() || server.isFailure) {
            _state.update {
                it.copy(
                    error = server.exceptionOrNull()?.message
                        ?.takeIf { s.server.isNotBlank() }
                        ?.let { m -> Msg(R.string.err_plain, listOf(m)) }
                        ?: Msg(R.string.err_fill),
                )
            }
            return
        }
        val url = server.getOrThrow()
        _state.update { it.copy(busy = true, error = null, server = url) }
        viewModelScope.launch {
            try {
                val r = app.api(url).login(username, s.password)
                val session = Session(url, r.username, r.userId, r.token)
                app.sessions.save(session)
                _state.update { it.copy(busy = false, password = "") }
                onConnected(session)
            } catch (e: ApiException) {
                _state.update { it.copy(busy = false, error = e.toMsg()) }
            }
        }
    }
}

internal fun ApiException.toMsg(): Msg = when (this) {
    is ApiException.Unauthorized -> Msg(R.string.err_credentials)
    is ApiException.Network -> Msg(R.string.err_network, listOf(message ?: ""))
    is ApiException.Server -> Msg(R.string.err_server, listOf(code))
}
