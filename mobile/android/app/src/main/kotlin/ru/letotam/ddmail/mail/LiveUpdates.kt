package ru.letotam.ddmail.mail

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import okhttp3.WebSocket
import ru.letotam.ddmail.sync.SyncEngine

/**
 * WebSocket, пока приложение на экране (контракт §7): любое событие сервера
 * (`new_message`, `flags_changed`, `expunge`, `dialog_folders_changed`, …)
 * — синхронизация, слитая в одну на пачку событий. В фоне канала нет:
 * там — push и периодическая синхронизация.
 *
 * Обрыв — переподключение с растущей паузой (2 с … 1 мин). Сервер отверг
 * токен — сначала синхронизация (она обменяет токен), потом снова.
 */
class LiveUpdates(
    private val engine: SyncEngine,
    private val scope: CoroutineScope,
    private val sync: () -> Unit,
) {
    private var socket: WebSocket? = null
    private var loop: Job? = null
    private var debounce: Job? = null
    private var wanted = false

    fun start() {
        if (wanted) return
        wanted = true
        connect(0)
    }

    fun stop() {
        wanted = false
        loop?.cancel()
        socket?.close(1000, null)
        socket = null
    }

    private fun connect(attempt: Int) {
        loop?.cancel()
        loop = scope.launch {
            if (attempt > 0) delay(minOf(60_000L, 2_000L shl minOf(attempt - 1, 5)))
            if (!wanted) return@launch
            socket = runCatching {
                engine.openSocket(
                    onEvent = { onEvent() },
                    onClosed = { authFailed ->
                        scope.launch {
                            if (!wanted) return@launch
                            if (authFailed) sync()
                            connect(attempt + 1)
                        }
                    },
                )
            }.getOrElse {
                connect(attempt + 1)
                null
            }
        }
    }

    /** События приходят пачками (по письму на событие) — одна синхронизация на пачку. */
    private fun onEvent() {
        debounce?.cancel()
        debounce = scope.launch {
            delay(400)
            sync()
        }
    }
}
