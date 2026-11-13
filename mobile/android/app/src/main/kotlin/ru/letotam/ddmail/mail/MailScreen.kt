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
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.layout.heightIn
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
import ru.letotam.ddmail.ui.BurgerButton
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
 * Почта (контракт §5–6, docs/mobile-layout.md). Колонка списка — как левая
 * колонка десктопа, сверху вниз и ничего больше: бургер + поле поиска, полоса
 * табов папок, диалоги. Раскладка — по ширине окна: Compact (Mate XT сложен,
 * 350 dp) — колонка на весь экран, беседа поверх; шире (712 / 1107 dp) —
 * колонка слева, беседа справа.
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
    val newChat by vm.newChat.collectAsState()
    val width = LocalConfiguration.current.screenWidthDp
    val wide = width >= 600
    // Открытая беседа ищется по id в полном видимом списке: дельта могла
    // переставить строки.
    val selected = s.conversations.firstOrNull { it.ref == selectedId }

    // Синхронизация на возврат и WebSocket — в оболочке (AppShell): они нужны
    // и пока на экране календарь.
    val picked by vm.picked.collectAsState()
    BackHandler(enabled = picked.isEmpty() && !wide && selectedId != null) { vm.open(null) }
    BackHandler(enabled = newChat != null) { vm.closeNew() }
    BackHandler(enabled = picked.isNotEmpty()) { vm.clearPicks() }
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

    // Окно пересылки; новое письмо пишется в новой беседе (из поиска).
    ComposeSheetDialog(vm.compose)
    BubbleMenu(vm)
    AttachmentMenu(vm)
    val app = context.applicationContext as DdmailApp
    ru.letotam.ddmail.settings.PrefetchQuestion(app.settings)
    // Открытие беседы из уведомления.
    val openReq by app.openRequests.collectAsState()
    LaunchedEffect(openReq) {
        openReq?.let {
            vm.open(it)
            app.openRequests.value = null
        }
    }

    // Один хост уведомлений на весь экран: в сложенном виде список не виден,
    // а «Сохранено в Загрузки» приходит из открытой беседы. Над строкой ввода.
    Box(modifier.fillMaxSize().imePadding()) {
        SnackbarHost(snackHost(vm), Modifier.align(Alignment.BottomCenter).padding(bottom = 84.dp).zIndex(1f))
        val needsLogin = sources.filter { statuses[it.accountKey] == AccountStatus.NeedsLogin }
        val column = @Composable { m: Modifier ->
            MailColumn(s, syncing, selectedId, picked, vm, needsLogin, accounts, { folderPicker = true }, { deleteAsk = true }, m)
        }
        val right = @Composable { onBack: (() -> Unit)?, m: Modifier ->
            val to = newChat
            if (to != null) NewChatPane(to, vm, onBack, m)
            else ConversationPane(selected, web, vm, onBack, m)
        }
        if (wide) {
            Row(Modifier.fillMaxSize()) {
                column(Modifier.width(if (width >= 840) 400.dp else 340.dp).fillMaxHeight())
                VerticalDivider()
                right(null, Modifier.weight(1f).fillMaxHeight())
            }
        } else if (newChat != null) {
            right({ vm.closeNew() }, Modifier.fillMaxSize())
        } else if (selected != null) {
            right({ vm.open(null) }, Modifier.fillMaxSize())
        } else {
            column(Modifier.fillMaxSize())
        }
    }
}

/**
 * Левая колонка: бургер + поиск, табы, диалоги. Выпадашка поиска — поверх
 * табов и списка, как у десктопа. Клавиатура EMUI на разложенном Mate XT
 * занимает ~85% высоты: пока пишут в беседе справа, первая строка и табы
 * прячутся — но не когда печатают в сам поиск.
 */
@Composable
private fun MailColumn(
    s: MailState,
    syncing: Boolean,
    selectedId: String?,
    picked: Set<String>,
    vm: MailViewModel,
    needsLogin: List<MailSource>,
    accounts: AccountActions,
    onMove: () -> Unit,
    onDelete: () -> Unit,
    modifier: Modifier,
) {
    val q by vm.query.collectAsState()
    val results by vm.search.collectAsState()
    var searchFocused by remember { mutableStateOf(false) }
    val ime = imeCrowds()
    val head = !ime || searchFocused
    BackHandler(enabled = results.open) { vm.clearSearch() }
    Box(modifier) {
        Column(Modifier.fillMaxSize()) {
            if (head) SearchRow(q, onChange = { vm.query.value = it }, onSubmit = vm::submitSearch, onFocus = { searchFocused = it })
            ListPane(s, syncing, selectedId, picked, vm, onMove, onDelete, showTabs = head, Modifier.weight(1f))
            // «Нужен вход» — у каждой учётки своя несмываемая плашка (§2,
            // mobile-external-accounts §7); внизу колонки, а не над поиском.
            needsLogin.forEach { src -> DeadSessionBanner(src.title) { accounts.relogin(src.accountKey) } }
        }
        if (results.open && head) {
            SearchDropdown(results, vm, Modifier.padding(top = SEARCH_ROW_HEIGHT).zIndex(2f))
        }
    }
}

/** Высота первой строки (бургер + поиск) — под ней открывается выпадашка. */
private val SEARCH_ROW_HEIGHT = 56.dp

/**
 * Первая строка экрана: бургер и круглое поле поиска на всю оставшуюся
 * ширину (сайдбар десктопа). Enter — первая строка выпадашки: адрес —
 * новая беседа с ним.
 */
@Composable
private fun SearchRow(q: String, onChange: (String) -> Unit, onSubmit: () -> Unit, onFocus: (Boolean) -> Unit) {
    Row(
        Modifier.fillMaxWidth().height(SEARCH_ROW_HEIGHT).padding(start = 4.dp, end = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        BurgerButton()
        Spacer(Modifier.width(4.dp))
        androidx.compose.foundation.text.BasicTextField(
            value = q,
            onValueChange = onChange,
            singleLine = true,
            textStyle = MaterialTheme.typography.bodyLarge.copy(color = Color(0xFF0F1419)),
            keyboardOptions = androidx.compose.foundation.text.KeyboardOptions(
                imeAction = androidx.compose.ui.text.input.ImeAction.Go,
                autoCorrectEnabled = false,
            ),
            keyboardActions = androidx.compose.foundation.text.KeyboardActions(onGo = { onSubmit() }),
            cursorBrush = androidx.compose.ui.graphics.SolidColor(Accent),
            modifier = Modifier
                .weight(1f)
                .height(40.dp)
                .onFocusChanged { onFocus(it.isFocused) }
                .onPreviewKeyEvent { e ->
                    // Аппаратный Enter (и `adb input keyevent ENTER`) — то же, что «Перейти».
                    val enter = e.key == androidx.compose.ui.input.key.Key.Enter || e.key == androidx.compose.ui.input.key.Key.NumPadEnter
                    if (enter && e.type == androidx.compose.ui.input.key.KeyEventType.KeyUp) onSubmit()
                    enter
                },
            decorationBox = { inner ->
                Row(
                    Modifier.fillMaxSize().clip(RoundedCornerShape(20.dp)).background(Color(0xFFECEEF0)).padding(start = 16.dp, end = 4.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Box(Modifier.weight(1f)) {
                        if (q.isEmpty()) Text("Поиск или новый адресат…", color = Color(0xFF8B95A1), style = MaterialTheme.typography.bodyLarge, maxLines = 1, overflow = TextOverflow.Ellipsis)
                        inner()
                    }
                    if (q.isNotEmpty()) IconButton(onClick = { onChange("") }, modifier = Modifier.size(32.dp)) {
                        Icon(Icons.Filled.Close, "Очистить поиск", tint = Color(0xFF6B7480), modifier = Modifier.size(18.dp))
                    }
                }
            },
        )
    }
}

/**
 * Выпадашка поиска (§4 «Поиск» десктопа): «Написать по адресу …»,
 * «Диалоги», «Контакты», «Сообщения». Карточка поверх табов и списка.
 */
@Composable
private fun SearchDropdown(r: SearchResults, vm: MailViewModel, modifier: Modifier) {
    Surface(
        modifier = modifier.padding(horizontal = 10.dp).fillMaxWidth().heightIn(max = 420.dp),
        shape = RoundedCornerShape(10.dp),
        color = Color.White,
        border = androidx.compose.foundation.BorderStroke(1.dp, Color(0xFFD8DDE6)),
        shadowElevation = 8.dp,
    ) {
        LazyColumn(contentPadding = androidx.compose.foundation.layout.PaddingValues(4.dp)) {
            if (r.composeEmail.isNotEmpty()) item(key = "compose") {
                Row(
                    Modifier.fillMaxWidth().clip(RoundedCornerShape(8.dp))
                        .clickable { vm.startNew(r.composeEmail) }.padding(horizontal = 12.dp, vertical = 6.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Icon(Icons.Filled.Edit, null, tint = SelectedRow, modifier = Modifier.size(18.dp))
                    Spacer(Modifier.width(10.dp))
                    // Адрес — своей строкой: в колонке 340 dp «Написать по
                    // адресу:» съедала его целиком.
                    Column {
                        Text("Написать по адресу:", color = SelectedRow, style = MaterialTheme.typography.bodyLarge, maxLines = 1)
                        Text(r.composeEmail, color = SelectedRow, fontWeight = FontWeight.Medium, style = MaterialTheme.typography.bodyMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    }
                }
            }
            if (r.total == 0) item(key = "empty") {
                if (r.loading || r.composeEmail.isEmpty()) Text(
                    if (r.loading) "Ищу…" else "Ничего не найдено",
                    color = Color(0xFF8B95A1),
                    style = MaterialTheme.typography.bodyMedium,
                    textAlign = androidx.compose.ui.text.style.TextAlign.Center,
                    modifier = Modifier.fillMaxWidth().padding(vertical = 10.dp),
                )
            }
            if (r.convs.isNotEmpty()) {
                item(key = "h-convs") { DropdownHeader("ДИАЛОГИ") }
                items(r.convs, key = { "c" + it.conv.ref }) { h ->
                    DropdownRow(h.conv.label.ifBlank { h.conv.counterparts.firstOrNull()?.addr.orEmpty() }, h.detail) { vm.pickSearchConv(h.conv.ref) }
                }
            }
            if (r.contacts.isNotEmpty()) {
                item(key = "h-contacts") { DropdownHeader("КОНТАКТЫ") }
                items(r.contacts, key = { "p" + it.email }) { c ->
                    DropdownRow(c.name.ifEmpty { c.email }, if (c.name.isEmpty()) "" else c.email) { vm.pickSearchContact(c.email) }
                }
            }
            if (r.messages.isNotEmpty()) {
                item(key = "h-msgs") { DropdownHeader("СООБЩЕНИЯ") }
                items(r.messages, key = { "m" + it.ref + it.dateTs + it.subject }) { m ->
                    DropdownRow(m.from, m.subject.ifBlank { stringResource(R.string.no_subject) }, trailing = formatListDate(m.dateTs)) { vm.pickSearchConv(m.ref) }
                }
            }
        }
    }
}

@Composable
private fun DropdownHeader(title: String) {
    Text(
        title,
        color = Color(0xFF8B95A1),
        fontSize = 11.sp,
        fontWeight = FontWeight.Bold,
        letterSpacing = 0.5.sp,
        modifier = Modifier.padding(start = 12.dp, top = 8.dp, bottom = 2.dp),
    )
}

@Composable
private fun DropdownRow(title: String, detail: String, trailing: String = "", onClick: () -> Unit) {
    Column(
        Modifier.fillMaxWidth().clip(RoundedCornerShape(8.dp)).clickable(onClick = onClick).padding(horizontal = 12.dp, vertical = 6.dp),
    ) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(title, color = Color(0xFF0F1419), fontWeight = FontWeight.Medium, style = MaterialTheme.typography.bodyLarge, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f))
            if (trailing.isNotEmpty()) Text(trailing, color = Color(0xFF8B95A1), style = MaterialTheme.typography.bodySmall, modifier = Modifier.padding(start = 8.dp))
        }
        if (detail.isNotEmpty()) Text(detail, color = Color(0xFF8B95A1), style = MaterialTheme.typography.bodySmall, maxLines = 1, overflow = TextOverflow.Ellipsis)
    }
}

/**
 * Новая беседа (compose-режим десктопа): шапка с адресатом, пустая лента —
 * в ней только ушедшие отсюда письма, композер внизу. Когда беседа придёт с
 * сервера, она откроется вместо заготовки.
 */
@Composable
private fun NewChatPane(to: String, vm: MailViewModel, onBack: (() -> Unit)?, modifier: Modifier) {
    val ime = imeCrowds()
    val u by vm.compose.ui.collectAsState()
    Column(modifier) {
        if (!ime) Row(Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 6.dp), verticalAlignment = Alignment.CenterVertically) {
            if (onBack != null) IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "К списку") }
            Box(Modifier.padding(start = if (onBack == null) 8.dp else 0.dp).size(40.dp).clip(CircleShape).background(SelectedRow), contentAlignment = Alignment.Center) {
                Text(to.take(1).uppercase(), color = Color.White, fontWeight = FontWeight.SemiBold)
            }
            Spacer(Modifier.width(12.dp))
            Column(Modifier.weight(1f)) {
                Text(to, style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                Text("Новая беседа", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
        }
        HorizontalDivider()
        LazyColumn(
            Modifier.fillMaxWidth().weight(1f).padding(horizontal = 12.dp),
            verticalArrangement = Arrangement.spacedBy(6.dp, Alignment.Bottom),
            contentPadding = androidx.compose.foundation.layout.PaddingValues(vertical = 8.dp),
        ) {
            items(u.pendingNew, key = { it.key }) { p ->
                Box(Modifier.fillMaxWidth(), contentAlignment = Alignment.CenterEnd) {
                    Column(
                        Modifier.widthIn(max = 480.dp).clip(RoundedCornerShape(14.dp)).background(Color(0xFFDCF8C6)).padding(horizontal = 12.dp, vertical = 8.dp),
                        horizontalAlignment = Alignment.End,
                    ) {
                        Text(p.text)
                        // Как заглушка в ленте: «Отправляется…», потом «Отправлено» — пока
                        // беседа не придёт с сервера и не откроется вместо заготовки.
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            if (p.sent) Icon(Icons.Filled.Check, null, tint = Accent, modifier = Modifier.size(14.dp))
                            Text(
                                if (p.sent) " Отправлено" else "Отправляется…",
                                style = MaterialTheme.typography.bodySmall,
                                color = Color(0xFF6B7480),
                            )
                        }
                    }
                }
            }
        }
        HorizontalDivider()
        ComposerBar(vm.compose, conversation = null, newTo = to, focusOnStart = true)
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
    showTabs: Boolean,
    modifier: Modifier,
) {
    Column(modifier) {
        // На время выбора панель действий заменяет полосу табов (§5).
        if (picked.isEmpty()) { if (showTabs) FolderTabs(s, vm) }
        else if (s.inSpam) SpamSelectionBar(picked.size, vm)
        else SelectionBar(picked.size, vm, onMove, onDelete)
        HorizontalDivider()
        // Крутится только по жесту: фоновый синк индикатора не имеет.
        val pulling by vm.pulling.collectAsState()
        val identColors by vm.identityColors.collectAsState()
        PullToRefreshBox(isRefreshing = pulling, onRefresh = vm::pullRefresh, modifier = Modifier.fillMaxSize()) {
            if (s.loaded && s.conversations.isEmpty() && !syncing) {
                Text(
                    when {
                        s.inSpam -> "Спама нет."
                        else -> stringResource(if (s.active == null) R.string.list_empty_inbox else R.string.list_empty_folder)
                    },
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(24.dp),
                )
            }
            // Новая беседа (письмо новому адресату, свежее входящее) встаёт
            // первой строкой. LazyColumn держит позицию по ключу прежней первой
            // строки, и новая оказывалась выше края экрана — «после отправки
            // диалог не появился» (замер 2026-11-13). Был в самом верху — верх и
            // остаётся; прокрутил вниз — позиция не трогается.
            val listState = androidx.compose.foundation.lazy.rememberLazyListState()
            val topKey = s.conversations.firstOrNull()?.let { it.accountKey + "\u0000" + it.id }
            var prevTop by remember { mutableStateOf<String?>(null) }
            LaunchedEffect(topKey) {
                val was = prevTop
                prevTop = topKey
                if (was == null || was == topKey) return@LaunchedEffect
                val anchored = listState.layoutInfo.visibleItemsInfo.firstOrNull()?.key == was
                if (anchored && listState.firstVisibleItemScrollOffset == 0) listState.scrollToItem(0)
            }
            LazyColumn(Modifier.fillMaxSize(), state = listState) {
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
        }
    }
}

/**
 * Табы папок, как в Telegram (§4 десктопа): текст, выбранный — акцентом с
 * подчёркиванием; таба «Все» нет — ничего не выбрано значит «Входящие». Пока
 * выбрана папка, первым стоит иконка «Входящих» со счётчиком — путь назад.
 *
 * Долгое нажатие — меню полосы, как правый клик десктопа: на табе — «Все
 * прочитаны», «Настроить…», «Удалить» (у «Спама» — только первое); везде —
 * «Новая папка» и «Показать скрытые (N)» / «Не показывать скрытые».
 */
@Composable
private fun FolderTabs(s: MailState, vm: MailViewModel) {
    if (s.tabs.isEmpty()) return
    // Меню открыто: на табе (`tab != null`) или на полосе.
    var menuFor by remember { mutableStateOf<FolderTab?>(null) }
    var stripMenu by remember { mutableStateOf(false) }
    var deleteAsk by remember { mutableStateOf<FolderTab?>(null) }
    deleteAsk?.let { t ->
        AlertDialog(
            onDismissRequest = { deleteAsk = null },
            title = { Text("Удалить папку «${t.name.ifBlank { "без названия" }}»?") },
            text = { Text("На всех устройствах. Диалоги вернутся во «Входящие», положенные в папку руками — тоже.") },
            confirmButton = {
                TextButton(onClick = {
                    deleteAsk = null
                    vm.deleteFolder(t.id)
                }) { Text("Удалить", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = { IconButton(onClick = { deleteAsk = null }) { Icon(Icons.Filled.Close, "Отмена") } },
        )
    }
    val menuItems = @Composable { tab: FolderTab?, close: () -> Unit ->
        FolderMenuItems(tab, s, vm, close) { deleteAsk = it }
    }
    // Долгое нажатие мимо табов — меню полосы. Таб своё долгое нажатие ловит
    // сам, но полоса-родитель видит тот же жест: решаем чуть позже — если
    // открылось меню таба, меню полосы не нужно.
    var stripAsk by remember { mutableStateOf(0) }
    LaunchedEffect(stripAsk) {
        if (stripAsk == 0) return@LaunchedEffect
        kotlinx.coroutines.delay(120)
        if (menuFor == null) stripMenu = true
    }
    Box(
        Modifier.fillMaxWidth().pointerInput(Unit) {
            detectTapGestures(onLongPress = { stripAsk++ })
        },
    ) {
        LazyRow(
            Modifier.fillMaxWidth().padding(horizontal = 8.dp),
            horizontalArrangement = Arrangement.spacedBy(4.dp),
        ) {
            if (s.active != null) {
                item(key = "inbox") {
                    Tab(
                        "", s.inboxUnread, selected = false, icon = Icons.Filled.Inbox,
                        description = stringResource(R.string.tab_inbox_back),
                        onLongClick = { stripAsk++ },
                    ) { vm.selectFolder(null) }
                }
            }
            items(s.tabs, key = { it.id }) { t ->
                val selected = t.index == s.active
                val title = if (t.icon.isBlank()) t.name else "${t.icon} ${t.name}"
                Box {
                    // Повторное касание выбранного таба — назад во «Входящие».
                    Tab(title, t.unread, selected, dim = t.hidden, onLongClick = { menuFor = t }) { vm.selectFolder(if (selected) null else t.id) }
                    DropdownMenu(expanded = menuFor?.id == t.id, onDismissRequest = { menuFor = null }) {
                        menuItems(t) { menuFor = null }
                    }
                }
            }
        }
        DropdownMenu(expanded = stripMenu, onDismissRequest = { stripMenu = false }) {
            menuItems(null) { stripMenu = false }
        }
    }
}

/** Пункты меню полосы табов; [tab] — таб под пальцем (`null` — полоса). */
@Composable
private fun FolderMenuItems(tab: FolderTab?, s: MailState, vm: MailViewModel, close: () -> Unit, askDelete: (FolderTab) -> Unit) {
    if (tab != null) {
        DropdownMenuItem(
            text = { Text("Все прочитаны") },
            leadingIcon = { Icon(Icons.Filled.DoneAll, null) },
            onClick = {
                close()
                vm.markFolderRead(tab.id)
            },
        )
        if (!tab.system) {
            DropdownMenuItem(
                text = { Text("Настроить…") },
                leadingIcon = { Icon(Icons.Filled.Tune, null) },
                onClick = {
                    close()
                    vm.folderEditor.value = tab.id
                },
            )
            DropdownMenuItem(
                text = { Text("Удалить") },
                leadingIcon = { Icon(Icons.Filled.Delete, null, tint = MaterialTheme.colorScheme.error) },
                onClick = {
                    close()
                    askDelete(tab)
                },
            )
        }
    }
    DropdownMenuItem(
        text = { Text("Новая папка") },
        leadingIcon = { Icon(Icons.Filled.CreateNewFolder, null) },
        onClick = {
            close()
            // И сразу в редактор — назвать её (как десктоп открывает настройки).
            vm.folderEditor.value = vm.newFolder()
        },
    )
    if (s.hiddenCount > 0) {
        DropdownMenuItem(
            text = { Text(if (s.showHidden) "Не показывать скрытые" else "Показать скрытые (${s.hiddenCount})") },
            leadingIcon = { Icon(if (s.showHidden) Icons.Filled.VisibilityOff else Icons.Filled.Visibility, null) },
            onClick = {
                close()
                vm.toggleShowHidden()
            },
        )
    }
}

@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun Tab(
    title: String,
    unread: Int,
    selected: Boolean,
    icon: androidx.compose.ui.graphics.vector.ImageVector? = null,
    description: String? = null,
    /** Скрытая папка, показанная по «Показать скрытые», — бледнее. */
    dim: Boolean = false,
    onLongClick: (() -> Unit)? = null,
    onClick: () -> Unit,
) {
    Column(
        Modifier.clip(RoundedCornerShape(6.dp)).combinedClickable(onClick = onClick, onLongClick = onLongClick)
            .padding(horizontal = 10.dp, vertical = 8.dp)
            .alpha(if (dim) 0.5f else 1f),
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
