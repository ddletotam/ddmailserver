package ru.letotam.ddmail.contacts

import android.content.Intent
import android.net.Uri
import androidx.activity.compose.BackHandler
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.Image
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
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Business
import androidx.compose.material.icons.filled.Call
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.Email
import androidx.compose.material.icons.filled.Person
import androidx.compose.material.icons.filled.Search
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.FilledIconButton
import androidx.compose.material3.FloatingActionButton
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextField
import androidx.compose.material3.TextFieldDefaults
import androidx.compose.material3.VerticalDivider
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.produceState
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.LifecycleResumeEffect
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.api.ContactDto
import ru.letotam.ddmail.api.ContactWriteRequest
import ru.letotam.ddmail.api.SessionApi
import ru.letotam.ddmail.mail.Avatars

/** Форма контакта: новый ([id] = null) или правка. Сервер хранит до трёх адресов и телефонов. */
data class ContactDraft(
    val id: Long?,
    val name: String = "",
    val org: String = "",
    val title: String = "",
    val emails: List<String> = listOf(""),
    val phones: List<String> = listOf(""),
    val busy: Boolean = false,
    val error: String? = null,
)

/**
 * Адресная книга (контракт §15): одна сводная книга по всем источникам —
 * так её отдаёт сервер, источник наружу не показывается.
 */
class ContactsViewModel(private val api: SessionApi, private val onDead: () -> Unit) : ViewModel() {
    val contacts = MutableStateFlow<List<ContactDto>?>(null)
    val query = MutableStateFlow("")
    val selected = MutableStateFlow<ContactDto?>(null)
    val draft = MutableStateFlow<ContactDraft?>(null)
    val loading = MutableStateFlow(false)
    val notice = MutableStateFlow<String?>(null)
    private var identity = ""

    init {
        refresh()
        viewModelScope.launch {
            runCatching { api.identities() }.onSuccess { l -> identity = (l.firstOrNull { it.isDefault } ?: l.firstOrNull())?.email.orEmpty() }
        }
    }

    fun refresh() = viewModelScope.launch {
        loading.value = true
        try {
            val list = api.contacts()
            contacts.value = list
            selected.update { s -> s?.let { cur -> list.firstOrNull { it.id == cur.id } } }
        } catch (e: ApiException) {
            if (e is ApiException.Unauthorized) onDead() else notice.value = "Не удалось загрузить контакты"
        }
        loading.value = false
    }

    fun newContact() {
        draft.value = ContactDraft(id = null)
    }

    fun edit(c: ContactDto) {
        draft.value = ContactDraft(
            id = c.id, name = c.fullName, org = c.organization, title = c.title,
            emails = c.emails.ifEmpty { listOf("") }, phones = c.phones.ifEmpty { listOf("") },
        )
    }

    fun editDraft(f: (ContactDraft) -> ContactDraft) = draft.update { it?.let { d -> f(d).copy(error = null) } }

    fun save() {
        val d = draft.value ?: return
        if (d.busy) return
        val name = d.name.trim()
        val emails = d.emails.map { it.trim() }.filter { it.isNotEmpty() }
        val phones = d.phones.map { it.trim() }.filter { it.isNotEmpty() }
        if (name.isEmpty() && emails.isEmpty()) return draft.update { it?.copy(error = "Нужно имя или адрес") }
        draft.update { it?.copy(busy = true) }
        viewModelScope.launch {
            val req = ContactWriteRequest(name, emails.take(3), phones.take(3), d.org.trim(), d.title.trim(), if (d.id == null) identity else "")
            try {
                if (d.id == null) api.createContact(req) else api.patchContact(d.id, req)
                draft.value = null
                notice.value = if (d.id == null) "Контакт добавлен" else "Сохранено"
                refresh()
            } catch (e: ApiException) {
                draft.update { it?.copy(busy = false, error = errorText(e)) }
                if (e is ApiException.Unauthorized) onDead()
            }
        }
    }

    fun delete(c: ContactDto) = viewModelScope.launch {
        try {
            api.deleteContact(c.id)
            selected.value = null
            notice.value = "Контакт удалён"
            refresh()
        } catch (e: ApiException) {
            if (e is ApiException.Unauthorized) onDead() else notice.value = errorText(e)
        }
    }

    private fun errorText(e: ApiException) = when (e) {
        is ApiException.Unauthorized -> "Нужно войти заново"
        is ApiException.Network -> "Нет связи с сервером"
        is ApiException.Server -> if (e.code == 403) "Адресная книга только для чтения" else e.serverMessage.ifBlank { "Ошибка сервера (${e.code})" }
    }
}

private fun ContactDto.display(): String = fullName.ifBlank { emails.firstOrNull() ?: phones.firstOrNull() ?: "(без имени)" }

private fun ContactDto.matches(q: String): Boolean {
    val n = q.trim().lowercase()
    if (n.isEmpty()) return true
    return fullName.lowercase().contains(n) || organization.lowercase().contains(n) ||
        emails.any { it.lowercase().contains(n) } || phones.any { it.filter(Char::isDigit).contains(n.filter(Char::isDigit).ifEmpty { "\u0000" }) }
}

@Composable
fun ContactsScreen(vm: ContactsViewModel, avatars: Avatars, onMail: (String) -> Unit, modifier: Modifier = Modifier) {
    val wide = LocalConfiguration.current.screenWidthDp >= 600
    val selected by vm.selected.collectAsState()
    val snack = remember { SnackbarHostState() }
    val notice by vm.notice.collectAsState()
    LaunchedEffect(notice) { notice?.let { snack.showSnackbar(it); vm.notice.value = null } }
    LifecycleResumeEffect(Unit) {
        vm.refresh()
        onPauseOrDispose { }
    }
    BackHandler(enabled = !wide && selected != null) { vm.selected.value = null }
    Box(modifier.fillMaxSize()) {
        if (wide) {
            Row(Modifier.fillMaxSize()) {
                ContactList(vm, avatars, Modifier.width(340.dp).fillMaxHeight())
                VerticalDivider()
                Box(Modifier.weight(1f).fillMaxHeight()) {
                    selected?.let { ContactCard(it, vm, avatars, onMail, onBack = null) }
                        ?: Icon(Icons.Filled.Person, null, Modifier.size(96.dp).align(Alignment.Center), tint = MaterialTheme.colorScheme.outlineVariant)
                }
            }
        } else {
            val s = selected
            if (s != null) ContactCard(s, vm, avatars, onMail, onBack = { vm.selected.value = null })
            else ContactList(vm, avatars, Modifier.fillMaxSize())
        }
        SnackbarHost(snack, Modifier.align(Alignment.BottomCenter).padding(bottom = 84.dp))
    }
    ContactEditor(vm, wide)
}

@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun ContactList(vm: ContactsViewModel, avatars: Avatars, modifier: Modifier) {
    val all by vm.contacts.collectAsState()
    val q by vm.query.collectAsState()
    val loading by vm.loading.collectAsState()
    val selected by vm.selected.collectAsState()
    Box(modifier) {
        Column(Modifier.fillMaxSize()) {
            Text("Контакты", fontSize = 22.sp, fontWeight = FontWeight.Bold, modifier = Modifier.padding(start = 16.dp, top = 10.dp, bottom = 4.dp))
            TextField(
                q, { vm.query.value = it }, Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 4.dp),
                placeholder = { Text("Поиск") }, singleLine = true,
                leadingIcon = { Icon(Icons.Filled.Search, null) },
                trailingIcon = { if (q.isNotEmpty()) IconButton(onClick = { vm.query.value = "" }) { Icon(Icons.Filled.Close, "Очистить") } },
                shape = MaterialTheme.shapes.medium,
                colors = TextFieldDefaults.colors(focusedIndicatorColor = Color.Transparent, unfocusedIndicatorColor = Color.Transparent),
            )
            if (loading) LinearProgressIndicator(Modifier.fillMaxWidth().height(2.dp)) else Spacer(Modifier.height(2.dp))
            val list = all
            when {
                list == null -> Text("Загрузка…", Modifier.padding(24.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
                else -> {
                    val shown = list.filter { it.matches(q) }.sortedBy { it.display().lowercase() }
                    if (shown.isEmpty()) Text(if (q.isBlank()) "Контактов нет" else "Ничего не найдено", Modifier.padding(24.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
                    val groups = shown.groupBy { it.display().firstOrNull()?.uppercaseChar()?.takeIf(Char::isLetter) ?: '#' }
                    LazyColumn(Modifier.fillMaxSize()) {
                        groups.forEach { (letter, items) ->
                            stickyHeader(key = "h$letter") {
                                Text(
                                    "$letter", fontSize = 13.sp, fontWeight = FontWeight.SemiBold, color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    modifier = Modifier.fillMaxWidth().background(MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.9f)).padding(horizontal = 16.dp, vertical = 2.dp),
                                )
                            }
                            items(items, key = { it.id }) { c ->
                                Row(
                                    Modifier.fillMaxWidth()
                                        .background(if (selected?.id == c.id) MaterialTheme.colorScheme.primaryContainer else Color.Transparent)
                                        .clickable { vm.selected.value = c }.padding(horizontal = 16.dp, vertical = 8.dp),
                                    verticalAlignment = Alignment.CenterVertically,
                                ) {
                                    ContactAvatar(c, avatars, 36.dp)
                                    Spacer(Modifier.width(12.dp))
                                    Column(Modifier.weight(1f)) {
                                        Text(c.display(), fontSize = 16.sp, maxLines = 1, overflow = TextOverflow.Ellipsis)
                                        val sub = c.organization.ifBlank { c.emails.firstOrNull().orEmpty().takeIf { c.fullName.isNotBlank() }.orEmpty() }
                                        if (sub.isNotBlank()) Text(sub, fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
        FloatingActionButton(onClick = vm::newContact, Modifier.align(Alignment.BottomEnd).padding(16.dp)) { Icon(Icons.Filled.Add, "Новый контакт") }
    }
}

@Composable
private fun ContactAvatar(c: ContactDto, avatars: Avatars, size: Dp) {
    val email = c.emails.firstOrNull().orEmpty()
    val image by produceState(if (email.isEmpty()) null else avatars.cached(email), email) { if (email.isNotEmpty()) value = avatars.get(email) }
    image?.let {
        Image(it, null, contentScale = ContentScale.Crop, modifier = Modifier.size(size).clip(CircleShape))
        return
    }
    val initials = c.display().split(' ', '.', '@', '_', '-').filter { it.isNotBlank() }.take(2).joinToString("") { it.first().uppercase() }.ifEmpty { "?" }
    val palette = listOf(0xFF0E7490, 0xFF7C3AED, 0xFFB45309, 0xFF047857, 0xFFBE185D, 0xFF1D4ED8, 0xFF4D7C0F)
    Box(Modifier.size(size).clip(CircleShape).background(Color(palette[Math.floorMod(c.id.toInt(), palette.size)])), contentAlignment = Alignment.Center) {
        Text(initials, color = Color.White, fontWeight = FontWeight.SemiBold, fontSize = (size.value * 0.38f).sp)
    }
}

@Composable
private fun ContactCard(c: ContactDto, vm: ContactsViewModel, avatars: Avatars, onMail: (String) -> Unit, onBack: (() -> Unit)?) {
    val context = LocalContext.current
    var askDelete by remember(c.id) { mutableStateOf(false) }
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState())) {
        Row(Modifier.fillMaxWidth().padding(4.dp), verticalAlignment = Alignment.CenterVertically) {
            if (onBack != null) IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Назад") }
            Spacer(Modifier.weight(1f))
            IconButton(onClick = { vm.edit(c) }) { Icon(Icons.Filled.Edit, "Изменить") }
            IconButton(onClick = { askDelete = true }) { Icon(Icons.Filled.Delete, "Удалить", tint = MaterialTheme.colorScheme.error) }
        }
        Column(Modifier.fillMaxWidth().padding(horizontal = 24.dp), horizontalAlignment = Alignment.CenterHorizontally) {
            ContactAvatar(c, avatars, 88.dp)
            SelectionContainer { Text(c.display(), fontSize = 24.sp, fontWeight = FontWeight.SemiBold, modifier = Modifier.padding(top = 12.dp)) }
            val sub = listOf(c.title, c.organization).filter { it.isNotBlank() }.joinToString(" · ")
            if (sub.isNotBlank()) Text(sub, color = MaterialTheme.colorScheme.onSurfaceVariant)
            Row(Modifier.padding(vertical = 16.dp), horizontalArrangement = Arrangement.spacedBy(16.dp)) {
                c.emails.firstOrNull()?.let { e -> FilledIconButton(onClick = { onMail(e) }) { Icon(Icons.Filled.Email, "Написать") } }
                c.phones.firstOrNull()?.let { p -> FilledIconButton(onClick = { dial(context, p) }) { Icon(Icons.Filled.Call, "Позвонить") } }
            }
        }
        HorizontalDivider()
        c.emails.forEach { e -> CardLine(Icons.Filled.Email, e) { onMail(e) } }
        c.phones.forEach { p -> CardLine(Icons.Filled.Call, p) { dial(context, p) } }
        if (c.organization.isNotBlank()) CardLine(Icons.Filled.Business, c.organization, null)
    }
    if (askDelete) AlertDialog(
        onDismissRequest = { askDelete = false },
        title = { Text("Удалить контакт?") },
        text = { Text(c.display()) },
        confirmButton = { IconButton(onClick = { askDelete = false; vm.delete(c) }) { Icon(Icons.Filled.Delete, "Удалить", tint = MaterialTheme.colorScheme.error) } },
        dismissButton = { IconButton(onClick = { askDelete = false }) { Icon(Icons.Filled.Close, "Отмена") } },
    )
}

private fun dial(context: android.content.Context, phone: String) {
    runCatching { context.startActivity(Intent(Intent.ACTION_DIAL, Uri.parse("tel:" + phone.filter { it.isDigit() || it == '+' })).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)) }
}

@Composable
private fun CardLine(icon: ImageVector, text: String, click: (() -> Unit)?) {
    Row(
        Modifier.fillMaxWidth().then(if (click != null) Modifier.clickable(onClick = click) else Modifier).padding(horizontal = 24.dp, vertical = 14.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(icon, null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
        Spacer(Modifier.width(18.dp))
        Text(text, fontSize = 16.sp, color = if (click != null) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurface)
    }
}

@Composable
private fun ContactEditor(vm: ContactsViewModel, wide: Boolean) {
    val d by vm.draft.collectAsState()
    val draft = d ?: return
    Dialog(onDismissRequest = { if (!draft.busy) vm.draft.value = null }, properties = DialogProperties(usePlatformDefaultWidth = false, decorFitsSystemWindows = false)) {
        Surface(
            Modifier.then(if (wide) Modifier.widthIn(max = 560.dp).padding(24.dp) else Modifier.fillMaxSize()).imePadding(),
            shape = if (wide) MaterialTheme.shapes.large else androidx.compose.ui.graphics.RectangleShape,
        ) {
            Column {
                Row(Modifier.fillMaxWidth().padding(4.dp).then(if (!wide) Modifier.padding(top = 24.dp) else Modifier), verticalAlignment = Alignment.CenterVertically) {
                    IconButton(onClick = { vm.draft.value = null }, enabled = !draft.busy) { Icon(Icons.Filled.Close, "Отмена") }
                    Text(if (draft.id == null) "Новый контакт" else "Изменить", fontSize = 18.sp, fontWeight = FontWeight.SemiBold, modifier = Modifier.weight(1f))
                    if (draft.busy) CircularProgressIndicator(Modifier.size(24.dp), strokeWidth = 2.dp)
                    else FilledIconButton(onClick = vm::save) { Icon(Icons.Filled.Check, "Сохранить") }
                    Spacer(Modifier.width(8.dp))
                }
                Column(Modifier.verticalScroll(rememberScrollState()).padding(16.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
                    draft.error?.let { Text(it, color = MaterialTheme.colorScheme.error) }
                    OutlinedTextField(draft.name, { v -> vm.editDraft { it.copy(name = v) } }, Modifier.fillMaxWidth(), placeholder = { Text("Имя") }, singleLine = true, leadingIcon = { Icon(Icons.Filled.Person, null) })
                    OutlinedTextField(draft.org, { v -> vm.editDraft { it.copy(org = v) } }, Modifier.fillMaxWidth(), placeholder = { Text("Организация") }, singleLine = true, leadingIcon = { Icon(Icons.Filled.Business, null) })
                    OutlinedTextField(draft.title, { v -> vm.editDraft { it.copy(title = v) } }, Modifier.fillMaxWidth(), placeholder = { Text("Должность") }, singleLine = true)
                    MultiField(draft.emails, Icons.Filled.Email, "Адрес", KeyboardType.Email) { l -> vm.editDraft { it.copy(emails = l) } }
                    MultiField(draft.phones, Icons.Filled.Call, "Телефон", KeyboardType.Phone) { l -> vm.editDraft { it.copy(phones = l) } }
                }
            }
        }
    }
}

/** До трёх значений: пустое последнее поле добавляет следующее, как в iOS. */
@Composable
private fun MultiField(values: List<String>, icon: ImageVector, hint: String, type: KeyboardType, onChange: (List<String>) -> Unit) {
    val shown = if (values.size < 3 && values.lastOrNull()?.isNotBlank() == true) values + "" else values
    shown.forEachIndexed { i, v ->
        OutlinedTextField(
            v, { nv -> onChange(shown.toMutableList().also { it[i] = nv }) }, Modifier.fillMaxWidth(),
            placeholder = { Text(hint) }, singleLine = true, leadingIcon = { Icon(icon, null) },
            keyboardOptions = KeyboardOptions(keyboardType = type),
            trailingIcon = { if (v.isNotEmpty()) IconButton(onClick = { onChange(shown.filterIndexed { j, _ -> j != i }.ifEmpty { listOf("") }) }) { Icon(Icons.Filled.Close, "Убрать") } },
        )
    }
}
