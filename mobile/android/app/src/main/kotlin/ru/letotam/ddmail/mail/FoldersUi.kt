package ru.letotam.ddmail.mail

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.systemBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ArrowDownward
import androidx.compose.material.icons.filled.ArrowUpward
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.CreateNewFolder
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import ru.letotam.ddmail.sync.DialogFolder
import ru.letotam.ddmail.sync.DialogKind
import ru.letotam.ddmail.sync.FoldersDoc
import ru.letotam.ddmail.sync.SYSTEM_SPAM
import ru.letotam.ddmail.sync.splitRuleList

private val Accent = Color(0xFF047857)

/**
 * Редактор папки — то же, что строка раздела «Папки» настроек десктопа:
 * иконка и название, правила «Получено на» / «Домены» / «Диалоги», «Не
 * показывать в списке», «Письма сразу прочитаны», сброс положенных руками.
 * Правка уходит одной записью документа по «Сохранить» (у десктопа —
 * дебаунсом по полям; на телефоне запись на каждую букву не нужна).
 *
 * Открывается из меню полосы табов («Настроить…», «Новая папка») и из
 * настроек; рисуется оболочкой поверх всего.
 */
@Composable
fun FolderEditorDialog(vm: MailViewModel) {
    val id by vm.folderEditor.collectAsState()
    val doc by vm.folders.collectAsState()
    val fid = id ?: return
    // Новая папка появляется в документе чуть позже (запись в кэш) — ждём её.
    // Системная («Спам») не настраивается.
    val folder = doc.folders.firstOrNull { it.id == fid && !it.isSystem } ?: return
    FolderEditor(folder, onClose = { vm.folderEditor.value = null }) { edited ->
        vm.folderEditor.value = null
        vm.saveFolder(edited)
    }
}

@Composable
private fun FolderEditor(f: DialogFolder, onClose: () -> Unit, onSave: (DialogFolder) -> Unit) {
    // Поля — свои копии на время правки: документ может смениться синком.
    var icon by remember(f.id) { mutableStateOf(f.icon) }
    var name by remember(f.id) { mutableStateOf(f.name) }
    var idents by remember(f.id) { mutableStateOf(f.rules.identities.joinToString(", ")) }
    var domains by remember(f.id) { mutableStateOf(f.rules.domains.joinToString(", ")) }
    var kind by remember(f.id) { mutableStateOf(f.rules.kind) }
    var hidden by remember(f.id) { mutableStateOf(f.hidden) }
    var autoRead by remember(f.id) { mutableStateOf(f.autoRead) }
    var clearManual by remember(f.id) { mutableStateOf(false) }
    val save = {
        onSave(
            f.copy(
                icon = clipIcon(icon.trim()),
                name = name.trim(),
                rules = f.rules.copy(identities = splitRuleList(idents), domains = splitRuleList(domains), kind = kind),
                hidden = hidden,
                autoRead = autoRead,
                include = if (clearManual) emptyList() else f.include,
                exclude = if (clearManual) emptyList() else f.exclude,
            ),
        )
    }
    Dialog(onDismissRequest = onClose, properties = DialogProperties(usePlatformDefaultWidth = false, decorFitsSystemWindows = false)) {
        Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) {
            Column(Modifier.fillMaxSize().systemBarsPadding().imePadding()) {
                Row(Modifier.fillMaxWidth().padding(8.dp), verticalAlignment = Alignment.CenterVertically) {
                    IconButton(onClick = onClose) { Icon(Icons.Filled.Close, "Отмена") }
                    Text("Папка", style = MaterialTheme.typography.titleLarge, modifier = Modifier.weight(1f))
                    IconButton(onClick = save) { Icon(Icons.Filled.Check, "Сохранить", tint = Accent) }
                }
                HorizontalDivider()
                Column(
                    Modifier.fillMaxWidth().widthIn(max = 640.dp).verticalScroll(rememberScrollState()).padding(16.dp),
                    verticalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        OutlinedTextField(
                            value = icon, onValueChange = { icon = clipIcon(it) },
                            placeholder = { Text("📁") }, singleLine = true,
                            modifier = Modifier.width(72.dp),
                        )
                        OutlinedTextField(
                            value = name, onValueChange = { name = it },
                            label = { Text("Название") }, singleLine = true,
                            keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.Sentences),
                            modifier = Modifier.weight(1f),
                        )
                    }
                    OutlinedTextField(
                        value = idents, onValueChange = { idents = it },
                        label = { Text("Получено на") },
                        supportingText = { Text("Свои адреса через запятую") },
                        placeholder = { Text("напр. me@acme.com") },
                        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Email),
                        modifier = Modifier.fillMaxWidth(),
                    )
                    OutlinedTextField(
                        value = domains, onValueChange = { domains = it },
                        label = { Text("Домены") },
                        supportingText = { Text("Собеседника, вместе с поддоменами") },
                        placeholder = { Text("напр. acme.com") },
                        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
                        modifier = Modifier.fillMaxWidth(),
                    )
                    Text("Диалоги", style = MaterialTheme.typography.bodyLarge)
                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        listOf(DialogKind.Any to "все", DialogKind.Direct to "только 1:1", DialogKind.Group to "только группы").forEach { (k, t) ->
                            FilterChip(selected = kind == k, onClick = { kind = k }, label = { Text(t) })
                        }
                    }
                    SwitchRow(
                        "Не показывать в списке",
                        "Таба нет, письма без уведомлений; вернуть — долгое нажатие на табы → «Показать скрытые»",
                        hidden,
                    ) { hidden = it }
                    SwitchRow(
                        "Письма сразу прочитаны",
                        "Новые письма её диалогов помечаются прочитанными, уже лежащие — нет (исполняет десктоп)",
                        autoRead,
                    ) { autoRead = it }
                    val manual = if (clearManual) 0 else f.include.size
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Column(Modifier.weight(1f)) {
                            Text("Положено руками: $manual", style = MaterialTheme.typography.bodyLarge)
                            Text(
                                "Эти диалоги лежат здесь независимо от правил",
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                            )
                        }
                        if (manual > 0 || (!clearManual && f.exclude.isNotEmpty())) TextButton(onClick = { clearManual = true }) { Text("Сбросить") }
                    }
                    Text(
                        FOLDERS_HELP,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        }
    }
}

@Composable
private fun SwitchRow(title: String, subtitle: String, checked: Boolean, onChange: (Boolean) -> Unit) {
    Row(Modifier.fillMaxWidth().clickable { onChange(!checked) }, verticalAlignment = Alignment.CenterVertically) {
        Column(Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.bodyLarge)
            Text(subtitle, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Spacer(Modifier.width(8.dp))
        Switch(checked = checked, onCheckedChange = onChange)
    }
}

/**
 * Раздел «Папки» настроек (десктоп §5г): выключатель «Показывать папки»
 * (на этом телефоне), список папок — правка, порядок, удаление, — «Новая
 * папка». Системной «Спам» в списке нет: она не настраивается.
 */
@Composable
fun FolderSettings(vm: MailViewModel) {
    val on by vm.foldersOn.collectAsState()
    val doc by vm.folders.collectAsState()
    var deleteAsk by remember { mutableStateOf<DialogFolder?>(null) }
    deleteAsk?.let { f ->
        AlertDialog(
            onDismissRequest = { deleteAsk = null },
            title = { Text("Удалить папку «${f.name.ifBlank { "без названия" }}»?") },
            text = { Text("На всех устройствах: папки общие. Диалоги вернутся во «Входящие», положенные в папку руками — тоже.") },
            confirmButton = {
                TextButton(onClick = {
                    deleteAsk = null
                    vm.deleteFolder(f.id)
                }) { Text("Удалить", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = { IconButton(onClick = { deleteAsk = null }) { Icon(Icons.Filled.Close, "Отмена") } },
        )
    }
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        SwitchRow("Показывать папки", "Выключатель на этом телефоне, сами папки общие для всех устройств", on) { vm.setFoldersOn(it) }
        val list = doc.folders.filter { !it.isSystem }
        list.forEachIndexed { i, f ->
            Row(verticalAlignment = Alignment.CenterVertically) {
                Column(Modifier.weight(1f).clickable { vm.folderEditor.value = f.id }) {
                    Text(
                        (if (f.icon.isBlank()) "" else f.icon + " ") + f.name.ifBlank { "Без названия" },
                        style = MaterialTheme.typography.bodyLarge,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                    )
                    Text(folderSummary(f), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2)
                }
                // Порядок папок — порядок табов и приоритет правил.
                IconButton(onClick = { vm.moveFolder(f.id, list[i - 1].id) }, enabled = i > 0) { Icon(Icons.Filled.ArrowUpward, "Выше") }
                IconButton(onClick = { vm.moveFolder(f.id, list[i + 1].id) }, enabled = i < list.size - 1) { Icon(Icons.Filled.ArrowDownward, "Ниже") }
                IconButton(onClick = { vm.folderEditor.value = f.id }) { Icon(Icons.Filled.Edit, "Настроить") }
                IconButton(onClick = { deleteAsk = f }) { Icon(Icons.Filled.Delete, "Удалить", tint = MaterialTheme.colorScheme.error) }
            }
        }
        OutlinedButton(onClick = { vm.folderEditor.value = vm.newFolder() }) {
            Icon(Icons.Filled.CreateNewFolder, null)
            Spacer(Modifier.width(8.dp))
            Text(if (list.isEmpty()) "Создать первую папку" else "Новая папка")
        }
        Text(FOLDERS_HELP, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

/**
 * Табы полосы папок (десктоп `refresh_folder_tabs`): скрытые — только по
 * «Показать скрытые» или когда в папку привели, и тогда бледнее. «Спам» —
 * скрытая папка, как прочие (всегда скрыта: [FoldersDoc.withSpam]), только
 * без счётчика — непрочитанный спам не повод на него смотреть. [counts] —
 * из [FoldersDoc.unreadCounts].
 */
internal fun folderTabs(doc: FoldersDoc, counts: List<Int>, active: Int?, showHidden: Boolean): List<FolderTab> =
    doc.folders.mapIndexedNotNull { i, f ->
        if (!doc.tabVisible(i, showHidden, active)) return@mapIndexedNotNull null
        val spam = f.system == SYSTEM_SPAM
        FolderTab(i, f.id, f.name, f.icon, if (spam) 0 else counts.getOrElse(i) { 0 }, hidden = f.hidden, system = f.isSystem)
    }

/** Правила папки одной строкой — под названием в списке настроек. */
internal fun folderSummary(f: DialogFolder): String {
    val parts = buildList {
        if (f.rules.identities.isNotEmpty()) add("на " + f.rules.identities.joinToString(", "))
        if (f.rules.domains.isNotEmpty()) add("домены " + f.rules.domains.joinToString(", "))
        when (f.rules.kind) {
            DialogKind.Direct -> add("только 1:1")
            DialogKind.Group -> add("только группы")
            DialogKind.Any -> {}
        }
        if (f.include.isNotEmpty()) add("руками: ${f.include.size}")
        if (f.hidden) add("скрыта")
        if (f.autoRead) add("сразу прочитаны")
    }
    return if (parts.isEmpty()) "без правил — только руками" else parts.joinToString(" · ")
}

/**
 * Иконка — до четырёх символов, эмодзи не режется посередине (`clip_icon`
 * десктопа): ZWJ-последовательности, модификаторы и флаги — один символ.
 */
internal fun clipIcon(text: String): String {
    val zwj = 0x200D
    fun joins(c: Int) = c == zwj || c == 0x20E3 || c in 0xFE00..0xFE0F || c in 0x1F3FB..0x1F3FF || c in 0xE0020..0xE007F
    fun regional(c: Int) = c in 0x1F1E6..0x1F1FF
    val out = StringBuilder()
    var units = 0
    var prevZwj = false
    var openFlag = false
    var i = 0
    while (i < text.length) {
        val c = text.codePointAt(i)
        val secondOfFlag = regional(c) && openFlag
        if (!joins(c) && !prevZwj && !secondOfFlag) {
            if (units == 4) break
            units++
        }
        out.appendCodePoint(c)
        prevZwj = c == zwj
        openFlag = regional(c) && !secondOfFlag
        i += Character.charCount(c)
    }
    return out.toString()
}

/** Пояснение раздела — как у десктопа, жесты телефона. */
private const val FOLDERS_HELP =
    "Диалог лежит ровно в одной папке: положенный руками (свайп влево по диалогу или выбор → «В папку») — там, " +
        "иначе в первой по порядку табов, чьи правила подходят. Между правилами — «и», внутри списка — «или»; " +
        "папка без правил набирается только руками. Не попавшие никуда — во «Входящих»: это когда ни один таб не выбран."
