package ru.letotam.ddmail.background

import android.content.Context
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.NetworkType
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import kotlinx.coroutines.flow.first
import ru.letotam.ddmail.DdmailApp
import ru.letotam.ddmail.sync.isAuthError
import java.util.concurrent.TimeUnit

/**
 * Фоновая синхронизация (контракт §4, §7; mobile-external-accounts §6): у
 * ddmail — страховка от потерянного push, у внешних IMAP-учёток — единственный
 * путь новой почты в фоне (push у них нет). Раз в ~15 минут (меньше Android
 * не даёт; точное время решает ОС): синк всех учёток, уведомления, докачка
 * тел по выбору пользователя. Ошибка одной учётки другим не мешает.
 */
class SyncWorker(context: Context, params: WorkerParameters) : CoroutineWorker(context, params) {
    override suspend fun doWork(): Result {
        val app = applicationContext as DdmailApp
        // Процесс мог подняться ради этой задачи — учётки ещё не загружены.
        app.ensureAccounts()
        // Напоминания — и при открытом приложении: их окно привязано к «сейчас»,
        // а не к тому, что показывает экран (контракт §13).
        app.pim()?.let { p -> runCatching { app.reminders.refresh(p) } }
        // Приложение на экране — почту синхронизирует оно само (WebSocket, IDLE);
        // второй проход рядом гонялся бы с ним за кэш.
        if (app.foreground) return Result.success()
        val hub = app.hub
        if (hub.sources.value.isEmpty()) return Result.success()
        hub.syncAll()
        val open = if (app.foreground) app.openConversation else null
        app.notifier.check(hub.conversations.first(), hub.folders.first(), open)
        if (app.settings.prefetchAllowedNow()) {
            for (s in hub.sources.value) {
                try {
                    s.prefetchBodies(allowed = { app.settings.prefetchAllowedNow() })
                } catch (e: Exception) {
                    if (e is kotlinx.coroutines.CancellationException) throw e
                    hub.noteError(s.accountKey, e)
                    if (!e.isAuthError() && e !is ru.letotam.ddmail.api.ApiException && e !is ru.letotam.ddmail.imap.MailException) throw e
                }
            }
        }
        return Result.success()
    }

    companion object {
        private const val NAME = "mail-sync"

        fun schedule(context: Context) {
            val req = PeriodicWorkRequestBuilder<SyncWorker>(15, TimeUnit.MINUTES)
                .setConstraints(Constraints.Builder().setRequiredNetworkType(NetworkType.CONNECTED).build())
                .build()
            WorkManager.getInstance(context).enqueueUniquePeriodicWork(NAME, ExistingPeriodicWorkPolicy.KEEP, req)
        }

        fun cancel(context: Context) = WorkManager.getInstance(context).cancelUniqueWork(NAME)
    }
}
