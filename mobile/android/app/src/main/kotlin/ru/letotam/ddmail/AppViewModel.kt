package ru.letotam.ddmail

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.auth.Session
import ru.letotam.ddmail.connect.ConnectState
import ru.letotam.ddmail.connect.ConnectStep

/** Состояние сессии (контракт §2): «нужен вход» отдельно от «нет связи». */
sealed interface SessionStatus {
    data object Checking : SessionStatus
    data object Ok : SessionStatus
    data class Offline(val reason: String) : SessionStatus
    data object Dead : SessionStatus
}

sealed interface AppState {
    data object Loading : AppState

    /** Подключения нет (или идёт повторный вход — тогда форма заполнена). */
    data class Connect(val prefill: ConnectState, val generation: Int) : AppState

    data class Connected(val session: Session, val status: SessionStatus) : AppState
}

class AppViewModel(private val app: DdmailApp) : ViewModel() {
    private val _state = MutableStateFlow<AppState>(AppState.Loading)
    val state: StateFlow<AppState> = _state
    private var generation = 0

    init {
        viewModelScope.launch {
            val s = app.sessions.load()
            if (s == null) toConnect(ConnectState()) else connected(s)
        }
    }

    fun connected(session: Session) {
        _state.value = AppState.Connected(session, SessionStatus.Checking)
        refresh(session)
    }

    /**
     * Обменять токен на свежий: и проверка сессии, и продление (сервер
     * обменивает токен не старше 30 суток). 401/403 — сессия мертва, ретраи
     * не помогут; остальное — временное.
     */
    fun refresh(session: Session) {
        viewModelScope.launch {
            val status = try {
                val r = app.api(session.serverUrl).refresh(session.token)
                app.sessions.updateToken(r.token)
                SessionStatus.Ok
            } catch (e: ApiException.Unauthorized) {
                SessionStatus.Dead
            } catch (e: ApiException) {
                SessionStatus.Offline(e.message ?: "")
            }
            val cur = _state.value
            if (cur is AppState.Connected && cur.session.serverUrl == session.serverUrl) {
                _state.value = cur.copy(status = status)
            }
        }
    }

    /** Повторный вход: тот же сервер и логин, осталось ввести пароль. */
    fun relogin(session: Session) = toConnect(
        ConnectState(step = ConnectStep.Found, server = session.serverUrl, username = session.username),
    )

    fun logout() {
        viewModelScope.launch {
            app.sessions.clear()
            toConnect(ConnectState())
        }
    }

    private fun toConnect(prefill: ConnectState) {
        generation++
        _state.value = AppState.Connect(prefill, generation)
    }
}
