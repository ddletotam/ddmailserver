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
import ru.letotam.ddmail.api.ApiException
import java.util.concurrent.TimeUnit

/**
 * Фоновая синхронизация (контракт §4, §7): страховка от потерянного push и,
 * пока push нет, — единственный путь новой почты в фоне. Раз в ~15 минут
 * (меньше Android не даёт; точное время решает ОС): синк, уведомления,
 * докачка тел по выбору пользователя.
 */
class SyncWorker(context: Context, params: WorkerParameters) : CoroutineWorker(context, params) {
    override suspend fun doWork(): Result {
        val app = applicationContext as DdmailApp
        val session = app.sessions.load() ?: return Result.success()
        // Напоминания — и при открытом приложении: их окно привязано к «сейчас»,
        // а не к тому, что показывает экран (контракт §13).
        runCatching { app.reminders.refresh(app.sessionApi(session)) }
        // Приложение на экране — почту синхронизирует оно само (и WebSocket);
        // второй движок рядом гонялся бы с ним за кэш.
        if (app.foreground) return Result.success()
        val engine = app.engine(session)
        return try {
            engine.sync()
            val open = if (app.foreground) app.openConversation else null
            app.notifier.check(engine.conversations.first(), engine.folders.first(), open)
            if (app.settings.prefetchAllowedNow()) {
                engine.prefetchBodies(allowed = { app.settings.prefetchAllowedNow() })
            }
            Result.success()
        } catch (e: ApiException.Unauthorized) {
            // Сессия мертва — ретраи не помогут; войдёт пользователь — продолжим.
            Result.success()
        } catch (e: ApiException) {
            Result.retry()
        }
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
