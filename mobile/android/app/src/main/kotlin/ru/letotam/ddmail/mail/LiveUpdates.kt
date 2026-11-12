package ru.letotam.ddmail.mail

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import okhttp3.WebSocket
import ru.letotam.ddmail.sync.AccountStatus
import ru.letotam.ddmail.sync.ImapEngine
import ru.letotam.ddmail.sync.MailHub
import ru.letotam.ddmail.sync.SyncEngine
import ru.letotam.ddmail.sync.isAuthError

/**
 * Новая почта, пока приложение на экране:
 *
 * - сервер ddmail — WebSocket (контракт §7): любое событие — синк, слитый в
 *   один на пачку событий; обрыв — переподключение с паузой 2 с … 1 мин,
 *   отказ токена — сначала синк (он обменяет токен);
 * - внешние IMAP-учётки — IDLE на INBOX, своё соединение на каждую
 *   (mobile-external-accounts §6): событие — синк; обрыв — переподключение
 *   с бэкоффом не чаще раза в 30 с; «нужен вход» — IDLE этой учётки стоит,
 *   пока не войдут.
 *
 * В фоне ничего этого нет: там — push (ddmail) и периодический синк.
 */
class LiveUpdates(
    private val hub: MailHub,
    private val scope: CoroutineScope,
    private val sync: () -> Unit,
    /** `calendar_updated`: календарь, задачи и напоминания перечитываются, почта — нет. */
    private val onCalendar: () -> Unit = {},
) {
    private var socket: WebSocket? = null
    private var loop: Job? = null
    private var debounce: Job? = null
    private var watchers: Job? = null
    private val imap = mutableMapOf<String, Job>()
    private var wanted = false

    fun start() {
        if (wanted) return
        wanted = true
        connect(0)
        // Набор учёток сменился — IDLE-соединения вслед за ним.
        watchers = scope.launch {
            hub.sources.collect { list ->
                val engines = list.filterIsInstance<ImapEngine>().associateBy { it.accountKey }
                (imap.keys - engines.keys).forEach { imap.remove(it)?.cancel() }
                engines.forEach { (k, e) -> if (k !in imap) imap[k] = scope.launch { watch(e) } }
            }
        }
    }

    fun stop() {
        wanted = false
        loop?.cancel()
        watchers?.cancel()
        imap.values.forEach { it.cancel() }
        imap.clear()
        socket?.close(1000, null)
        socket = null
    }

    private suspend fun watch(e: ImapEngine) {
        var attempt = 0
        while (wanted) {
            // Учётке нужен вход — ретраи не помогут; ждём, пока статус сменится.
            if (hub.statuses.value[e.accountKey] == AccountStatus.NeedsLogin) {
                delay(30_000)
                continue
            }
            val started = System.currentTimeMillis()
            try {
                e.watch { onEvent() }
            } catch (ex: Exception) {
                if (ex is kotlinx.coroutines.CancellationException) throw ex
                if (ex.isAuthError()) hub.setStatus(e.accountKey, AccountStatus.NeedsLogin)
            }
            // Долго жило — обрыв случайный, начинаем бэкофф заново.
            if (System.currentTimeMillis() - started > 5 * 60_000) attempt = 0
            attempt++
            delay(minOf(5 * 60_000L, 30_000L * attempt))
            // Пока соединения не было, могли прийти письма.
            onEvent()
        }
    }

    private fun connect(attempt: Int) {
        loop?.cancel()
        val engine: SyncEngine = hub.ddmail ?: return
        loop = scope.launch {
            if (attempt > 0) delay(minOf(60_000L, 2_000L shl minOf(attempt - 1, 5)))
            if (!wanted) return@launch
            socket = runCatching {
                engine.openSocket(
                    onEvent = { type -> if (type == "calendar_updated") onCalendar() else onEvent() },
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

    /** События приходят пачками — одна синхронизация на пачку. */
    private fun onEvent() {
        debounce?.cancel()
        debounce = scope.launch {
            delay(400)
            sync()
        }
    }
}
