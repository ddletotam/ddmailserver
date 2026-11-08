package ru.letotam.ddmail.mail

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
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
import androidx.lifecycle.compose.LifecycleResumeEffect
import ru.letotam.ddmail.DdmailApp
import ru.letotam.ddmail.R
import ru.letotam.ddmail.compose.ComposeSheetDialog
import ru.letotam.ddmail.compose.ComposerBar
import ru.letotam.ddmail.conversation.ConversationWeb
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.isImeVisible
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ExtendedFloatingActionButton
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.text.AnnotatedString
import ru.letotam.ddmail.SessionStatus
import ru.letotam.ddmail.auth.Session
import ru.letotam.ddmail.sync.Conversation

private val Accent = Color(0xFF047857)

/**
 * Почта (контракт §5–6). Раскладка — по ширине окна: Compact (Mate XT
 * сложен, 350 dp) — список, беседа поверх; шире (712 / 1107 dp) — список и
 * беседа рядом. Табы папок наверху во всех состояниях.
 */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun MailScreen(
    vm: MailViewModel,
    session: Session,
    status: SessionStatus,
    onRelogin: () -> Unit,
    onLogout: () -> Unit,
    modifier: Modifier = Modifier,
) {
    val s by vm.state.collectAsState()
    val syncing by vm.syncing.collectAsState()
    val selectedId by vm.selected.collectAsState()
    val width = LocalConfiguration.current.screenWidthDp
    val wide = width >= 600
    // Открытая беседа ищется по id в полном видимом списке: дельта могла
    // переставить строки.
    val selected = s.conversations.firstOrNull { it.id == selectedId }

    // Вернулись в приложение — подтянуть новое (до push это единственный путь).
    LifecycleResumeEffect(Unit) {
        vm.sync()
        onPauseOrDispose {}
    }
    BackHandler(enabled = !wide && selectedId != null) { vm.open(null) }
    LaunchedEffect(wide, s.conversations.size) { if (wide) vm.restoreLast() }

    // Один WebView на всё время экрана: создаётся сразу, открытие беседы
    // меняет только документ (контракт §8).
    val context = LocalContext.current
    val web = remember {
        val app = context.applicationContext as DdmailApp
        ConversationWeb(context, ConversationWeb.proxyClient(app.http))
    }
    DisposableEffect(web) {
        web.host = object : ConversationWeb.Host {
            override fun onReady() {}
            override fun onRequestOlder() = vm.conversation.older()
            override fun onOpenAttachment(uid: Long, index: Int) =
                vm.conversation.openAttachment(uid, index, vm.conversation.attachmentName(uid, index))
            override fun loadPart(uid: Long, cid: String) = vm.conversation.part(uid, cid)
            override fun onBubbleMenu(uid: Long) = vm.showBubbleMenu(uid)
        }
        onDispose { web.host = null }
    }
    LaunchedEffect(web) {
        for (js in vm.conversation.js) web.eval(js)
    }

    ComposeSheetDialog(vm.compose)
    BubbleMenu(vm)

    // Клавиатура EMUI на разложенном Mate XT занимает ~85% высоты: пока она
    // открыта, прячем всё, кроме строки ввода, иначе композера не видно.
    val ime = WindowInsets.isImeVisible
    Column(modifier.fillMaxSize().imePadding()) {
        if (!ime) {
            TopBar(session, status, syncing, onLogout)
            if (status == SessionStatus.Dead) DeadSessionBanner(onRelogin)
        }
        if (wide) {
            Row(Modifier.fillMaxSize()) {
                ListPane(s, syncing, selectedId, vm, Modifier.width(if (width >= 840) 400.dp else 340.dp).fillMaxHeight())
                VerticalDivider()
                ConversationPane(selected, web, vm, onBack = null, Modifier.weight(1f).fillMaxHeight())
            }
        } else if (selected != null) {
            ConversationPane(selected, web, vm, onBack = { vm.open(null) }, Modifier.fillMaxSize())
        } else {
            ListPane(s, syncing, selectedId, vm, Modifier.fillMaxSize())
        }
    }
}

@Composable
private fun TopBar(session: Session, status: SessionStatus, syncing: Boolean, onLogout: () -> Unit) {
    var menu by remember { mutableStateOf(false) }
    Row(
        Modifier.fillMaxWidth().padding(start = 16.dp, end = 4.dp, top = 6.dp, bottom = 2.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(Modifier.weight(1f)) {
            Text(stringResource(R.string.mail_title), style = MaterialTheme.typography.titleLarge)
            val sub = when {
                status is SessionStatus.Offline -> stringResource(R.string.status_offline)
                syncing -> stringResource(R.string.status_syncing)
                else -> "${session.username} · ${session.serverUrl.removePrefix("https://")}"
            }
            Text(
                sub,
                style = MaterialTheme.typography.bodySmall,
                color = if (status is SessionStatus.Offline) MaterialTheme.colorScheme.error
                else MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
        }
        Box {
            TextButton(onClick = { menu = true }) { Text("⋮", fontSize = 22.sp) }
            DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
                DropdownMenuItem(text = { Text(stringResource(R.string.action_logout)) }, onClick = {
                    menu = false
                    onLogout()
                })
            }
        }
    }
}

/** Несмываемая плашка (контракт §2): кэш виден, но почта не обновляется. */
@Composable
private fun DeadSessionBanner(onRelogin: () -> Unit) {
    Surface(color = MaterialTheme.colorScheme.errorContainer, modifier = Modifier.fillMaxWidth()) {
        Row(Modifier.padding(horizontal = 16.dp, vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
            Text(stringResource(R.string.session_dead_banner), Modifier.weight(1f), style = MaterialTheme.typography.bodyMedium)
            Button(onClick = onRelogin) { Text(stringResource(R.string.action_relogin)) }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ListPane(s: MailState, syncing: Boolean, selectedId: String?, vm: MailViewModel, modifier: Modifier) {
    Column(modifier) {
        FolderTabs(s, vm::selectFolder)
        HorizontalDivider()
        PullToRefreshBox(isRefreshing = syncing, onRefresh = vm::sync, modifier = Modifier.fillMaxSize()) {
            if (s.loaded && s.conversations.isEmpty() && !syncing) {
                Text(
                    stringResource(if (s.active == null) R.string.list_empty_inbox else R.string.list_empty_folder),
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(24.dp),
                )
            }
            LazyColumn(Modifier.fillMaxSize()) {
                items(s.conversations, key = { it.accountKey + "\u0000" + it.id }) { c ->
                    ConversationRow(c, selected = c.id == selectedId, onClick = { vm.open(c.id) })
                }
            }
            ExtendedFloatingActionButton(
                onClick = vm.compose::openNew,
                containerColor = Accent,
                contentColor = Color.White,
                modifier = Modifier.align(Alignment.BottomEnd).padding(16.dp),
            ) { Text("✎  Написать") }
        }
    }
}

/**
 * Табы папок, как в Telegram (§4 десктопа): текст, выбранный — акцентом с
 * подчёркиванием; таба «Все» нет — ничего не выбрано значит «Входящие». Пока
 * выбрана папка, первым стоит «← Входящие» со счётчиком — путь назад.
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
                Tab(stringResource(R.string.tab_inbox_back), s.inboxUnread, selected = false) { onSelect(null) }
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
private fun Tab(title: String, unread: Int, selected: Boolean, onClick: () -> Unit) {
    Column(
        Modifier.clip(RoundedCornerShape(6.dp)).clickable(onClick = onClick).padding(horizontal = 10.dp, vertical = 8.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(
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

/** Строгий двухстрочный формат (§4 десктопа): имя / тема последнего письма. */
@Composable
private fun ConversationRow(c: Conversation, selected: Boolean, onClick: () -> Unit) {
    val unread = c.unreadCount > 0
    Row(
        Modifier
            .fillMaxWidth()
            .background(if (selected) MaterialTheme.colorScheme.primaryContainer else Color.Transparent)
            .clickable(onClick = onClick)
            .padding(horizontal = 14.dp, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Avatar(c)
        Spacer(Modifier.width(12.dp))
        Column(Modifier.weight(1f)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    c.label.ifBlank { c.counterparts.firstOrNull()?.addr.orEmpty() },
                    fontWeight = if (unread) FontWeight.Bold else FontWeight.SemiBold,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f),
                )
                Spacer(Modifier.width(8.dp))
                Text(
                    formatListDate(c.lastDateTs),
                    style = MaterialTheme.typography.bodySmall,
                    color = if (unread) Accent else MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    c.lastSubject.ifBlank { stringResource(R.string.no_subject) },
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f),
                )
                if (unread) {
                    Spacer(Modifier.width(8.dp))
                    Text(
                        c.unreadCount.toString(),
                        color = Color.White,
                        fontSize = 11.sp,
                        modifier = Modifier.clip(RoundedCornerShape(9.dp)).background(Accent)
                            .padding(horizontal = 7.dp, vertical = 1.dp),
                    )
                }
            }
        }
    }
}

/** Пока без серверных аватарок — инициалы на цвете, стабильном для диалога. */
@Composable
private fun Avatar(c: Conversation) {
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
    val ime = WindowInsets.isImeVisible
    Column(modifier) {
        if (!ime) Row(Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 6.dp), verticalAlignment = Alignment.CenterVertically) {
            if (onBack != null) TextButton(onClick = onBack) { Text("←", fontSize = 20.sp) }
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
                listOf(
                    "Ответить" to { vm.compose.replyTo(body) },
                    "Переслать" to { vm.compose.openForward(body) },
                    "Копировать текст" to {
                        val plain = body.text?.takeIf { it.isNotBlank() }
                            ?: android.text.Html.fromHtml(body.html.orEmpty(), android.text.Html.FROM_HTML_MODE_COMPACT).toString()
                        clipboard.setText(AnnotatedString(plain.trim()))
                    },
                ).forEach { (title, action) ->
                    Text(
                        title,
                        modifier = Modifier.fillMaxWidth().clickable {
                            action()
                            close()
                        }.padding(vertical = 14.dp),
                        style = MaterialTheme.typography.bodyLarge,
                    )
                }
            }
        },
        confirmButton = { TextButton(onClick = close) { Text("Отмена") } },
    )
}
