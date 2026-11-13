package ru.letotam.ddmail

import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.CalendarMonth
import androidx.compose.material.icons.filled.ChecklistRtl
import androidx.compose.material.icons.filled.Contacts
import androidx.compose.material.icons.filled.Mail
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.LifecycleResumeEffect
import androidx.lifecycle.viewmodel.compose.viewModel
import kotlinx.coroutines.launch
import ru.letotam.ddmail.auth.Session
import ru.letotam.ddmail.calendar.CalendarList
import ru.letotam.ddmail.calendar.CalendarScreen
import ru.letotam.ddmail.calendar.CalendarViewModel
import ru.letotam.ddmail.calendar.SnoozeDialog
import ru.letotam.ddmail.contacts.AddressBookList
import ru.letotam.ddmail.contacts.ContactsScreen
import ru.letotam.ddmail.contacts.ContactsViewModel
import ru.letotam.ddmail.conversation.ConversationWeb
import ru.letotam.ddmail.mail.AccountActions
import ru.letotam.ddmail.mail.MailScreen
import ru.letotam.ddmail.mail.MailViewModel
import ru.letotam.ddmail.reminders.BackgroundWork
import ru.letotam.ddmail.reminders.BackgroundWorkDialog
import ru.letotam.ddmail.settings.SettingsDialog
import ru.letotam.ddmail.tasks.TasksScreen
import ru.letotam.ddmail.tasks.TasksViewModel
import ru.letotam.ddmail.ui.BurgerButton
import ru.letotam.ddmail.ui.BurgerItem
import ru.letotam.ddmail.ui.BurgerMenu
import ru.letotam.ddmail.ui.LocalBurger

enum class Section(val icon: ImageVector, val label: String) {
    Mail(Icons.Filled.Mail, "Почта"),
    Calendar(Icons.Filled.CalendarMonth, "Календарь"),
    Contacts(Icons.Filled.Contacts, "Контакты"),
    Tasks(Icons.Filled.ChecklistRtl, "Задачи"),
}

/**
 * Разделы приложения (контракт §16, docs/mobile-layout.md): почта,
 * календарь, контакты, задачи. Навигация между ними — только бургер в первой
 * строке каждого раздела, как у десктопа: ни нижней панели, ни боковой
 * колонки иконок нет ни сложенным, ни разложенным.
 */
@Composable
fun AppShell(
    app: DdmailApp,
    session: Session?,
    mailVm: MailViewModel,
    accounts: AccountActions,
    modifier: Modifier = Modifier,
) {
    val key = app.pimKey()
    // Календарь и контакты — ddmail вместе с CalDAV/CardDAV внешних учёток; задачи — только ddmail.
    val pim = remember(key) { app.pim() }
    val api = remember(key) { session?.let { app.sessionApi(it) } }
    // Календарь «только для чтения» и т.п. — не «нужен вход» (§2); 401 после refresh — он.
    val onDead = { session?.let { app.hub.setStatus(it.accountKey, ru.letotam.ddmail.sync.AccountStatus.NeedsLogin) }; Unit }
    val calVm = pim?.let { viewModel(key = "cal-$key") { CalendarViewModel(app, it, onDead) } }
    val tasksVm = api?.let { viewModel(key = "tasks-$key") { TasksViewModel(app, it, onDead) } }
    val contactsVm = pim?.let { viewModel(key = "contacts-$key") { ContactsViewModel(app, it, onDead) } }
    var section by rememberSaveable { mutableStateOf(Section.Mail) }
    var menuOpen by remember { mutableStateOf(false) }
    var settingsOpen by rememberSaveable { mutableStateOf(false) }
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
        if (pim != null) app.calendarChanged.collect { launch { runCatching { app.reminders.refresh(pim) } } }
    }
    LaunchedEffect(key) { if (pim != null) runCatching { app.reminders.refresh(pim) } }
    // Адресная книга — в секцию «Контакты» поиска почты.
    LaunchedEffect(contactsVm) {
        contactsVm?.contacts?.collect { list -> mailVm.setAddressBook(list.orEmpty()) }
    }

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

    if (settingsOpen) {
        val sources by mailVm.sources.collectAsState()
        val statuses by mailVm.statuses.collectAsState()
        SettingsDialog(
            app.settings, sources, statuses, accounts.closing { settingsOpen = false },
            calendars = calVm?.let { vm -> { CalendarList(vm) } },
            addressBooks = contactsVm?.let { vm -> { AddressBookList(vm) } },
            folders = { ru.letotam.ddmail.mail.FolderSettings(mailVm) },
        ) { settingsOpen = false }
    }
    // Редактор папки — поверх всего: его открывают и меню полосы табов, и настройки.
    ru.letotam.ddmail.mail.FolderEditorDialog(mailVm)

    Box(modifier.fillMaxSize()) {
        CompositionLocalProvider(LocalBurger provides { menuOpen = true }) {
            val content = Modifier.fillMaxSize()
            when (section) {
                Section.Mail -> MailScreen(mailVm, accounts, web, content)
                Section.Calendar -> if (calVm != null) CalendarScreen(calVm, content) else NoServer(content)
                Section.Tasks -> if (tasksVm != null) TasksScreen(tasksVm, content) else NoServer(content)
                Section.Contacts -> if (contactsVm != null) {
                    ContactsScreen(contactsVm, mailVm.avatars, onMail = { addr ->
                        // «Написать» из карточки — та же новая беседа, что из поиска.
                        section = Section.Mail
                        mailVm.startNew(addr)
                    }, content)
                } else NoServer(content)
            }
        }
        if (menuOpen) {
            BurgerMenu(
                Section.entries.map { s -> BurgerItem(s.icon, s.label, selected = s == section) { section = s } } +
                    BurgerItem(Icons.Filled.Settings, "Настройки", selected = false) { settingsOpen = true },
                onDismiss = { menuOpen = false },
            )
        }
    }
}

/**
 * Раздел без источника: календарь и контакты внешних учёток — через
 * CalDAV/CardDAV. Бургер есть и здесь — иначе из раздела не уйти.
 */
@Composable
private fun NoServer(modifier: Modifier) {
    Column(modifier.fillMaxSize()) {
        Row(Modifier.fillMaxWidth().padding(start = 4.dp, top = 4.dp)) { BurgerButton() }
        Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
            Text(
                "Нет источника: подключите сервер ddmail или адрес CalDAV/CardDAV в настройках учётки.",
                modifier = Modifier.padding(24.dp),
            )
        }
    }
}
