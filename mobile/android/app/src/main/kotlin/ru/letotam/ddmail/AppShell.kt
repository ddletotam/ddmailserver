package ru.letotam.ddmail

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
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
    session: Session,
    status: SessionStatus,
    mailVm: MailViewModel,
    onDead: () -> Unit,
    onRelogin: () -> Unit,
    onLogout: () -> Unit,
    modifier: Modifier = Modifier,
) {
    val key = "${session.serverUrl}-${session.token.hashCode()}"
    val api = remember(key) { app.sessionApi(session) }
    val calVm = viewModel(key = "cal-$key") { CalendarViewModel(app, api, onDead) }
    val tasksVm = viewModel(key = "tasks-$key") { TasksViewModel(app, api, onDead) }
    val contactsVm = viewModel(key = "contacts-$key") { ContactsViewModel(app, api, onDead) }
    var section by rememberSaveable { mutableStateOf(Section.Mail) }
    val context = LocalContext.current
    // WebView беседы живёт всю сессию: уход в календарь и обратно ленту не теряет.
    val web = remember(key) { ConversationWeb(context, ConversationWeb.proxyClient(app.http)) }

    // Вернулись в приложение — подтянуть новое; WebSocket — пока приложение на экране.
    LifecycleResumeEffect(key) {
        mailVm.sync()
        mailVm.live.start()
        onPauseOrDispose { mailVm.live.stop() }
    }
    // Изменения календаря с сервера — перевзвести напоминания, даже если календарь не открыт.
    LaunchedEffect(key) {
        app.calendarChanged.collect { launch { runCatching { app.reminders.refresh(api) } } }
    }
    LaunchedEffect(key) { runCatching { app.reminders.refresh(api) } }

    // Из уведомления-напоминания: открыть событие в календаре.
    val eventReq by app.eventRequests.collectAsState()
    LaunchedEffect(eventReq) {
        eventReq?.let { (id, start) ->
            section = Section.Calendar
            calVm.openById(id, start)
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
                Section.Mail -> MailScreen(mailVm, session, status, onRelogin, onLogout, web, content)
                Section.Calendar -> CalendarScreen(calVm, content)
                Section.Tasks -> TasksScreen(tasksVm, content)
                Section.Contacts -> ContactsScreen(contactsVm, mailVm.avatars, onMail = { addr ->
                    section = Section.Mail
                    mailVm.compose.openNewTo(addr)
                }, content)
            }
            if (barVisible) NavigationBar {
                Section.entries.forEach { s ->
                    NavigationBarItem(selected = section == s, onClick = { pick(s) }, icon = { Icon(s.icon, s.label) })
                }
            }
        }
    }
}
