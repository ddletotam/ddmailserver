package ru.letotam.ddmail

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.ui.unit.dp
import androidx.compose.foundation.layout.isImeVisible
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.CalendarMonth
import androidx.compose.material.icons.filled.ChecklistRtl
import androidx.compose.material.icons.filled.Contacts
import androidx.compose.material.icons.filled.Mail
import androidx.compose.material3.Icon
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.NavigationRail
import androidx.compose.material3.NavigationRailItem
import androidx.compose.material3.VerticalDivider
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.ui.platform.LocalContext
import androidx.lifecycle.compose.LifecycleResumeEffect
import androidx.lifecycle.viewmodel.compose.viewModel
import kotlinx.coroutines.launch
import ru.letotam.ddmail.auth.Session
import ru.letotam.ddmail.calendar.CalendarScreen
import ru.letotam.ddmail.calendar.CalendarViewModel
import ru.letotam.ddmail.calendar.SnoozeDialog
import ru.letotam.ddmail.contacts.ContactsScreen
import ru.letotam.ddmail.contacts.ContactsViewModel
import ru.letotam.ddmail.conversation.ConversationWeb
import ru.letotam.ddmail.mail.MailScreen
import ru.letotam.ddmail.mail.MailViewModel
import ru.letotam.ddmail.mail.AccountActions
import ru.letotam.ddmail.reminders.BackgroundWork
import ru.letotam.ddmail.reminders.BackgroundWorkDialog
import ru.letotam.ddmail.tasks.TasksScreen
import ru.letotam.ddmail.tasks.TasksViewModel

enum class Section(val icon: ImageVector, val label: String) {
    Mail(Icons.Filled.Mail, "Почта"),
    Calendar(Icons.Filled.CalendarMonth, "Календарь"),
    Tasks(Icons.Filled.ChecklistRtl, "Задачи"),
    Contacts(Icons.Filled.Contacts, "Контакты"),
}

/**
 * Разделы приложения (контракт §16): почта, календарь, задачи, контакты.
 * Сложен — панель иконок снизу, как таб-бар iOS; она уходит, когда открыта
 * беседа (у беседы своя шапка) или клавиатура. Разложен — колонка иконок
 * слева, как в iPadOS.
 */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun AppShell(
    app: DdmailApp,
    session: Session?,
    mailVm: MailViewModel,
    accounts: AccountActions,
    modifier: Modifier = Modifier,
) {
    val key = session?.let { "${it.serverUrl}-${it.token.hashCode()}" } ?: "none"
    val api = remember(key) { session?.let { app.sessionApi(it) } }
    // Календарь «только для чтения» и т.п. — не «нужен вход» (§2); 401 после refresh — он.
    val onDead = { session?.let { app.hub.setStatus(it.accountKey, ru.letotam.ddmail.sync.AccountStatus.NeedsLogin) }; Unit }
    val calVm = api?.let { viewModel(key = "cal-$key") { CalendarViewModel(app, it, onDead) } }
    val tasksVm = api?.let { viewModel(key = "tasks-$key") { TasksViewModel(app, it, onDead) } }
    val contactsVm = api?.let { viewModel(key = "contacts-$key") { ContactsViewModel(app, it, onDead) } }
    var section by rememberSaveable { mutableStateOf(Section.Mail) }
    val context = LocalContext.current
    // WebView беседы живёт всё время работы приложения: уход в календарь и обратно ленту не теряет.
    val web = remember { ConversationWeb(context, ConversationWeb.proxyClient(app.http)) }

    // Вернулись в приложение — подтянуть новое; WebSocket и IDLE — пока приложение на экране.
    LifecycleResumeEffect(key) {
        mailVm.sync()
        mailVm.live.start()
        onPauseOrDispose { mailVm.live.stop() }
    }
    // Изменения календаря с сервера — перевзвести напоминания, даже если календарь не открыт.
    LaunchedEffect(key) {
        if (api != null) app.calendarChanged.collect { launch { runCatching { app.reminders.refresh(api) } } }
    }
    LaunchedEffect(key) { if (api != null) runCatching { app.reminders.refresh(api) } }

    // Из уведомления-напоминания: открыть событие в календаре.
    val eventReq by app.eventRequests.collectAsState()
    LaunchedEffect(eventReq) {
        eventReq?.let { (id, start) ->
            section = Section.Calendar
            calVm?.openById(id, start)
            app.eventRequests.value = null
        }
    }
    val openReq by app.openRequests.collectAsState()
    LaunchedEffect(openReq) { if (openReq != null) section = Section.Mail }
    SnoozeDialog(app)

    // Первый заход в календарь — спросить про работу в фоне (один раз).
    var askBattery by remember { mutableStateOf(false) }
    LaunchedEffect(section) {
        if (section == Section.Calendar && !app.calendarPrefs.batteryAsked()) {
            app.calendarPrefs.setBatteryAsked()
            if (!BackgroundWork.unrestricted(context) || BackgroundWork.isHuawei()) askBattery = true
        }
    }
    if (askBattery) BackgroundWorkDialog { askBattery = false }

    val wide = LocalConfiguration.current.screenWidthDp >= 600
    val selectedConv by mailVm.selected.collectAsState()
    val ime = WindowInsets.isImeVisible
    val barVisible = !wide && !ime && !(section == Section.Mail && selectedConv != null)
    val pick = { s: Section ->
        // Повторный тап по «Почте» в открытой беседе — к списку, как в iOS.
        if (s == section && s == Section.Mail && !wide) mailVm.open(null)
        section = s
    }

    Row(modifier.fillMaxSize()) {
        if (wide) {
            NavigationRail {
                Section.entries.forEach { s ->
                    NavigationRailItem(selected = section == s, onClick = { pick(s) }, icon = { Icon(s.icon, s.label) })
                }
            }
            VerticalDivider()
        }
        Column(Modifier.weight(1f).fillMaxHeight()) {
            val content = Modifier.weight(1f)
            when (section) {
                Section.Mail -> MailScreen(mailVm, accounts, web, content)
                Section.Calendar -> if (calVm != null) CalendarScreen(calVm, content) else NoServer(content)
                Section.Tasks -> if (tasksVm != null) TasksScreen(tasksVm, content) else NoServer(content)
                Section.Contacts -> if (contactsVm != null) {
                    ContactsScreen(contactsVm, mailVm.avatars, onMail = { addr ->
                        section = Section.Mail
                        mailVm.compose.openNewTo(addr)
                    }, content)
                } else NoServer(content)
            }
            if (barVisible) NavigationBar {
                Section.entries.forEach { s ->
                    NavigationBarItem(selected = section == s, onClick = { pick(s) }, icon = { Icon(s.icon, s.label) })
                }
            }
        }
    }
}

/** Раздел без источника: календарь и контакты внешних учёток — через CalDAV/CardDAV. */
@Composable
private fun NoServer(modifier: Modifier) {
    androidx.compose.foundation.layout.Box(modifier.fillMaxSize(), contentAlignment = androidx.compose.ui.Alignment.Center) {
        androidx.compose.material3.Text(
            "Нет источника: подключите сервер ddmail или адрес CalDAV/CardDAV в настройках учётки.",
            modifier = androidx.compose.ui.Modifier.padding(24.dp),
        )
    }
}
