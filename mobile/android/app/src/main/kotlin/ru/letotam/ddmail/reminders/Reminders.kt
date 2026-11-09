package ru.letotam.ddmail.reminders

import android.app.AlarmManager
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.json.Json
import ru.letotam.ddmail.DdmailApp
import ru.letotam.ddmail.MainActivity
import ru.letotam.ddmail.R
import ru.letotam.ddmail.api.EventDto
import ru.letotam.ddmail.api.SessionApi
import ru.letotam.ddmail.calendar.ReminderPlanner
import ru.letotam.ddmail.calendar.ReminderPlanner.Mode
import ru.letotam.ddmail.db.ReminderEntity
import java.time.Instant
import java.time.LocalDate
import java.time.ZoneId
import java.time.format.DateTimeFormatter

/**
 * Напоминания о событиях (контракт §13). Логика — [ReminderPlanner], здесь —
 * Android: один точный будильник на ближайший срок, уведомления, ответы на них.
 *
 * Будильник — `setExactAndAllowWhileIdle`: срабатывает и в Doze. Процесс
 * может быть выгружен — будильник живёт в системе и поднимет ресивер. После
 * перезагрузки будильники стираются ОС — ресивер ловит BOOT_COMPLETED и
 * взводит заново.
 */
class Reminders(private val app: DdmailApp) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private val lock = Mutex()
    private val dao get() = app.db.dao()
    private val json = Json { ignoreUnknownKeys = true; coerceInputValues = true }

    /** Забрать события окна посева и пересеять. Ошибку сети — наверх (вызывающий решит). */
    suspend fun refresh(api: SessionApi) {
        if (!app.calendarPrefs.remindersOn.first()) {
            lock.withLock { clearLocked() }
            return
        }
        val (from, to) = window()
        val events = api.calendarEvents(from, to)
        dao.setMeta(K_EVENTS, json.encodeToString(ListSerializer(EventDto.serializer()), events))
        reseed(events)
    }

    /** Пересев из последнего снимка (включили календарь — звонить сразу, без сети). */
    suspend fun reseedFromCache() {
        val cached = dao.meta(K_EVENTS) ?: return
        val events = runCatching { json.decodeFromString(ListSerializer(EventDto.serializer()), cached) }.getOrNull() ?: return
        reseed(events)
    }

    private suspend fun reseed(events: List<EventDto>) = lock.withLock {
        val (from, to) = window()
        val seed = ReminderPlanner.seed(dao.reminders(), events, app.calendarPrefs.hiddenNow(), System.currentTimeMillis(), from, to)
        dao.applyReminders(seed.delete, seed.put)
        val nm = NotificationManagerCompat.from(app)
        seed.delete.forEach { (e, s) -> nm.cancel(TAG, notifId(e, s)) }
        scheduleLocked()
    }

    /** Календарь скрыли — его напоминания снимаются целиком, висящие уведомления тоже. */
    suspend fun calendarHidden(calendarId: Long) = lock.withLock {
        val nm = NotificationManagerCompat.from(app)
        dao.reminders().filter { it.calendarId == calendarId }.forEach { nm.cancel(TAG, notifId(it.eventId, it.occStart)) }
        dao.deleteCalendarReminders(calendarId)
        scheduleLocked()
    }

    /** Напоминания выключили в настройках. */
    suspend fun disable() = lock.withLock { clearLocked() }

    private suspend fun clearLocked() {
        val nm = NotificationManagerCompat.from(app)
        dao.reminders().forEach { nm.cancel(TAG, notifId(it.eventId, it.occStart)) }
        dao.applyReminders(dao.reminders().map { it.eventId to it.occStart }, emptyList())
        cancelAll()
    }

    /** Будильник снять (выход из учётки). Строки уходят вместе с кэшем. */
    fun cancelAll() {
        app.getSystemService(AlarmManager::class.java)?.cancel(alarmIntent())
    }

    suspend fun schedule() = lock.withLock { scheduleLocked() }

    private suspend fun scheduleLocked() {
        val am = app.getSystemService(AlarmManager::class.java) ?: return
        val next = dao.reminders().mapNotNull { ReminderPlanner.fireAt(it) }.minOrNull()
        val pi = alarmIntent()
        if (next == null) {
            am.cancel(pi)
            return
        }
        val at = next.coerceAtLeast(System.currentTimeMillis() + 1_000)
        if (android.os.Build.VERSION.SDK_INT < 31 || am.canScheduleExactAlarms()) {
            am.setExactAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, at, pi)
        } else {
            am.setAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, at, pi)
        }
    }

    /** Будильник сработал: показать всё, чей срок подошёл, и взвести следующий. */
    suspend fun onAlarm() = lock.withLock {
        val now = System.currentTimeMillis()
        val hidden = app.calendarPrefs.hiddenNow()
        val due = dao.reminders().filter { r -> ReminderPlanner.fireAt(r)?.let { it <= now + 5_000 } == true }
        val updated = mutableListOf<ReminderEntity>()
        val drop = mutableListOf<Pair<Long, Long>>()
        for (r in due) {
            // Последняя линия обороны: скрытый календарь не звонит, даже если строка просочилась.
            if (r.calendarId in hidden) {
                drop += r.eventId to r.occStart
                continue
            }
            val (mode, row) = ReminderPlanner.fire(r, now)
            updated += row
            if (mode != null) notify(row, mode, now)
        }
        dao.applyReminders(drop, updated)
        scheduleLocked()
    }

    /** «✕» — уведомление смахнули: по этому вхождению больше не звонить. */
    suspend fun dismiss(eventId: Long, occStart: Long) = lock.withLock {
        dao.reminder(eventId, occStart)?.let { dao.putReminders(listOf(it.copy(state = ReminderPlanner.CANCELLED, snoozeAt = null))) }
        NotificationManagerCompat.from(app).cancel(TAG, notifId(eventId, occStart))
        scheduleLocked()
    }

    /** «Напомнить позже»: каскад заменяется одним сроком. */
    suspend fun snooze(eventId: Long, occStart: Long, at: Long) = lock.withLock {
        dao.reminder(eventId, occStart)?.let { dao.putReminders(listOf(it.copy(snoozeAt = at, state = ReminderPlanner.ARMED))) }
        NotificationManagerCompat.from(app).cancel(TAG, notifId(eventId, occStart))
        scheduleLocked()
    }

    suspend fun row(eventId: Long, occStart: Long): ReminderEntity? = dao.reminder(eventId, occStart)

    private fun notify(r: ReminderEntity, mode: Mode, now: Long) {
        if (!NotificationManagerCompat.from(app).areNotificationsEnabled()) return
        ensureChannel(app)
        val id = notifId(r.eventId, r.occStart)
        val hhmm = Instant.ofEpochMilli(r.occStart).atZone(ZoneId.systemDefault()).format(HHMM)
        val title = when (mode) {
            Mode.Soon -> "Скоро: ${r.summary}"
            Mode.AtStart -> "Наступило: ${r.summary}"
            Mode.Running -> "Событие уже идёт! ${r.summary}"
        }
        val mins = (r.occStart - now) / 60_000
        val text = when {
            mode == Mode.Running -> "Началось в $hhmm"
            mins > 1 -> "Через $mins мин — начало в $hhmm"
            else -> "Начинается — в $hhmm"
        }
        val open = PendingIntent.getActivity(
            app, id,
            Intent(app, MainActivity::class.java)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP)
                .putExtra(MainActivity.EXTRA_EVENT_ID, r.eventId)
                .putExtra(MainActivity.EXTRA_OCC_START, r.occStart),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val dismiss = PendingIntent.getBroadcast(
            app, id,
            Intent(app, ReminderReceiver::class.java).setAction(ReminderReceiver.DISMISS)
                .putExtra(MainActivity.EXTRA_EVENT_ID, r.eventId).putExtra(MainActivity.EXTRA_OCC_START, r.occStart),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val b = NotificationCompat.Builder(app, CHANNEL)
            .setSmallIcon(R.drawable.ic_stat_event)
            .setContentTitle(title)
            .setContentText(text)
            .setCategory(NotificationCompat.CATEGORY_REMINDER)
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setWhen(r.occStart)
            .setShowWhen(true)
            .setContentIntent(open)
            .setDeleteIntent(dismiss)
            .setAutoCancel(mode != Mode.Soon)
        if (mode == Mode.Soon) {
            val later = PendingIntent.getActivity(
                app, id + 1,
                Intent(app, MainActivity::class.java)
                    .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP)
                    .putExtra(MainActivity.EXTRA_SNOOZE_EVENT, r.eventId)
                    .putExtra(MainActivity.EXTRA_OCC_START, r.occStart),
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
            )
            b.addAction(0, "Напомнить позже", later)
        }
        runCatching { NotificationManagerCompat.from(app).notify(TAG, id, b.build()) }
    }

    private fun alarmIntent(): PendingIntent = PendingIntent.getBroadcast(
        app, 0,
        Intent(app, ReminderReceiver::class.java).setAction(ReminderReceiver.FIRE),
        PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
    )

    /** Фоновое действие из ресивера: процесс держится, пока оно не кончится. */
    fun runAsync(pending: BroadcastReceiver.PendingResult, block: suspend () -> Unit) {
        scope.launch {
            try {
                block()
            } finally {
                pending.finish()
            }
        }
    }

    companion object {
        const val CHANNEL = "reminders"
        private const val TAG = "reminder"
        private const val K_EVENTS = "reminder_events"
        private val HHMM = DateTimeFormatter.ofPattern("HH:mm")

        fun notifId(eventId: Long, occStart: Long): Int = (eventId * 31 + occStart / 60_000).hashCode() and 0x7ffffffe

        /** `[сегодняшняя полночь, +8 суток)` — окно посева. */
        fun window(): Pair<Long, Long> {
            val zone = ZoneId.systemDefault()
            val from = LocalDate.now(zone).atStartOfDay(zone)
            return from.toInstant().toEpochMilli() to from.plusDays(ReminderPlanner.WINDOW_DAYS).toInstant().toEpochMilli()
        }

        fun ensureChannel(context: Context) {
            val nm = context.getSystemService(NotificationManager::class.java) ?: return
            if (nm.getNotificationChannel(CHANNEL) != null) return
            nm.createNotificationChannel(
                NotificationChannel(CHANNEL, "Напоминания о событиях", NotificationManager.IMPORTANCE_HIGH).apply {
                    description = "Скоро встреча, встреча началась"
                    enableVibration(true)
                },
            )
        }
    }
}

/** Будильник, смахнутое уведомление, перезагрузка, смена времени. */
class ReminderReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val app = context.applicationContext as DdmailApp
        val r = app.reminders
        val pending = goAsync()
        when (intent.action) {
            FIRE -> r.runAsync(pending) { r.onAlarm() }
            DISMISS -> r.runAsync(pending) {
                r.dismiss(intent.getLongExtra(MainActivity.EXTRA_EVENT_ID, 0), intent.getLongExtra(MainActivity.EXTRA_OCC_START, 0))
            }
            else -> pending.finish()
        }
    }

    companion object {
        const val FIRE = "ru.letotam.ddmail.reminders.FIRE"
        const val DISMISS = "ru.letotam.ddmail.reminders.DISMISS"
    }
}

/** Перезагрузка стёрла будильник; время или пояс сменились — срок другой. */
class RescheduleReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val r = (context.applicationContext as DdmailApp).reminders
        r.runAsync(goAsync()) { r.schedule() }
    }
}
