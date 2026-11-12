package ru.letotam.ddmail.mail

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Button
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.VerticalDivider
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.viewinterop.AndroidView
import android.view.ViewGroup
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import ru.letotam.ddmail.DdmailApp
import ru.letotam.ddmail.R
import ru.letotam.ddmail.compose.ComposeSheetDialog
import ru.letotam.ddmail.compose.ComposerBar
import ru.letotam.ddmail.conversation.ConversationWeb
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.ime
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ExtendedFloatingActionButton
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.*
import androidx.compose.material.icons.filled.*
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.ui.zIndex
import ru.letotam.ddmail.sync.AccountStatus
import ru.letotam.ddmail.sync.Conversation
import ru.letotam.ddmail.sync.MailSource

private val Accent = Color(0xFF047857)

/**
 * Клавиатура занимает больше половины окна (EMUI в разложенном Mate XT —
 * ~85%): тогда шапки прячутся, иначе строки ввода не видно. В сложенном
 * виде клавиатура — около трети, шапки остаются.
 */
@Composable
private fun imeCrowds(): Boolean {
    val density = androidx.compose.ui.platform.LocalDensity.current
    val imePx = WindowInsets.ime.getBottom(density)
    val heightPx = with(density) { androidx.compose.ui.platform.LocalConfiguration.current.screenHeightDp.dp.toPx() }
    return imePx > heightPx / 2
}

/** Открытая беседа — как `#10b981` выделения в списке десктопа. */
private val SelectedRow = Color(0xFF10B981)

/**
 * Почта (контракт §5–6). Раскладка — по ширине окна: Compact (Mate XT
 * сложен, 350 dp) — список, беседа поверх; шире (712 / 1107 dp) — список и
 * беседа рядом. Табы папок наверху во всех состояниях.
 */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun MailScreen(
    vm: MailViewModel,
    accounts: AccountActions,
    web: ConversationWeb,
    modifier: Modifier = Modifier,
) {
    val sources by vm.sources.collectAsState()
    val statuses by vm.statuses.collectAsState()
    val s by vm.state.collectAsState()
    val syncing by vm.syncing.collectAsState()
    val selectedId by vm.selected.collectAsState()
    val width = LocalConfiguration.current.screenWidthDp
    val wide = width >= 600
    // Открытая беседа ищется по id в полном видимом списке: дельта могла
    // переставить строки.
    val selected = s.conversations.firstOrNull { it.ref == selectedId }

    // Синхронизация на возврат и WebSocket — в оболочке (AppShell): они нужны
    // и пока на экране календарь.
    val picked by vm.picked.collectAsState()
    BackHandler(enabled = picked.isNotEmpty()) { vm.clearPicks() }
    BackHandler(enabled = picked.isEmpty() && !wide && selectedId != null) { vm.open(null) }
    val snack = vm.snackHost
    val notice by vm.notice.collectAsState()
    LaunchedEffect(notice) {
        notice?.let {
            snack.showSnackbar(it)
            vm.notice.value = null
        }
    }
    var folderPicker by remember { mutableStateOf(false) }
    var deleteAsk by remember { mutableStateOf(false) }
    if (folderPicker) FolderPickerDialog(vm) { folderPicker = false }
    if (deleteAsk) DeleteDialog(vm) { deleteAsk = false }
    SpamDialog(vm)
    LaunchedEffect(wide) { vm.setWide(wide) }

    // Один WebView на всю сессию (создаёт оболочка): открытие беседы меняет
    // только документ, уход в календарь и обратно ленту не теряет (контракт §8).
    val context = LocalContext.current
    DisposableEffect(web) {
        web.host = object : ConversationWeb.Host {
            override fun onReady() {}
            override fun onRequestOlder() = vm.conversation.older()
            override fun onOpenAttachment(uid: Long, index: Int) =
                vm.conversation.openAttachment(uid, index, vm.conversation.attachmentName(uid, index))
            override fun loadPart(uid: Long, cid: String) = vm.conversation.part(uid, cid)
            override fun onBubbleMenu(uid: Long) = vm.showBubbleMenu(uid)
            override fun onAttachmentMenu(uid: Long, index: Int) {
                vm.attachmentMenu.value = uid to index
            }
            override fun onAllowSender(uid: Long) = vm.conversation.allowSender(uid)
        }
        // Пропуск внешних картинок (спам — только по нажатию); не снимается
        // на уходе экрана: WebView живёт дольше него.
        web.gate = vm.conversation.gate
        onDispose { web.host = null }
    }
    LaunchedEffect(web) {
        for (js in vm.conversation.js) web.eval(js)
    }

    ComposeSheetDialog(vm.compose)
    BubbleMenu(vm)
    AttachmentMenu(vm)
    val app = context.applicationContext as DdmailApp
    ru.letotam.ddmail.settings.PrefetchQuestion(app.settings)
    var settingsOpen by remember { mutableStateOf(false) }
    if (settingsOpen) ru.letotam.ddmail.settings.SettingsDialog(app.settings, sources, statuses, accounts.closing { settingsOpen = false }) { settingsOpen = false }
    // Открытие беседы из уведомления.
    val openReq by app.openRequests.collectAsState()
    LaunchedEffect(openReq) {
        openReq?.let {
            vm.open(it)
            app.openRequests.value = null
        }
    }

    // Клавиатура EMUI на разложенном Mate XT занимает ~85% высоты: пока она
    // открыта, прячем всё, кроме строки ввода, иначе композера не видно.
    val ime = imeCrowds()
    // Один хост уведомлений на весь экран: в сложенном виде список не виден,
    // а «Сохранено в Загрузки» приходит из открытой беседы. Над FAB/строкой ввода.
    Box(modifier.fillMaxSize().imePadding()) {
    SnackbarHost(snackHost(vm), Modifier.align(Alignment.BottomCenter).padding(bottom = 84.dp).zIndex(1f))
    Column(Modifier.fillMaxSize()) {
        val q by vm.query.collectAsState()
        // Поле поиска видно и с открытой клавиатурой — в него и печатают.
        // Узкое окно с открытой беседой: у неё своя шапка со стрелкой назад,
        // шапка приложения и поиск только съедали бы высоту (§6).
        val convOnly = !wide && selected != null
        if (q != null && !convOnly) SearchBar(q!!, onChange = { vm.query.value = it }, onClose = { vm.query.value = null })
        else if (!ime) {
            if (!convOnly) TopBar(sources, statuses, syncing, onSearch = { vm.query.value = "" }, onSpam = vm::openSpam, onSettings = { settingsOpen = true }, onAdd = accounts.add)
            // «Нужен вход» — у каждой учётки своя плашка (mobile-external-accounts §7).
            sources.filter { statuses[it.accountKey] == AccountStatus.NeedsLogin }.forEach { src ->
                DeadSessionBanner(src.title) { accounts.relogin(src.accountKey) }
            }
        }
        if (wide) {
            Row(Modifier.fillMaxSize()) {
                ListPane(s, syncing, selectedId, picked, vm, { folderPicker = true }, { deleteAsk = true }, Modifier.width(if (width >= 840) 400.dp else 340.dp).fillMaxHeight())
                VerticalDivider()
                ConversationPane(selected, web, vm, onBack = null, Modifier.weight(1f).fillMaxHeight())
            }
        } else if (selected != null) {
            ConversationPane(selected, web, vm, onBack = { vm.open(null) }, Modifier.fillMaxSize())
        } else {
            ListPane(s, syncing, selectedId, picked, vm, { folderPicker = true }, { deleteAsk = true }, Modifier.fillMaxSize())
        }
    }
    }
}

@Composable
private fun TopBar(
    sources: List<MailSource>,
    statuses: Map<String, AccountStatus>,
    syncing: Boolean,
    onSearch: () -> Unit,
    onSpam: () -> Unit,
    onSettings: () -> Unit,
    onAdd: () -> Unit,
) {
    val offline = sources.mapNotNull { src -> (statuses[src.accountKey] as? AccountStatus.Offline)?.let { src.title } }
    var menu by remember { mutableStateOf(false) }
    Row(
        Modifier.fillMaxWidth().padding(start = 16.dp, end = 4.dp, top = 6.dp, bottom = 2.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(Modifier.weight(1f)) {
            Text(stringResource(R.string.mail_title), style = MaterialTheme.typography.titleLarge)
            val sub = when {
                offline.isNotEmpty() && offline.size == sources.size -> stringResource(R.string.status_offline)
                offline.isNotEmpty() -> "Нет связи: " + offline.joinToString(", ")
                syncing -> stringResource(R.string.status_syncing)
                else -> sources.joinToString(", ") { it.title }
            }
            Text(
                sub,
                style = MaterialTheme.typography.bodySmall,
                color = if (offline.isNotEmpty()) MaterialTheme.colorScheme.error
                else MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
        }
        IconButton(onClick = onSearch) { Icon(Icons.Filled.Search, "Поиск") }
        Box {
            IconButton(onClick = { menu = true }) { Icon(Icons.Filled.MoreVert, "Меню") }
            DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
                // Папка «Спам» скрыта всегда (таба нет) — вход только отсюда.
                DropdownMenuItem(leadingIcon = { Icon(Icons.Filled.Report, null) }, text = { Text("Спам") }, onClick = {
                    menu = false
                    onSpam()
                })
                DropdownMenuItem(leadingIcon = { Icon(Icons.Filled.Settings, null) }, text = { Text("Настройки") }, onClick = {
                    menu = false
                    onSettings()
                })
                DropdownMenuItem(leadingIcon = { Icon(Icons.Filled.PersonAdd, null) }, text = { Text("Добавить учётку") }, onClick = {
                    menu = false
                    onAdd()
                })
            }
        }
    }
}

/** Snackbar-хост живёт в MailScreen; сюда — через ViewModel, чтобы не тащить параметр. */
@Composable
private fun snackHost(vm: MailViewModel): SnackbarHostState = remember(vm) { vm.snackHost }

/**
 * Панель действий над выбранными беседами (§5) — одна строка иконок
 * (иконки вместо надписей — правило мобильных клиентов, §0): закрыть,
 * счётчик, выбрать все, прочитано, непрочитано, в папку, в спам, удалить.
 */
@Composable
private fun SelectionBar(n: Int, vm: MailViewModel, onMove: () -> Unit, onDelete: () -> Unit) {
    Row(
        Modifier.fillMaxWidth().background(Color(0xFFE9EDF1)).padding(horizontal = 2.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        IconButton(onClick = vm::clearPicks) { Icon(Icons.Filled.Close, "Снять выбор") }
        Text("$n", fontWeight = FontWeight.SemiBold, maxLines = 1)
        Spacer(Modifier.weight(1f))
        IconButton(onClick = vm::pickAll) { Icon(Icons.Filled.SelectAll, "Выбрать все") }
        IconButton(onClick = { vm.markPicked(true) }) { Icon(Icons.Filled.Drafts, "Прочитано") }
        IconButton(onClick = { vm.markPicked(false) }) { Icon(Icons.Filled.MarkEmailUnread, "Непрочитано") }
        IconButton(onClick = onMove) { Icon(Icons.AutoMirrored.Filled.DriveFileMove, "В папку") }
        IconButton(onClick = vm::askSpamPicked) { Icon(Icons.Filled.Report, "В спам") }
        IconButton(onClick = onDelete) { Icon(Icons.Filled.Delete, "Удалить", tint = MaterialTheme.colorScheme.error) }
    }
}

/**
 * Выбор в папке «Спам»: «Не спам» (письма во «Входящие») и «Не спам и
 * доверять отправителю» (ещё и allow-правило на отправителя на сервере).
 */
@Composable
private fun SpamSelectionBar(n: Int, vm: MailViewModel) {
    Row(
        Modifier.fillMaxWidth().background(Color(0xFFE9EDF1)).padding(horizontal = 2.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        IconButton(onClick = vm::clearPicks) { Icon(Icons.Filled.Close, "Снять выбор") }
        Text("$n", fontWeight = FontWeight.SemiBold, maxLines = 1)
        Spacer(Modifier.weight(1f))
        IconButton(onClick = vm::pickAll) { Icon(Icons.Filled.SelectAll, "Выбрать все") }
        IconButton(onClick = { vm.notSpamPicked(trust = false) }) { Icon(Icons.Filled.MoveToInbox, "Не спам") }
        IconButton(onClick = { vm.notSpamPicked(trust = true) }) { Icon(Icons.Filled.VerifiedUser, "Не спам и доверять отправителю") }
    }
}

@Composable
private fun FolderPickerDialog(vm: MailViewModel, close: () -> Unit) {
    // Закрыли без выбора — выбор, сделанный свайпом, не должен остаться висеть.
    val dismiss = {
        vm.clearPicks()
        close()
    }
    AlertDialog(
        onDismissRequest = dismiss,
        title = { Text("В папку") },
        text = {
            Column {
                (listOf<Pair<String?, String>>(null to "📥 Во «Входящие»") + vm.folderTargets()).forEach { (id, title) ->
                    Text(
                        title,
                        modifier = Modifier.fillMaxWidth().clickable {
                            vm.movePicked(id)
                            close()
                        }.padding(vertical = 14.dp),
                        style = MaterialTheme.typography.bodyLarge,
                    )
                }
            }
        },
        confirmButton = { IconButton(onClick = close) { Icon(Icons.Filled.Close, "Отмена") } },
    )
}

/**
 * Удаление — с подтверждением до, а не «отменить» после: API восстановления
 * нет, а у внешних учёток удаление сразу уходит на источник (§5).
 */
@Composable
private fun DeleteDialog(vm: MailViewModel, close: () -> Unit) {
    val (convs, msgs) = remember { vm.pickedSummary() }
    AlertDialog(
        onDismissRequest = close,
        title = { Text("Удалить беседы?") },
        text = { Text("Бесед: $convs, писем в них: $msgs. Письма будут удалены на сервере; у внешних ящиков — и в исходном ящике.") },
        confirmButton = {
            IconButton(onClick = {
                vm.deletePicked()
                close()
            }) { Icon(Icons.Filled.Delete, "Удалить", tint = MaterialTheme.colorScheme.error) }
        },
        dismissButton = { IconButton(onClick = close) { Icon(Icons.Filled.Close, "Отмена") } },
    )
}

/**
 * «В спам» — с подтверждением, как удаление: письма удаляются, отправитель
 * блокируется. Два ответа, как на десктопе (§5а): адрес — этот отправитель,
 * домен — против спамеров с меняющейся частью до `@`.
 */
@Composable
private fun SpamDialog(vm: MailViewModel) {
    val ask by vm.spamAsk.collectAsState()
    val list = ask?.refs ?: return
    val rules = ask?.rules == true
    val (convs, msgs) = remember(list) { vm.spamSummary(list) }
    val close = { vm.spamAsk.value = null }
    AlertDialog(
        onDismissRequest = close,
        title = { Text("В спам?") },
        text = {
            Text(
                if (rules) {
                    "Бесед: $convs, писем в них: $msgs. Письма будут удалены, отправитель — заблокирован. " +
                        "«Домен» останавливает спам с меняющихся адресов одного домена."
                } else {
                    // Внешний ящик: правил «адрес/домен» нет — их держит только сервер ddmail.
                    "Бесед: $convs, писем в них: $msgs. Письма уйдут в папку «Спам» ящика."
                },
            )
        },
        confirmButton = {
            Row {
                if (rules) {
                    TextButton(onClick = { vm.spamConfirmed("address") }) { Text("Адрес") }
                    TextButton(onClick = { vm.spamConfirmed("domain") }) { Text("Домен", color = MaterialTheme.colorScheme.error) }
                } else {
                    TextButton(onClick = { vm.spamConfirmed("address") }) { Text("В спам", color = MaterialTheme.colorScheme.error) }
                }
            }
        },
        dismissButton = { IconButton(onClick = close) { Icon(Icons.Filled.Close, "Отмена") } },
    )
}

/** Поиск по диалогам: поле вместо шапки; результаты — вместо списка папки. */
@Composable
private fun SearchBar(q: String, onChange: (String) -> Unit, onClose: () -> Unit) {
    val focus = remember { androidx.compose.ui.focus.FocusRequester() }
    LaunchedEffect(Unit) { focus.requestFocus() }
    BackHandler(onBack = onClose)
    Row(Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 6.dp), verticalAlignment = Alignment.CenterVertically) {
        IconButton(onClick = onClose) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Закрыть поиск") }
        androidx.compose.material3.OutlinedTextField(
            value = q,
            onValueChange = onChange,
            placeholder = { Text("Имя, адрес или тема") },
            singleLine = true,
            modifier = Modifier.weight(1f).focusRequester(focus),
        )
    }
}

/** Несмываемая плашка (контракт §2): кэш виден, но почта не обновляется. */
@Composable
private fun DeadSessionBanner(title: String, onRelogin: () -> Unit) {
    Surface(color = MaterialTheme.colorScheme.errorContainer, modifier = Modifier.fillMaxWidth()) {
        Row(Modifier.padding(horizontal = 16.dp, vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
            Text("$title: " + stringResource(R.string.session_dead_banner), Modifier.weight(1f), style = MaterialTheme.typography.bodyMedium)
            Button(onClick = onRelogin) { Text(stringResource(R.string.action_relogin)) }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ListPane(
    s: MailState,
    syncing: Boolean,
    selectedId: String?,
    picked: Set<String>,
    vm: MailViewModel,
    onMove: () -> Unit,
    onDelete: () -> Unit,
    modifier: Modifier,
) {
    Column(modifier) {
        // На время выбора панель действий заменяет полосу табов (§5).
        if (picked.isEmpty()) FolderTabs(s, vm::selectFolder)
        else if (s.inSpam) SpamSelectionBar(picked.size, vm)
        else SelectionBar(picked.size, vm, onMove, onDelete)
        HorizontalDivider()
        // Крутится только по жесту: фоновые синки показывает строка под заголовком.
        val pulling by vm.pulling.collectAsState()
        val identColors by vm.identityColors.collectAsState()
        PullToRefreshBox(isRefreshing = pulling, onRefresh = vm::pullRefresh, modifier = Modifier.fillMaxSize()) {
            if (s.loaded && s.conversations.isEmpty() && !syncing) {
                Text(
                    when {
                        vm.query.value != null -> "Ничего не нашлось."
                        s.inSpam -> "Спама нет."
                        else -> stringResource(if (s.active == null) R.string.list_empty_inbox else R.string.list_empty_folder)
                    },
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(24.dp),
                )
            }
            LazyColumn(Modifier.fillMaxSize()) {
                items(s.conversations, key = { it.accountKey + "\u0000" + it.id }) { c ->
                    val inPick = c.ref in picked
                    SwipeRow(
                        // В «Спам» жестов нет: там только смотрят и «Не спам».
                        enabled = picked.isEmpty() && !s.inSpam,
                        unread = c.unreadCount > 0,
                        onRight = { vm.toggleRead(c.ref) },
                        onLeft = {
                            vm.togglePick(c.ref)
                            onMove()
                        },
                    ) {
                    ConversationRow(
                        c,
                        avatars = vm.avatars,
                        tint = Color(identColors.colorOf(c.receivedBy)),
                        selected = c.ref == selectedId && picked.isEmpty(),
                        picked = inPick,
                        onClick = { if (picked.isNotEmpty()) vm.togglePick(c.ref) else vm.open(c.ref) },
                        onLongClick = { vm.togglePick(c.ref) },
                    )
                    }
                }
            }
            if (picked.isEmpty()) androidx.compose.material3.FloatingActionButton(
                onClick = vm.compose::openNew,
                containerColor = Accent,
                contentColor = Color.White,
                modifier = Modifier.align(Alignment.BottomEnd).padding(16.dp),
            ) { Icon(Icons.Filled.Edit, "Написать") }
        }
    }
}

/**
 * Табы папок, как в Telegram (§4 десктопа): текст, выбранный — акцентом с
 * подчёркиванием; таба «Все» нет — ничего не выбрано значит «Входящие». Пока
 * выбрана папка, первым стоит иконка «Входящих» со счётчиком — путь назад.
 */
@Composable
private fun FolderTabs(s: MailState, onSelect: (String?) -> Unit) {
    if (s.tabs.isEmpty()) return
    LazyRow(
        Modifier.fillMaxWidth().padding(horizontal = 8.dp),
        horizontalArrangement = Arrangement.spacedBy(4.dp),
    ) {
        if (s.active != null) {
            item(key = "inbox") {
                Tab("", s.inboxUnread, selected = false, icon = Icons.Filled.Inbox, description = stringResource(R.string.tab_inbox_back)) { onSelect(null) }
            }
        }
        items(s.tabs, key = { it.id }) { t ->
            val selected = t.index == s.active
            val title = if (t.icon.isBlank()) t.name else "${t.icon} ${t.name}"
            // Повторное касание выбранного таба — назад во «Входящие».
            Tab(title, t.unread, selected) { onSelect(if (selected) null else t.id) }
        }
    }
}

@Composable
private fun Tab(
    title: String,
    unread: Int,
    selected: Boolean,
    icon: androidx.compose.ui.graphics.vector.ImageVector? = null,
    description: String? = null,
    onClick: () -> Unit,
) {
    Column(
        Modifier.clip(RoundedCornerShape(6.dp)).clickable(onClick = onClick).padding(horizontal = 10.dp, vertical = 8.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            if (icon != null) Icon(icon, description, tint = MaterialTheme.colorScheme.onSurfaceVariant)
            if (title.isNotEmpty()) Text(
                title,
                color = if (selected) Accent else MaterialTheme.colorScheme.onSurface,
                fontWeight = if (selected) FontWeight.SemiBold else FontWeight.Normal,
                style = MaterialTheme.typography.bodyMedium,
                maxLines = 1,
            )
            if (unread > 0) {
                Spacer(Modifier.width(6.dp))
                Text(
                    unread.toString(),
                    color = Color.White,
                    fontSize = 11.sp,
                    modifier = Modifier
                        .clip(RoundedCornerShape(8.dp))
                        .background(if (selected) Accent else Color(0xFF9AA4AF))
                        .padding(horizontal = 6.dp, vertical = 1.dp),
                )
            }
        }
        Spacer(Modifier.height(4.dp))
        Box(
            Modifier.height(2.dp).fillMaxWidth()
                .background(if (selected) Accent else Color.Transparent),
        )
    }
}

/**
 * Свайпы по строке (контракт §11 — предложение для пробы): вправо —
 * прочитано/непрочитано, влево — «В папку…». Удаления свайпом нет: оно
 * необратимо, а свайп легко сделать случайно. Строка всегда возвращается
 * на место — действие срабатывает, строка не «улетает».
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun SwipeRow(enabled: Boolean, unread: Boolean, onRight: () -> Unit, onLeft: () -> Unit, content: @Composable () -> Unit) {
    val state = androidx.compose.material3.rememberSwipeToDismissBoxState(
        confirmValueChange = { v ->
            when (v) {
                androidx.compose.material3.SwipeToDismissBoxValue.StartToEnd -> onRight()
                androidx.compose.material3.SwipeToDismissBoxValue.EndToStart -> onLeft()
                else -> {}
            }
            false
        },
        positionalThreshold = { it * 0.35f },
    )
    androidx.compose.material3.SwipeToDismissBox(
        state = state,
        enableDismissFromStartToEnd = enabled,
        enableDismissFromEndToStart = enabled,
        backgroundContent = {
            val right = state.dismissDirection == androidx.compose.material3.SwipeToDismissBoxValue.StartToEnd
            Box(
                Modifier.fillMaxSize().background(if (right) Accent else Color(0xFF1D4ED8)).padding(horizontal = 20.dp),
                contentAlignment = if (right) Alignment.CenterStart else Alignment.CenterEnd,
            ) {
                Icon(
                    when {
                        !right -> Icons.AutoMirrored.Filled.DriveFileMove
                        unread -> Icons.Filled.Drafts
                        else -> Icons.Filled.MarkEmailUnread
                    },
                    contentDescription = null,
                    tint = Color.White,
                    modifier = Modifier.size(28.dp),
                )
            }
        },
    ) { Box(Modifier.background(MaterialTheme.colorScheme.background)) { content() } }
}

/** Строгий двухстрочный формат (§4 десктопа): имя / тема последнего письма. */
@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun ConversationRow(
    c: Conversation,
    avatars: Avatars,
    tint: Color,
    selected: Boolean,
    picked: Boolean,
    onClick: () -> Unit,
    onLongClick: () -> Unit,
) {
    val unread = c.unreadCount > 0
    // Как у десктопа: в покое — цвет группы адреса получателя, открытая
    // беседа — сплошной зелёный с белым текстом (пастель группы бывает
    // зелёной, светлое выделение на ней не видно).
    val onSel = selected && !picked
    val main = if (onSel) Color.White else MaterialTheme.colorScheme.onSurface
    val dim = if (onSel) Color.White.copy(alpha = 0.85f) else MaterialTheme.colorScheme.onSurfaceVariant
    Row(
        Modifier
            .fillMaxWidth()
            .background(
                when {
                    picked -> Color(0xFFBFD8FF)
                    selected -> SelectedRow
                    else -> tint
                },
            )
            .combinedClickable(onClick = onClick, onLongClick = onLongClick)
            .padding(horizontal = 14.dp, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        if (picked) {
            Box(Modifier.size(44.dp).clip(CircleShape).background(Accent), contentAlignment = Alignment.Center) {
                Icon(Icons.Filled.Check, "Выбрано", tint = Color.White)
            }
        } else {
            Avatar(c, avatars)
        }
        Spacer(Modifier.width(12.dp))
        Column(Modifier.weight(1f)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    c.label.ifBlank { c.counterparts.firstOrNull()?.addr.orEmpty() },
                    fontWeight = if (unread) FontWeight.Bold else FontWeight.SemiBold,
                    color = main,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f),
                )
                Spacer(Modifier.width(8.dp))
                Text(
                    formatListDate(c.lastDateTs),
                    style = MaterialTheme.typography.bodySmall,
                    color = if (unread && !onSel) Accent else dim,
                )
            }
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    c.lastSubject.ifBlank { stringResource(R.string.no_subject) },
                    style = MaterialTheme.typography.bodyMedium,
                    color = dim,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f),
                )
                // У спама счётчика нет (жирность остаётся): число зазывает открыть.
                if (unread && !c.spam) {
                    Spacer(Modifier.width(8.dp))
                    Text(
                        c.unreadCount.toString(),
                        color = if (onSel) Accent else Color.White,
                        fontSize = 11.sp,
                        modifier = Modifier.clip(RoundedCornerShape(9.dp)).background(if (onSel) Color.White else Accent)
                            .padding(horizontal = 7.dp, vertical = 1.dp),
                    )
                }
            }
        }
    }
}

/**
 * Аватарка с сервера, без неё — инициалы на цвете, стабильном для диалога.
 * У спама — только инициалы: за аватаркой сервер ходит на домен отправителя
 * (favicon, BIMI), а это внешний запрос по сигналу спамера.
 */
@Composable
private fun Avatar(c: Conversation, avatars: Avatars) {
    val email = if (c.spam) "" else c.counterparts.firstOrNull()?.addr.orEmpty()
    val image by androidx.compose.runtime.produceState(if (email.isEmpty()) null else avatars.cached(email), email) {
        if (email.isNotEmpty()) value = avatars.get(email)
    }
    image?.let {
        androidx.compose.foundation.Image(
            it, contentDescription = null,
            contentScale = androidx.compose.ui.layout.ContentScale.Crop,
            modifier = Modifier.size(44.dp).clip(CircleShape),
        )
        return
    }
    val name = c.label.ifBlank { c.counterparts.firstOrNull()?.addr.orEmpty() }
    val initials = name.split(' ', '.', '@', '_', '-')
        .filter { it.isNotBlank() }
        .take(2)
        .joinToString("") { it.first().uppercase() }
        .ifEmpty { "?" }
    val palette = listOf(0xFF0E7490, 0xFF7C3AED, 0xFFB45309, 0xFF047857, 0xFFBE185D, 0xFF1D4ED8, 0xFF4D7C0F)
    val color = Color(palette[Math.floorMod(c.id.hashCode(), palette.size)])
    Box(Modifier.size(44.dp).clip(CircleShape).background(color), contentAlignment = Alignment.Center) {
        Text(initials, color = Color.White, fontWeight = FontWeight.SemiBold)
    }
}

/** Панель беседы: нативная шапка и лента в WebView (контракт §8). */
@Composable
private fun ConversationPane(c: Conversation?, web: ConversationWeb, vm: MailViewModel, onBack: (() -> Unit)?, modifier: Modifier) {
    if (c == null) {
        Box(modifier, contentAlignment = Alignment.Center) {
            Text(stringResource(R.string.pick_conversation), color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        return
    }
    @OptIn(ExperimentalLayoutApi::class)
    val ime = imeCrowds()
    Column(modifier) {
        if (!ime) Row(Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 6.dp), verticalAlignment = Alignment.CenterVertically) {
            if (onBack != null) IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "К списку") }
            Column(Modifier.weight(1f).padding(start = if (onBack == null) 8.dp else 0.dp)) {
                Text(c.label, style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                Text(
                    c.counterparts.joinToString(", ") { it.addr } + if (c.receivedBy.isNotBlank()) " → ${c.receivedBy}" else "",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
            }
            if (!c.spam) IconButton(onClick = { vm.askSpam(c.ref) }) { Icon(Icons.Filled.Report, "В спам") }
        }
        HorizontalDivider()
        AndroidView(
            // Тот же WebView переезжает между раскладками (сложили/разложили):
            // из прежнего родителя его надо вынуть.
            factory = { web.view.also { (it.parent as? ViewGroup)?.removeView(it) } },
            modifier = Modifier.fillMaxWidth().weight(1f),
        )
        HorizontalDivider()
        ComposerBar(vm.compose, c)
    }
}

/** Долгое нажатие на вложение: открыть или сохранить в «Загрузки». */
@Composable
private fun AttachmentMenu(vm: MailViewModel) {
    val m by vm.attachmentMenu.collectAsState()
    val (uid, index) = m ?: return
    val close = { vm.attachmentMenu.value = null }
    val name = vm.conversation.attachmentName(uid, index) ?: "Вложение"
    AlertDialog(
        onDismissRequest = close,
        title = { Text(name, maxLines = 2, overflow = TextOverflow.Ellipsis) },
        text = {
            Column {
                MenuRow(Icons.AutoMirrored.Filled.OpenInNew, "Открыть", close) { vm.conversation.openAttachment(uid, index, name) }
                MenuRow(Icons.Filled.Download, "В «Загрузки»", close) { vm.conversation.saveAttachment(uid, index) { msg -> vm.notice.value = msg } }
            }
        },
        confirmButton = { IconButton(onClick = close) { Icon(Icons.Filled.Close, "Отмена") } },
    )
}

/** Долгое нажатие на пузырь: ответить именно на это письмо, переслать, копировать. */
@Composable
private fun BubbleMenu(vm: MailViewModel) {
    val b by vm.bubbleMenu.collectAsState()
    val body = b ?: return
    val clipboard = LocalClipboardManager.current
    val close = { vm.bubbleMenu.value = null }
    AlertDialog(
        onDismissRequest = close,
        title = { Text(body.subject.ifBlank { stringResource(R.string.no_subject) }, maxLines = 2, overflow = TextOverflow.Ellipsis) },
        text = {
            Column {
                MenuRow(Icons.AutoMirrored.Filled.Reply, "Ответить", close) { vm.compose.replyTo(body) }
                MenuRow(Icons.AutoMirrored.Filled.Forward, "Переслать", close) { vm.compose.openForward(body) }
                MenuRow(Icons.Filled.ContentCopy, "Копировать", close) {
                    val plain = body.text?.takeIf { it.isNotBlank() }
                        ?: android.text.Html.fromHtml(body.html.orEmpty(), android.text.Html.FROM_HTML_MODE_COMPACT).toString()
                    clipboard.setText(AnnotatedString(plain.trim()))
                }
            }
        },
        confirmButton = { IconButton(onClick = close) { Icon(Icons.Filled.Close, "Отмена") } },
    )
}

/** Пункт меню-диалога: иконка и короткое слово. */
@Composable
private fun MenuRow(icon: androidx.compose.ui.graphics.vector.ImageVector, title: String, close: () -> Unit, action: () -> Unit) {
    Row(
        Modifier.fillMaxWidth().clickable {
            action()
            close()
        }.padding(vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(icon, null, tint = Accent)
        Spacer(Modifier.width(16.dp))
        Text(title, style = MaterialTheme.typography.bodyLarge)
    }
}

/** Действия над учётками, которые живут выше экрана почты (AppViewModel). */
class AccountActions(
    val add: () -> Unit,
    /** «Войти» у учётки с «нужен вход». */
    val relogin: (String) -> Unit,
    val edit: (String) -> Unit,
    val remove: (String) -> Unit,
) {
    /** Те же действия, но сначала закрыть окно (настройки). */
    fun closing(close: () -> Unit) = AccountActions(
        add = { close(); add() },
        relogin = { close(); relogin(it) },
        edit = { close(); edit(it) },
        remove = { close(); remove(it) },
    )
}
