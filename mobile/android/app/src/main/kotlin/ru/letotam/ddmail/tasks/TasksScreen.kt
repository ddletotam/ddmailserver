package ru.letotam.ddmail.tasks

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.DoneAll
import androidx.compose.material.icons.filled.Flag
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.IconToggleButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
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
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextDecoration
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.LifecycleResumeEffect
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ru.letotam.ddmail.DdmailApp
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.api.SessionApi
import ru.letotam.ddmail.api.TaskDto
import ru.letotam.ddmail.calendar.CalendarColors
import ru.letotam.ddmail.calendar.MONTHS_GEN
import ru.letotam.ddmail.calendar.Occ
import java.time.LocalDate
import java.time.ZoneId
import java.time.format.DateTimeFormatter

/**
 * Задачи (VTODO, контракт §14; §5е десктопа): окна по времени нет — забираем
 * все; выполненные скрыты, тумблер меняет запрос к серверу; галочка
 * оптимистична — строка меняется сразу, отказ сервера откатывает.
 */
class TasksViewModel(private val app: DdmailApp, private val api: SessionApi, private val onDead: () -> Unit) : ViewModel() {
    val tasks = MutableStateFlow<List<TaskDto>?>(null) // null — ещё грузится
    val showDone = MutableStateFlow(false)
    val loading = MutableStateFlow(false)
    val notice = MutableStateFlow<String?>(null)
    val open = MutableStateFlow<TaskDto?>(null)

    init {
        viewModelScope.launch {
            showDone.value = app.calendarPrefs.showCompleted.first()
            refresh()
        }
        viewModelScope.launch { app.calendarChanged.collect { refresh() } }
    }

    fun refresh() = viewModelScope.launch {
        loading.value = true
        try {
            tasks.value = api.tasks(showDone.value)
        } catch (e: ApiException) {
            if (e is ApiException.Unauthorized) onDead() else notice.value = "Не удалось загрузить задачи"
        }
        loading.value = false
    }

    fun toggleShowDone() {
        showDone.update { !it }
        viewModelScope.launch { app.calendarPrefs.setShowCompleted(showDone.value) }
        refresh()
    }

    fun toggle(t: TaskDto) {
        if (!t.canWrite) return
        val done = !t.completed
        tasks.update { l -> l?.map { if (it.id == t.id) it.copy(completed = done) else it } }
        open.update { if (it?.id == t.id) it.copy(completed = done) else it }
        viewModelScope.launch {
            try {
                api.setTaskCompleted(t.id, done)
            } catch (e: ApiException) {
                tasks.update { l -> l?.map { if (it.id == t.id) it.copy(completed = t.completed) else it } }
                if (e is ApiException.Unauthorized) onDead() else notice.value = if (e is ApiException.Server && e.code == 403) "Список только для чтения" else "Не удалось сохранить"
            }
        }
    }
}

private val HHMM = DateTimeFormatter.ofPattern("HH:mm")
private val Overdue = Color(0xFFDC2626)
private val Today = Color(0xFFD97706)

/** Срок: текст и цвет — красный просрочено, янтарный сегодня, серый остальное. */
private fun dueOf(t: TaskDto): Pair<String, Color?>? {
    val due = t.due ?: return null
    val zone = ZoneId.systemDefault()
    val d = Occ.dateOf(due, t.allDay, zone)
    val today = LocalDate.now(zone)
    val time = if (t.allDay) "" else ", " + java.time.Instant.ofEpochMilli(due).atZone(zone).format(HHMM)
    val day = when (d) {
        today -> "Сегодня"
        today.plusDays(1) -> "Завтра"
        today.minusDays(1) -> "Вчера"
        else -> "${d.dayOfMonth} ${MONTHS_GEN[d.monthValue - 1]}" + if (d.year != today.year) " ${d.year}" else ""
    }
    val overdue = if (t.allDay) d < today else due < System.currentTimeMillis()
    val color = when {
        t.completed -> null
        overdue -> Overdue
        d == today -> Today
        else -> null
    }
    return day + time to color
}

@Composable
fun TasksScreen(vm: TasksViewModel, modifier: Modifier = Modifier) {
    val tasks by vm.tasks.collectAsState()
    val showDone by vm.showDone.collectAsState()
    val loading by vm.loading.collectAsState()
    val snack = remember { SnackbarHostState() }
    val notice by vm.notice.collectAsState()
    LaunchedEffect(notice) { notice?.let { snack.showSnackbar(it); vm.notice.value = null } }
    LifecycleResumeEffect(Unit) {
        vm.refresh()
        onPauseOrDispose { }
    }
    Box(modifier.fillMaxSize()) {
        Column(Modifier.fillMaxSize().align(Alignment.TopCenter).widthIn(max = 720.dp)) {
            Row(Modifier.fillMaxWidth().padding(start = 16.dp, end = 4.dp, top = 4.dp), verticalAlignment = Alignment.CenterVertically) {
                Text("Задачи", fontSize = 22.sp, fontWeight = FontWeight.Bold, modifier = Modifier.weight(1f))
                IconToggleButton(showDone, { vm.toggleShowDone() }) {
                    Icon(Icons.Filled.DoneAll, if (showDone) "Скрыть выполненные" else "Показать выполненные", tint = if (showDone) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.outline)
                }
                IconButton(onClick = { vm.refresh() }) { Icon(Icons.Filled.Refresh, "Обновить") }
            }
            if (loading) LinearProgressIndicator(Modifier.fillMaxWidth().height(2.dp)) else Spacer(Modifier.height(2.dp))
            val list = tasks
            when {
                // Пустое состояние различает «ещё грузится» и «правда ничего нет».
                list == null -> Text("Загрузка…", Modifier.padding(24.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
                list.isEmpty() -> Column(Modifier.fillMaxWidth().padding(top = 64.dp), horizontalAlignment = Alignment.CenterHorizontally) {
                    Icon(Icons.Filled.DoneAll, null, Modifier.size(48.dp), tint = MaterialTheme.colorScheme.outline)
                    Text(if (showDone) "Задач нет" else "Невыполненных задач нет", color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(top = 8.dp))
                }
                else -> {
                    val sorted = list.sortedWith(compareBy<TaskDto>({ it.completed }, { it.due ?: Long.MAX_VALUE }, { it.priority?.takeIf { p -> p > 0 } ?: 10 }, { it.summary.lowercase() }))
                    LazyColumn(Modifier.fillMaxSize()) {
                        items(sorted, key = { it.id }) { t ->
                            TaskRow(t, onCheck = { vm.toggle(t) }, onOpen = { vm.open.value = t })
                            HorizontalDivider(Modifier.padding(start = 56.dp), color = MaterialTheme.colorScheme.outlineVariant.copy(alpha = 0.5f))
                        }
                    }
                }
            }
        }
        SnackbarHost(snack, Modifier.align(Alignment.BottomCenter).padding(bottom = 16.dp))
    }
    val opened by vm.open.collectAsState()
    opened?.let { t ->
        AlertDialog(
            onDismissRequest = { vm.open.value = null },
            title = { Text(t.summary.ifBlank { "(без названия)" }) },
            text = {
                Column {
                    Text(t.calendarName, fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    dueOf(t)?.let { (txt, c) -> Text("Срок: $txt", color = c ?: Color.Unspecified, modifier = Modifier.padding(top = 6.dp)) }
                    if (t.description.isNotBlank()) SelectionContainer { Text(t.description, Modifier.padding(top = 10.dp)) }
                }
            },
            confirmButton = {
                if (t.canWrite) IconButton(onClick = { vm.toggle(t) }) {
                    Icon(Icons.Filled.Check, if (t.completed) "Не выполнено" else "Выполнено", tint = if (t.completed) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.outline)
                }
            },
            dismissButton = { IconButton(onClick = { vm.open.value = null }) { Icon(Icons.Filled.Close, "Закрыть") } },
        )
    }
}

@Composable
private fun TaskRow(t: TaskDto, onCheck: () -> Unit, onOpen: () -> Unit) {
    val color = CalendarColors.parse(t.color) ?: MaterialTheme.colorScheme.primary
    Row(Modifier.fillMaxWidth().clickable(onClick = onOpen).padding(horizontal = 12.dp, vertical = 10.dp), verticalAlignment = Alignment.CenterVertically) {
        Box(
            Modifier.size(26.dp).clip(CircleShape)
                .background(if (t.completed) color else Color.Transparent)
                .border(2.dp, color, CircleShape)
                .clickable(enabled = t.canWrite, onClick = onCheck),
            contentAlignment = Alignment.Center,
        ) {
            if (t.completed) Icon(Icons.Filled.Check, null, Modifier.size(16.dp), tint = Color.White)
        }
        Spacer(Modifier.width(18.dp))
        Column(Modifier.weight(1f)) {
            Text(
                t.summary.ifBlank { "(без названия)" }, fontSize = 16.sp, maxLines = 2, overflow = TextOverflow.Ellipsis,
                color = if (t.completed) MaterialTheme.colorScheme.outline else MaterialTheme.colorScheme.onSurface,
                textDecoration = if (t.completed) TextDecoration.LineThrough else null,
            )
            dueOf(t)?.let { (txt, c) -> Text(txt, fontSize = 13.sp, color = c ?: MaterialTheme.colorScheme.onSurfaceVariant) }
        }
        if ((t.priority ?: 0) in 1..4 && !t.completed) Icon(Icons.Filled.Flag, "Важная", Modifier.size(18.dp), tint = Overdue)
    }
}
