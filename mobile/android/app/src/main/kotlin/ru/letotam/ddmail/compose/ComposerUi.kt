package ru.letotam.ddmail.compose

import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.systemBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TextField
import androidx.compose.material3.TextFieldDefaults
import androidx.compose.runtime.Composable
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
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.*
import androidx.compose.material.icons.filled.*
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import ru.letotam.ddmail.api.Identity
import ru.letotam.ddmail.sync.Conversation

private val Accent = Color(0xFF047857)

/**
 * Композер под лентой беседы (и новой беседы — [newTo] вместо [conversation]). Enter — перенос строки, отправка — кнопкой
 * (§9). Переопределения «Кому»/«Копия»/«Тема»/«От» — под «⌄»: пусто значит
 * «как выведет ветка» (§1 десктопа), подсказка показывает, что выйдет.
 */
@Composable
fun ComposerBar(
    cc: ComposeController,
    conversation: Conversation?,
    newTo: String? = null,
    /** Фокус в поле ввода (новая беседа — сразу печатать, как `focus_composer` десктопа). */
    focus: androidx.compose.ui.focus.FocusRequester? = null,
    modifier: Modifier = Modifier,
) {
    val u by cc.ui.collectAsState()
    val ids by cc.identities.collectAsState()
    val d = u.draft
    val pick = rememberLauncherForActivityResult(ActivityResultContracts.OpenMultipleDocuments()) { uris ->
        if (uris.isNotEmpty()) cc.attach(uris, toSheet = false)
    }
    Surface(color = MaterialTheme.colorScheme.surface, modifier = modifier.fillMaxWidth()) {
        Column(Modifier.padding(horizontal = 8.dp, vertical = 6.dp)) {
            u.replyTo?.let { r ->
                Row(
                    Modifier.fillMaxWidth().clip(RoundedCornerShape(8.dp)).background(Color(0xFFE9EDF1)).padding(start = 10.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Icon(Icons.AutoMirrored.Filled.Reply, null, tint = Accent, modifier = Modifier.size(18.dp))
                    Text(
                        " ${r.from.ifBlank { r.fromAddr }}: ${r.subject}",
                        style = MaterialTheme.typography.bodySmall,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                        modifier = Modifier.weight(1f),
                    )
                    IconButton(onClick = { cc.replyTo(null) }) { Icon(Icons.Filled.Close, "Не отвечать на это письмо") }
                }
                Spacer(Modifier.padding(2.dp))
            }
            if (u.expanded) {
                val autoTo = conversation?.counterparts?.joinToString(", ") { it.addr } ?: newTo.orEmpty()
                Overrides(d, ids, conversation?.receivedBy, autoTo, subjectHint = if (conversation != null) "Re: …" else "", onEdit = cc::edit)
            }
            Attachments(d.attachments) { a -> cc.edit { it.copy(attachments = it.attachments - a) } }
            u.error?.let {
                Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall, modifier = Modifier.padding(4.dp))
            }
            if (u.sentFlash) {
                Icon(Icons.Filled.DoneAll, "Отправлено", tint = Accent, modifier = Modifier.padding(4.dp).size(20.dp))
            }
            Row(verticalAlignment = Alignment.Bottom) {
                IconButton(onClick = { pick.launch(arrayOf("*/*")) }) { Icon(Icons.Filled.AttachFile, "Вложить файл") }
                // Поля «Кому / Копия / Тема / От»: @ — про адреса.
                IconButton(onClick = cc::toggleExpanded) {
                    Icon(
                        Icons.Filled.AlternateEmail,
                        if (u.expanded) "Скрыть адресатов" else "Адресаты и тема",
                        tint = if (u.expanded) Accent else MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                TextField(
                    value = d.text,
                    onValueChange = { t -> cc.edit { it.copy(text = t) } },
                    placeholder = { Text("Сообщение") },
                    maxLines = 6,
                    keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.Sentences),
                    colors = TextFieldDefaults.colors(
                        focusedContainerColor = Color(0xFFF0F2F5),
                        unfocusedContainerColor = Color(0xFFF0F2F5),
                        focusedIndicatorColor = Color.Transparent,
                        unfocusedIndicatorColor = Color.Transparent,
                    ),
                    shape = RoundedCornerShape(18.dp),
                    modifier = Modifier.weight(1f).heightIn(min = 48.dp).then(if (focus != null) Modifier.focusRequester(focus) else Modifier),
                )
                Spacer(Modifier.padding(2.dp))
                androidx.compose.material3.FilledIconButton(
                    onClick = { if (conversation != null) cc.sendReply(conversation) else newTo?.let(cc::sendNew) },
                    enabled = d.canSend,
                    colors = androidx.compose.material3.IconButtonDefaults.filledIconButtonColors(containerColor = Accent),
                    modifier = Modifier.size(48.dp),
                ) { Icon(Icons.AutoMirrored.Filled.Send, "Отправить") }
            }
        }
    }
}

@Composable
private fun Overrides(d: Draft, ids: List<Identity>, receivedBy: String?, autoTo: String, subjectHint: String, onEdit: ((Draft) -> Draft) -> Unit) {
    Column(verticalArrangement = Arrangement.spacedBy(4.dp), modifier = Modifier.padding(bottom = 4.dp)) {
        FromPicker(d, ids, receivedBy) { email -> onEdit { it.copy(from = email) } }
        OutlinedTextField(
            value = d.to, onValueChange = { v -> onEdit { it.copy(to = v) } },
            label = { Text("Кому") }, placeholder = { Text(autoTo) }, singleLine = true,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Email),
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = d.cc, onValueChange = { v -> onEdit { it.copy(cc = v) } },
            label = { Text("Копия") }, singleLine = true,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Email),
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = d.subject, onValueChange = { v -> onEdit { it.copy(subject = v) } },
            label = { Text("Тема") }, placeholder = { if (subjectHint.isNotEmpty()) Text(subjectHint) }, singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
    }
}

@Composable
fun FromPicker(d: Draft, ids: List<Identity>, receivedBy: String?, onPick: (String) -> Unit) {
    var open by remember { mutableStateOf(false) }
    val cur = pickFrom(d, ids, receivedBy)
    Box {
        Row(
            Modifier.fillMaxWidth().clip(RoundedCornerShape(6.dp)).clickable { open = true }.padding(horizontal = 4.dp, vertical = 8.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Icon(Icons.Filled.AccountCircle, "От", tint = MaterialTheme.colorScheme.onSurfaceVariant)
            Spacer(Modifier.width(8.dp))
            Text(cur?.email ?: "—", fontWeight = FontWeight.SemiBold, modifier = Modifier.weight(1f))
            Icon(Icons.Filled.ArrowDropDown, null)
        }
        DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
            ids.forEach { id ->
                DropdownMenuItem(
                    text = { Text(if (id.name.isBlank()) id.email else "${id.name} · ${id.email}") },
                    onClick = {
                        open = false
                        onPick(id.email)
                    },
                )
            }
        }
    }
}

@Composable
fun Attachments(list: List<PendingAttachment>, onRemove: (PendingAttachment) -> Unit) {
    if (list.isEmpty()) return
    LazyRow(horizontalArrangement = Arrangement.spacedBy(6.dp), modifier = Modifier.padding(vertical = 4.dp)) {
        items(list) { a ->
            Row(
                Modifier.clip(RoundedCornerShape(10.dp)).background(Color(0xFFE9EDF1)).padding(start = 10.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Icon(Icons.Filled.AttachFile, null, modifier = Modifier.size(16.dp))
                Text(a.name, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.widthIn(max = 200.dp), style = MaterialTheme.typography.bodySmall)
                Text(" ${formatSize(a.size)}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                IconButton(onClick = { onRemove(a) }) { Icon(Icons.Filled.Close, "Убрать вложение", modifier = Modifier.size(18.dp)) }
            }
        }
    }
}

fun formatSize(n: Int): String = when {
    n >= 1 shl 20 -> String.format("%.1f МБ", n / 1048576.0)
    n >= 1 shl 10 -> "${n / 1024} КБ"
    else -> "$n Б"
}

/** Окно нового письма и пересылки — во весь экран на любой раскладке. */
@Composable
fun ComposeSheetDialog(cc: ComposeController) {
    val u by cc.ui.collectAsState()
    val sheet = u.sheet ?: return
    val ids by cc.identities.collectAsState()
    val d = sheet.draft
    val pick = rememberLauncherForActivityResult(ActivityResultContracts.OpenMultipleDocuments()) { uris ->
        if (uris.isNotEmpty()) cc.attach(uris, toSheet = true)
    }
    // Окно само отводит место под клавиатуру и системные панели: иначе оно
    // сдвигалось целиком, и шапка с «Отправить» уезжала за верх экрана
    // (на разложенном Mate XT клавиатура EMUI занимает ~80% высоты).
    Dialog(
        onDismissRequest = { if (!sheet.busy) cc.closeSheet() },
        properties = DialogProperties(usePlatformDefaultWidth = false, decorFitsSystemWindows = false),
    ) {
        Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) {
          Column(Modifier.fillMaxSize().systemBarsPadding().imePadding()) {
            Column(Modifier.fillMaxSize()) {
                Row(Modifier.fillMaxWidth().padding(8.dp), verticalAlignment = Alignment.CenterVertically) {
                    IconButton(onClick = cc::closeSheet, enabled = !sheet.busy) { Icon(Icons.Filled.Close, "Закрыть") }
                    Text(
                        if (sheet.branch is Branch.Forward) "Переслать" else "Новое письмо",
                        style = MaterialTheme.typography.titleLarge,
                        modifier = Modifier.weight(1f),
                    )
                    IconButton(onClick = { pick.launch(arrayOf("*/*")) }, enabled = !sheet.busy) { Icon(Icons.Filled.AttachFile, "Вложить файл") }
                    if (sheet.busy) {
                        androidx.compose.material3.CircularProgressIndicator(Modifier.padding(12.dp).size(24.dp), color = Accent, strokeWidth = 2.dp)
                    } else {
                        androidx.compose.material3.FilledIconButton(
                            onClick = cc::sendSheet,
                            colors = androidx.compose.material3.IconButtonDefaults.filledIconButtonColors(containerColor = Accent),
                        ) { Icon(Icons.AutoMirrored.Filled.Send, "Отправить") }
                    }
                }
                HorizontalDivider()
                Column(
                    Modifier.fillMaxWidth().widthIn(max = 720.dp).verticalScroll(rememberScrollState()).padding(16.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    FromPicker(d, ids, null) { email -> cc.editSheet { it.copy(from = email) } }
                    OutlinedTextField(
                        value = d.to, onValueChange = { v -> cc.editSheet { it.copy(to = v) } },
                        label = { Text("Кому") }, singleLine = true,
                        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Email, imeAction = ImeAction.Next),
                        modifier = Modifier.fillMaxWidth(),
                    )
                    OutlinedTextField(
                        value = d.cc, onValueChange = { v -> cc.editSheet { it.copy(cc = v) } },
                        label = { Text("Копия") }, singleLine = true,
                        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Email, imeAction = ImeAction.Next),
                        modifier = Modifier.fillMaxWidth(),
                    )
                    val fwd = (sheet.branch as? Branch.Forward)?.original
                    OutlinedTextField(
                        value = d.subject, onValueChange = { v -> cc.editSheet { it.copy(subject = v) } },
                        label = { Text("Тема") }, singleLine = true,
                        placeholder = { fwd?.let { Text("Fwd: ${it.subject}") } },
                        keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.Sentences, imeAction = ImeAction.Next),
                        modifier = Modifier.fillMaxWidth(),
                    )
                    OutlinedTextField(
                        value = d.text, onValueChange = { v -> cc.editSheet { it.copy(text = v) } },
                        label = { Text("Сообщение") },
                        keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.Sentences),
                        modifier = Modifier.fillMaxWidth().heightIn(min = 160.dp),
                    )
                    Attachments(d.attachments) { a -> cc.editSheet { it.copy(attachments = it.attachments - a) } }
                    fwd?.let { o ->
                        Text(
                            "Пересылается: «${o.subject}» от ${o.from.ifBlank { o.fromAddr }}" +
                                if (o.attachments.isNotEmpty()) " · вложений: ${o.attachments.size}" else "",
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                    sheet.error?.let { Text(it, color = MaterialTheme.colorScheme.error) }
                }
            }
          }
        }
    }
}
