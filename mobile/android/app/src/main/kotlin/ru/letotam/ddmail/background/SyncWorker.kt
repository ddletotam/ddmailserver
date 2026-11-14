package ru.letotam.ddmail.background

import android.content.Context
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.ExistingWorkPolicy
import androidx.work.ForegroundInfo
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.OutOfQuotaPolicy
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import kotlinx.coroutines.flow.first
import ru.letotam.ddmail.DdmailApp
import ru.letotam.ddmail.sync.isAuthError
import java.util.concurrent.TimeUnit

/**
 * Фоновая синхронизация (контракт §4, §7; mobile-external-accounts §6): у
 * ddmail — по push и страховкой от потерянного push, у внешних IMAP-учёток —
 * единственный путь новой почты в фоне (push у них нет). Периодически раз в
 * ~15 минут (меньше Android не даёт; точное время решает ОС) и разово —
 * сразу по push ([runNow]): синк всех учёток, уведомления, докачка тел по
 * выбору пользователя. Ошибка одной учётки другим не мешает.
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

    /**
     * Срочная задача по push на Android 10–11 идёт foreground-сервисом —
     * ему нужно своё уведомление (тихое, на время синка). С Android 12
     * срочная задача обходится без него.
     */
    override suspend fun getForegroundInfo(): ForegroundInfo {
        val ctx = applicationContext
        MailNotifier.ensureSyncChannel(ctx)
        val n = androidx.core.app.NotificationCompat.Builder(ctx, MailNotifier.SYNC_CHANNEL)
            .setSmallIcon(ru.letotam.ddmail.R.drawable.ic_stat_mail)
            .setContentTitle("Проверка почты")
            .setPriority(androidx.core.app.NotificationCompat.PRIORITY_MIN)
            .setSilent(true)
            .setOngoing(true)
            .build()
        return ForegroundInfo(SYNC_NOTIFICATION_ID, n)
    }

    companion object {
        private const val NAME = "mail-sync"
        private const val PUSH_NAME = "mail-sync-push"
        private const val SYNC_NOTIFICATION_ID = 0x5359_4e43

        /**
         * Синк сразу — пришёл push (контракт §7). Срочная задача: система
         * запускает её без ожидания окна; квота срочных кончилась — обычная.
         * Push во время идущего синка ставит ещё один проход следом, а не
         * теряется: письмо могло прийти после того, как синк его миновал.
         */
        fun runNow(context: Context) {
            val req = OneTimeWorkRequestBuilder<SyncWorker>()
                .setExpedited(OutOfQuotaPolicy.RUN_AS_NON_EXPEDITED_WORK_REQUEST)
                .setConstraints(Constraints.Builder().setRequiredNetworkType(NetworkType.CONNECTED).build())
                .build()
            WorkManager.getInstance(context).enqueueUniqueWork(PUSH_NAME, ExistingWorkPolicy.APPEND_OR_REPLACE, req)
        }

        fun schedule(context: Context) {
            val req = PeriodicWorkRequestBuilder<SyncWorker>(15, TimeUnit.MINUTES)
                .setConstraints(Constraints.Builder().setRequiredNetworkType(NetworkType.CONNECTED).build())
                .build()
            WorkManager.getInstance(context).enqueueUniquePeriodicWork(NAME, ExistingPeriodicWorkPolicy.KEEP, req)
        }

        fun cancel(context: Context) {
            WorkManager.getInstance(context).cancelUniqueWork(NAME)
            WorkManager.getInstance(context).cancelUniqueWork(PUSH_NAME)
        }
    }
}
