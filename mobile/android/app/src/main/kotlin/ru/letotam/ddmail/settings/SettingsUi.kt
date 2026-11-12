package ru.letotam.ddmail.settings

import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.*
import androidx.compose.material.icons.filled.*
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.systemBarsPadding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.setValue
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import kotlinx.coroutines.launch
import ru.letotam.ddmail.BuildConfig
import ru.letotam.ddmail.mail.AccountActions
import ru.letotam.ddmail.sync.AccountStatus
import ru.letotam.ddmail.sync.ImapEngine
import ru.letotam.ddmail.sync.MailSource

private val labels = listOf(
    Prefetch.Always to ("Всегда" to "Беседы открываются мгновенно, в том числе без сети."),
    Prefetch.WifiOnly to ("Только по Wi-Fi" to "На мобильной сети письма качаются при открытии беседы."),
    Prefetch.Never to ("Нет, при открытии" to "Экономит трафик и место; открытие беседы ждёт сеть."),
)

@Composable
private fun PrefetchChoice(current: Prefetch?, onPick: (Prefetch) -> Unit) {
    Column {
        labels.forEach { (v, t) ->
            Row(
                Modifier.fillMaxWidth().clickable { onPick(v) }.padding(vertical = 6.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                RadioButton(selected = current == v, onClick = { onPick(v) })
                Column {
                    Text(t.first, style = MaterialTheme.typography.bodyLarge)
                    Text(t.second, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
        }
    }
}

/**
 * После первого входа (контракт §4): трафик чужой — ответ по умолчанию не
 * выбран, окно не закрывается без выбора.
 */
@Composable
fun PrefetchQuestion(settings: AppSettings) {
    val current by settings.prefetch.collectAsState(initial = Prefetch.Always)
    if (current != null) return
    val scope = rememberCoroutineScope()
    AlertDialog(
        onDismissRequest = {},
        title = { Text("Загружать письма заранее?") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("Тогда новые письма окажутся на телефоне до того, как вы откроете беседу. Изменить можно в настройках.")
                PrefetchChoice(null) { v -> scope.launch { settings.setPrefetch(v) } }
            }
        },
        confirmButton = {},
    )
}

@Composable
fun SettingsDialog(
    settings: AppSettings,
    sources: List<MailSource>,
    statuses: Map<String, AccountStatus>,
    accounts: AccountActions,
    onClose: () -> Unit,
) {
    var removeAsk by androidx.compose.runtime.remember { androidx.compose.runtime.mutableStateOf<MailSource?>(null) }
    removeAsk?.let { src ->
        AlertDialog(
            onDismissRequest = { removeAsk = null },
            title = { Text("Удалить учётку?") },
            text = { Text("${src.title}: пароль и кэш писем этой учётки будут удалены с телефона. На сервере ничего не изменится.") },
            confirmButton = {
                TextButton(onClick = {
                    removeAsk = null
                    accounts.remove(src.accountKey)
                }) { Text("Удалить", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = { IconButton(onClick = { removeAsk = null }) { Icon(Icons.Filled.Close, "Отмена") } },
        )
    }
    val prefetch by settings.prefetch.collectAsState(initial = null)
    val notify by settings.notifications.collectAsState(initial = true)
    val scope = rememberCoroutineScope()
    Dialog(onDismissRequest = onClose, properties = DialogProperties(usePlatformDefaultWidth = false, decorFitsSystemWindows = false)) {
        Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) {
            Column(Modifier.fillMaxSize().systemBarsPadding()) {
                Row(Modifier.fillMaxWidth().padding(8.dp), verticalAlignment = Alignment.CenterVertically) {
                    IconButton(onClick = onClose) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Назад") }
                    Text("Настройки", style = MaterialTheme.typography.titleLarge)
                }
                HorizontalDivider()
                Column(
                    Modifier.fillMaxWidth().widthIn(max = 640.dp).verticalScroll(rememberScrollState()).padding(16.dp),
                    verticalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    Section("Учётки")
                    sources.forEach { src ->
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Column(Modifier.weight(1f)) {
                                Text(src.title, style = MaterialTheme.typography.bodyLarge)
                                val kind = if (src is ImapEngine) "IMAP · ${src.account.imapHost}" else "Сервер ddmail"
                                val st = when (val x = statuses[src.accountKey]) {
                                    AccountStatus.NeedsLogin -> " · нужен вход"
                                    is AccountStatus.Offline -> " · нет связи"
                                    else -> ""
                                }
                                Text(kind + st, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                            }
                            IconButton(onClick = { accounts.relogin(src.accountKey) }) { Icon(Icons.AutoMirrored.Filled.Login, "Войти заново") }
                            if (src is ImapEngine) IconButton(onClick = { accounts.edit(src.accountKey) }) { Icon(Icons.Filled.Edit, "Изменить") }
                            IconButton(onClick = { removeAsk = src }) { Icon(Icons.Filled.Delete, "Удалить", tint = MaterialTheme.colorScheme.error) }
                        }
                    }
                    OutlinedButton(onClick = accounts.add) {
                        Icon(Icons.Filled.PersonAdd, null)
                        Spacer(Modifier.width(8.dp))
                        Text("Добавить учётку")
                    }
                    HorizontalDivider()
                    Section("Загрузка писем заранее")
                    PrefetchChoice(prefetch) { v -> scope.launch { settings.setPrefetch(v) } }
                    HorizontalDivider()
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Column(Modifier.weight(1f)) {
                            Text("Уведомления о новых письмах", style = MaterialTheme.typography.bodyLarge)
                            Text(
                                "Без push-сервиса: пока приложение закрыто, почта проверяется примерно раз в 15 минут.",
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                            )
                        }
                        Switch(checked = notify, onCheckedChange = { v -> scope.launch { settings.setNotifications(v) } })
                    }
                    HorizontalDivider()
                    Section("Календарь")
                    val app = androidx.compose.ui.platform.LocalContext.current.applicationContext as ru.letotam.ddmail.DdmailApp
                    val remind by app.calendarPrefs.remindersOn.collectAsState(initial = true)
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Column(Modifier.weight(1f)) {
                            Text("Напоминания о событиях", style = MaterialTheme.typography.bodyLarge)
                            Text(
                                "По будильникам событий (VALARM); смахнуть уведомление — больше не напоминать об этой встрече.",
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                            )
                        }
                        Switch(checked = remind, onCheckedChange = { v ->
                            scope.launch {
                                app.calendarPrefs.setRemindersOn(v)
                                val pim = app.pim()
                                if (!v) app.reminders.disable() else if (pim != null) runCatching { app.reminders.refresh(pim) }
                            }
                        })
                    }
                    ru.letotam.ddmail.reminders.BackgroundWorkSteps()
                    HorizontalDivider()
                    Section("О приложении")
                    Text("ddmail для Android, версия ${BuildConfig.VERSION_NAME}")
                }
            }
        }
    }
}

@Composable
private fun Section(title: String) {
    Text(title, style = MaterialTheme.typography.titleSmall, color = MaterialTheme.colorScheme.primary)
}
