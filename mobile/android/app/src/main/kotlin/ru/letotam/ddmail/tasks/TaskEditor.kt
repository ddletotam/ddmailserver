package ru.letotam.ddmail.tasks

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Notes
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Event
import androidx.compose.material.icons.filled.Today
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.DatePicker
import androidx.compose.material3.DatePickerDialog
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilledIconButton
import androidx.compose.material3.FilterChip
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TimePicker
import androidx.compose.material3.rememberDatePickerState
import androidx.compose.material3.rememberTimePickerState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import ru.letotam.ddmail.calendar.CalendarColors
import ru.letotam.ddmail.calendar.MONTHS_GEN
import java.time.Instant
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter

private val TIME = DateTimeFormatter.ofPattern("HH:mm")

/**
 * Форма задачи (контракт §14): заголовок, список (только у новой — перенос
 * между списками сервер не делает), срок датой или датой со временем,
 * заметка; у существующей — удаление. Сложенный — на весь экран, разложенный
 * — окном, как форма события и контакта.
 */
@OptIn(ExperimentalMaterial3Api::class, ExperimentalLayoutApi::class)
@Composable
fun TaskEditor(vm: TasksViewModel, wide: Boolean) {
    val d by vm.draft.collectAsState()
    val draft = d ?: return
    val lists by vm.lists.collectAsState()
    var pick by remember { mutableStateOf<String?>(null) } // date / time
    var askDelete by remember { mutableStateOf(false) }
    val ro = draft.readOnly
    val crowd = ru.letotam.ddmail.ui.imeCrowds()
    Dialog(onDismissRequest = { vm.draft.value = null }, properties = DialogProperties(usePlatformDefaultWidth = false, decorFitsSystemWindows = false)) {
        Surface(
            Modifier.then(if (wide) Modifier.widthIn(max = 560.dp).padding(if (crowd) 0.dp else 24.dp) else Modifier.fillMaxSize()).imePadding(),
            shape = if (wide) MaterialTheme.shapes.large else androidx.compose.ui.graphics.RectangleShape,
        ) {
            Column(Modifier.then(if (wide) Modifier else Modifier.fillMaxSize())) {
                // Клавиатура в тесноте — без шапки: видно поле ввода, «Готово» её закроет.
                if (!crowd) Row(Modifier.fillMaxWidth().padding(4.dp).then(if (!wide) Modifier.padding(top = 24.dp) else Modifier), verticalAlignment = Alignment.CenterVertically) {
                    IconButton(onClick = { vm.draft.value = null }) { Icon(Icons.Filled.Close, "Отмена") }
                    Text(if (draft.original == null) "Новая задача" else "Задача", fontSize = 18.sp, fontWeight = FontWeight.SemiBold, modifier = Modifier.weight(1f))
                    if (draft.original != null && !ro) IconButton(onClick = { askDelete = true }) { Icon(Icons.Filled.Delete, "Удалить задачу") }
                    if (!ro) FilledIconButton(onClick = vm::save) { Icon(Icons.Filled.Check, "Сохранить") }
                    Spacer(Modifier.width(8.dp))
                }
                Column(Modifier.verticalScroll(rememberScrollState()).padding(horizontal = 16.dp).padding(bottom = 16.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
                    if (ro) Text("Список только для чтения", fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    draft.error?.let {
                        Text(it, color = MaterialTheme.colorScheme.error, modifier = Modifier.fillMaxWidth().clip(MaterialTheme.shapes.small).background(MaterialTheme.colorScheme.errorContainer).padding(10.dp))
                    }
                    OutlinedTextField(
                        draft.summary, { v -> vm.editDraft { it.copy(summary = v) } }, Modifier.fillMaxWidth(),
                        placeholder = { Text("Заголовок") }, singleLine = true, readOnly = ro, textStyle = MaterialTheme.typography.titleMedium,
                    )
                    if (draft.original == null) {
                        if (lists.isEmpty()) Text("Нет списков задач, доступных на запись", color = MaterialTheme.colorScheme.error)
                        FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                            lists.forEach { c ->
                                FilterChip(
                                    selected = draft.calendarId == c.id,
                                    onClick = { vm.editDraft { it.copy(calendarId = c.id) } },
                                    label = { Text(c.name.ifBlank { "Задачи" }, maxLines = 1) },
                                    leadingIcon = { Box(Modifier.size(10.dp).clip(CircleShape).background(CalendarColors.of(c, c.id))) },
                                )
                            }
                        }
                    } else if (draft.original.calendarName.isNotBlank()) {
                        Text(draft.original.calendarName, fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Icon(Icons.Filled.Event, null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
                        Spacer(Modifier.width(12.dp))
                        Text("Срок", Modifier.weight(1f))
                        Switch(draft.hasDue, { v -> vm.editDraft { it.copy(hasDue = v) } }, enabled = !ro)
                    }
                    if (draft.hasDue) {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Icon(Icons.Filled.Today, null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
                            Spacer(Modifier.width(12.dp))
                            Text("Весь день", Modifier.weight(1f))
                            Switch(draft.allDay, { v -> vm.editDraft { it.copy(allDay = v) } }, enabled = !ro)
                        }
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Spacer(Modifier.width(36.dp))
                            val dd = draft.dueDate
                            TextButton(onClick = { pick = "date" }, enabled = !ro) { Text("${dd.dayOfMonth} ${MONTHS_GEN[dd.monthValue - 1]} ${dd.year}", fontSize = 15.sp) }
                            if (!draft.allDay) TextButton(onClick = { pick = "time" }, enabled = !ro) { Text(draft.dueTime.format(TIME), fontSize = 15.sp) }
                        }
                    }
                    OutlinedTextField(
                        draft.description, { v -> vm.editDraft { it.copy(description = v) } }, Modifier.fillMaxWidth(),
                        placeholder = { Text("Заметка") }, minLines = 3, readOnly = ro, leadingIcon = { Icon(Icons.AutoMirrored.Filled.Notes, null) },
                    )
                }
            }
        }
    }
    when (pick) {
        "date" -> {
            val st = rememberDatePickerState(initialSelectedDateMillis = draft.dueDate.atStartOfDay().toInstant(ZoneOffset.UTC).toEpochMilli())
            DatePickerDialog(
                onDismissRequest = { pick = null },
                confirmButton = {
                    IconButton(onClick = {
                        st.selectedDateMillis?.let { ms ->
                            val date = Instant.ofEpochMilli(ms).atZone(ZoneOffset.UTC).toLocalDate()
                            vm.editDraft { it.copy(dueDate = date) }
                        }
                        pick = null
                    }) { Icon(Icons.Filled.Check, "Готово") }
                },
                dismissButton = { IconButton(onClick = { pick = null }) { Icon(Icons.Filled.Close, "Отмена") } },
            ) { DatePicker(st, showModeToggle = false) }
        }
        "time" -> {
            val st = rememberTimePickerState(draft.dueTime.hour, draft.dueTime.minute, is24Hour = true)
            AlertDialog(
                onDismissRequest = { pick = null },
                confirmButton = {
                    IconButton(onClick = {
                        vm.editDraft { it.copy(dueTime = java.time.LocalTime.of(st.hour, st.minute)) }
                        pick = null
                    }) { Icon(Icons.Filled.Check, "Готово") }
                },
                dismissButton = { IconButton(onClick = { pick = null }) { Icon(Icons.Filled.Close, "Отмена") } },
                text = { TimePicker(st) },
            )
        }
    }
    val orig = draft.original
    if (askDelete && orig != null) AlertDialog(
        onDismissRequest = { askDelete = false },
        title = { Text("Удалить задачу?") },
        text = { Text(orig.summary.ifBlank { "(без названия)" }) },
        confirmButton = { IconButton(onClick = { askDelete = false; vm.delete(orig) }) { Icon(Icons.Filled.Delete, "Удалить", tint = MaterialTheme.colorScheme.error) } },
        dismissButton = { IconButton(onClick = { askDelete = false }) { Icon(Icons.Filled.Close, "Отмена") } },
    )
}
