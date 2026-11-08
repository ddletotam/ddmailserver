package ru.letotam.ddmail.background

import android.Manifest
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat
import kotlinx.coroutines.flow.first
import ru.letotam.ddmail.MainActivity
import ru.letotam.ddmail.R
import ru.letotam.ddmail.settings.AppSettings
import ru.letotam.ddmail.sync.Conversation
import ru.letotam.ddmail.sync.FoldersDoc

/**
 * Уведомления о новой почте (контракт §5, §7). Текст собирается на телефоне
 * из своего кэша — ни отправитель, ни тема через push не идут.
 *
 * «Новое» — непрочитанные письма с uid больше последнего, о котором уже
 * уведомили; первый проход лишь запоминает рубеж (иначе после входа
 * посыпались бы уведомления обо всём непрочитанном). Не уведомляем о беседе,
 * которая открыта на экране, и о скрытых папках (как десктоп).
 */
class MailNotifier(private val context: Context, private val settings: AppSettings) {

    suspend fun check(convs: List<Conversation>, folders: FoldersDoc, openId: String?) {
        badge(convs.count { c -> c.unreadCount > 0 && folders.folderOf(c)?.let { folders.folders[it].hidden } != true })
        val unseen = convs.flatMap { c -> c.messages.filter { !it.seen }.map { c to it.uid } }
        val top = convs.flatMap { c -> c.messages.map { it.uid } }.maxOrNull() ?: return
        val last = settings.notifiedMax()
        settings.setNotifiedMax(maxOf(top, last ?: 0))
        if (last == null) return
        if (!settingsOn()) return
        val fresh = unseen.filter { it.second > last }
            .groupBy({ it.first }, { it.second })
            .filterKeys { c ->
                c.id != openId && folders.folderOf(c)?.let { folders.folders[it].hidden } != true
            }
        fresh.forEach { (c, uids) -> post(c, uids.size) }
    }

    /**
     * Бейдж на иконке — беседы с непрочитанным (как точка трея десктопа:
     * беседы, а не письма; скрытые папки не считаются). Лаунчер Huawei —
     * своим API; на других лаунчерах вызов просто не найдёт провайдера.
     */
    private fun badge(n: Int) {
        try {
            val extra = android.os.Bundle().apply {
                putString("package", context.packageName)
                putString("class", MainActivity::class.java.name)
                putInt("badgenumber", n)
            }
            context.contentResolver.call(
                android.net.Uri.parse("content://com.huawei.android.launcher.settings/badge/"),
                "change_badge", null, extra,
            )
        } catch (_: Exception) {
        }
    }

    private suspend fun settingsOn(): Boolean {
        val enabled = settings.notifications.first()
        if (!enabled) return false
        return Build.VERSION.SDK_INT < 33 ||
            ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED
    }

    private fun post(c: Conversation, n: Int) {
        ensureChannel(context)
        val open = Intent(context, MainActivity::class.java)
            .addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP)
            .putExtra(MainActivity.EXTRA_OPEN, c.id)
        val pi = PendingIntent.getActivity(
            context, c.id.hashCode(), open,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val title = c.label.ifBlank { c.counterparts.firstOrNull()?.addr.orEmpty() }
        val text = if (n > 1) "${c.lastSubject} · новых: $n" else c.lastSubject
        val notification = NotificationCompat.Builder(context, CHANNEL)
            .setSmallIcon(R.drawable.ic_stat_mail)
            .setContentTitle(title)
            .setContentText(text)
            .setStyle(NotificationCompat.BigTextStyle().bigText(text))
            .setCategory(NotificationCompat.CATEGORY_EMAIL)
            .setAutoCancel(true)
            .setContentIntent(pi)
            .setNumber(n)
            .build()
        try {
            NotificationManagerCompat.from(context).notify(c.id.hashCode(), notification)
        } catch (_: SecurityException) {
        }
    }

    companion object {
        const val CHANNEL = "mail"

        fun ensureChannel(context: Context) {
            val nm = context.getSystemService(NotificationManager::class.java) ?: return
            if (nm.getNotificationChannel(CHANNEL) != null) return
            nm.createNotificationChannel(
                NotificationChannel(CHANNEL, "Новые письма", NotificationManager.IMPORTANCE_DEFAULT),
            )
        }

        /** Беседу открыли — её уведомление больше не нужно. */
        fun cancel(context: Context, conversationId: String) {
            NotificationManagerCompat.from(context).cancel(conversationId.hashCode())
        }
    }
}
