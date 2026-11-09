package ru.letotam.ddmail.calendar

import android.content.Intent
import android.net.Uri
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
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
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.AccessTime
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.Groups
import androidx.compose.material.icons.filled.HelpOutline
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material.icons.filled.Place
import androidx.compose.material.icons.filled.Repeat
import androidx.compose.material.icons.filled.Videocam
import androidx.compose.material.icons.filled.Notes
import androidx.compose.material.icons.filled.Today
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DatePicker
import androidx.compose.material3.DatePickerDialog
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilledIconButton
import androidx.compose.material3.FilledTonalIconToggleButton
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.IconButtonDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.SegmentedButton
import androidx.compose.material3.SegmentedButtonDefaults
import androidx.compose.material3.SingleChoiceSegmentedButtonRow
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TimePicker
import androidx.compose.material3.rememberDatePickerState
import androidx.compose.material3.rememberModalBottomSheetState
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
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import java.time.Instant
import kotlinx.coroutines.launch
import java.time.LocalDate
import java.time.LocalDateTime
import java.time.ZoneId
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter

private val HHMM = DateTimeFormatter.ofPattern("HH:mm")

fun longDate(d: LocalDate): String = "${WEEKDAYS_FULL[d.dayOfWeek.value - 1]}, ${d.dayOfMonth} ${MONTHS_GEN[d.monthValue - 1]} ${d.year}"

private fun shortDate(d: LocalDate): String = "${d.dayOfMonth} ${MONTHS_GEN[d.monthValue - 1]} ${d.year}"

/** Когда: «весь день» / «10:00–11:00» / через дни — с датами. */
fun whenText(o: Occ): Pair<String, String> {
    val zone = ZoneId.systemDefault()
    val days = o.dates(zone).toList()
    val first = days.first()
    val last = days.last()
    return if (o.allDay) {
        if (first == last) longDate(first) to "весь день" else "${shortDate(first)} — ${shortDate(last)}" to "весь день"
    } else {
        val s = o.startTime(zone)
        val e = o.endTime(zone)
        if (s.toLocalDate() == e.toLocalDate() || o.end - o.start < 86_400_000L && last == first) {
            longDate(first) to "${s.format(HHMM)} – ${e.format(HHMM)}"
        } else {
            "${shortDate(s.toLocalDate())}, ${s.format(HHMM)} —" to "${shortDate(e.toLocalDate())}, ${e.format(HHMM)}"
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun EventDetailSheet(vm: CalendarViewModel) {
    val o by vm.detail.collectAsState()
    val occ = o ?: return
    val cals by vm.calendars.collectAsState()
    val cal = cals.firstOrNull { it.id == occ.ev.calendarId }
    val context = LocalContext.current
    var askDelete by remember(occ.key) { mutableStateOf(false) }
    ModalBottomSheet(onDismissRequest = { vm.open(null) }, sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)) {
        Column(Modifier.fillMaxWidth().verticalScroll(rememberScrollState()).padding(horizontal = 20.dp).padding(bottom = 24.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Box(Modifier.size(12.dp).clip(CircleShape).background(occ.color))
                Spacer(Modifier.width(8.dp))
                Text(cal?.name ?: "", fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.weight(1f), maxLines = 1)
                if (occ.ev.editable) IconButton(onClick = { vm.edit(occ) }) { Icon(Icons.Filled.Edit, "Изменить") }
                if (occ.ev.deletable) IconButton(onClick = { askDelete = true }) { Icon(Icons.Filled.Delete, "Удалить", tint = MaterialTheme.colorScheme.error) }
                IconButton(onClick = { vm.open(null) }) { Icon(Icons.Filled.Close, "Закрыть") }
            }
            SelectionContainer {
                Text(occ.ev.summary.ifBlank { "(без названия)" }, fontSize = 22.sp, fontWeight = FontWeight.SemiBold, modifier = Modifier.padding(vertical = 6.dp))
            }
            val (w1, w2) = whenText(occ)
            InfoRow(Icons.Filled.AccessTime) {
                Text(w1, fontSize = 15.sp)
                Text(w2, fontSize = 15.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            if (occ.recurring) InfoRow(Icons.Filled.Repeat) { Text(Recurrence.describe(occ.ev.rrule).ifEmpty { "Повторяется" }, fontSize = 15.sp) }
            if (occ.ev.location.isNotBlank()) {
                val link = EventLinks.target(occ.ev.location)
                InfoRow(Icons.Filled.Place) {
                    Text(
                        occ.ev.location, fontSize = 15.sp,
                        color = if (link != null) MaterialTheme.colorScheme.primary else Color.Unspecified,
                        modifier = if (link != null) Modifier.clickable { openUrl(context, link) } else Modifier,
                    )
                }
            }
            val links = EventLinks.of(occ.ev).filter { it != EventLinks.target(occ.ev.location) }
            links.forEach { u ->
                InfoRow(Icons.Filled.Videocam) {
                    Text(u, fontSize = 15.sp, color = MaterialTheme.colorScheme.primary, maxLines = 2, modifier = Modifier.clickable { openUrl(context, u) })
                }
            }
            if (!occ.allDay) {
                val leads = occ.ev.alarmLeads.ifEmpty { if (occ.ev.alarmLeadMin > 0) listOf(occ.ev.alarmLeadMin) else emptyList() }
                if (leads.isNotEmpty()) InfoRow(Icons.Filled.Notifications) {
                    Text(leads.sortedDescending().joinToString(", ") { if (it == 0) "в момент начала" else "за ${leadText(it)}" }, fontSize = 15.sp)
                }
            }
            val mine = vm.myPartstat(occ)
            if (mine != null && occ.ev.editable) {
                HorizontalDivider(Modifier.padding(vertical = 8.dp))
                Row(horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
                    RsvpButton(Icons.Filled.Check, "Принять", mine == "ACCEPTED", Color(0xFF10B981)) { vm.rsvp(occ, "ACCEPTED") }
                    RsvpButton(Icons.Filled.HelpOutline, "Под вопросом", mine == "TENTATIVE", Color(0xFFF59E0B)) { vm.rsvp(occ, "TENTATIVE") }
                    RsvpButton(Icons.Filled.Close, "Отклонить", mine == "DECLINED", Color(0xFFEF4444)) { vm.rsvp(occ, "DECLINED") }
                }
            }
            if (occ.ev.attendees.isNotEmpty() || occ.ev.organizerEmail.isNotBlank()) {
                HorizontalDivider(Modifier.padding(vertical = 8.dp))
                InfoRow(Icons.Filled.Groups) {
                    if (occ.ev.organizerEmail.isNotBlank()) {
                        Text("${occ.ev.organizerName.ifBlank { occ.ev.organizerEmail }} · организатор", fontSize = 14.sp)
                    }
                    occ.ev.attendees.forEach { a ->
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            PartstatDot(a.partstat)
                            Spacer(Modifier.width(6.dp))
                            Text(a.name.ifBlank { a.email }, fontSize = 14.sp, maxLines = 1)
                        }
                    }
                }
            }
            if (occ.ev.description.isNotBlank()) {
                HorizontalDivider(Modifier.padding(vertical = 8.dp))
                InfoRow(Icons.Filled.Notes) { SelectionContainer { Text(occ.ev.description, fontSize = 14.sp) } }
            }
        }
    }
    if (askDelete) AlertDialog(
        onDismissRequest = { askDelete = false },
        title = { Text(if (occ.recurring) "Удалить всю серию?" else "Удалить событие?") },
        text = { if (occ.recurring) Text("Сервер пока удаляет повторяющееся событие только целиком — со всеми повторами.") },
        confirmButton = { IconButton(onClick = { askDelete = false; vm.delete(occ) }) { Icon(Icons.Filled.Delete, "Удалить", tint = MaterialTheme.colorScheme.error) } },
        dismissButton = { IconButton(onClick = { askDelete = false }) { Icon(Icons.Filled.Close, "Отмена") } },
    )
}

private fun leadText(m: Int): String = when {
    m % 1440 == 0 -> "${m / 1440} д"
    m % 60 == 0 -> "${m / 60} ч"
    else -> "$m мин"
}

fun openUrl(context: android.content.Context, url: String) {
    runCatching { context.startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(url)).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)) }
}

@Composable
private fun InfoRow(icon: ImageVector, content: @Composable () -> Unit) {
    Row(Modifier.fillMaxWidth().padding(vertical = 5.dp)) {
        Icon(icon, null, Modifier.size(20.dp).padding(top = 1.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
        Spacer(Modifier.width(14.dp))
        Column { content() }
    }
}

@Composable
private fun RsvpButton(icon: ImageVector, label: String, on: Boolean, color: Color, click: () -> Unit) {
    FilledTonalIconToggleButton(
        checked = on, onCheckedChange = { click() },
        colors = IconButtonDefaults.filledTonalIconToggleButtonColors(checkedContainerColor = color, checkedContentColor = Color.White),
    ) { Icon(icon, label) }
}

@Composable
private fun PartstatDot(p: String) {
    val (c, icon) = when (p.uppercase()) {
        "ACCEPTED" -> Color(0xFF10B981) to Icons.Filled.Check
        "DECLINED" -> Color(0xFFEF4444) to Icons.Filled.Close
        "TENTATIVE" -> Color(0xFFF59E0B) to Icons.Filled.HelpOutline
        else -> MaterialTheme.colorScheme.outline to null
    }
    Box(Modifier.size(16.dp).clip(CircleShape).background(c), contentAlignment = Alignment.Center) {
        if (icon != null) Icon(icon, p, Modifier.size(12.dp), tint = Color.White)
    }
}

/** Форма события: на весь экран в сложенном виде, окном — в разложенном. */
@OptIn(ExperimentalMaterial3Api::class, androidx.compose.foundation.layout.ExperimentalLayoutApi::class)
@Composable
fun EventEditor(vm: CalendarViewModel, wide: Boolean) {
    val d by vm.draft.collectAsState()
    val draft = d ?: return
    val cals by vm.calendars.collectAsState()
    var pick by remember { mutableStateOf<String?>(null) } // sd / st / ed / et
    val crowd = ru.letotam.ddmail.ui.imeCrowds()
    Dialog(onDismissRequest = { if (!draft.busy) vm.draft.value = null }, properties = DialogProperties(usePlatformDefaultWidth = false, decorFitsSystemWindows = false)) {
        Surface(
            Modifier.then(if (wide) Modifier.widthIn(max = 600.dp).padding(if (crowd) 0.dp else 24.dp) else Modifier.fillMaxSize()).imePadding(),
            shape = if (wide) MaterialTheme.shapes.large else androidx.compose.ui.graphics.RectangleShape,
        ) {
            Column(Modifier.then(if (wide) Modifier else Modifier.fillMaxSize())) {
                // Клавиатура в тесноте — без шапки: видно поле ввода, «Готово» её закроет.
                if (!crowd) Row(Modifier.fillMaxWidth().padding(horizontal = 4.dp, vertical = 4.dp).then(if (!wide) Modifier.padding(top = 24.dp) else Modifier), verticalAlignment = Alignment.CenterVertically) {
                    IconButton(onClick = { vm.draft.value = null }, enabled = !draft.busy) { Icon(Icons.Filled.Close, "Отмена") }
                    Text(if (draft.original == null) "Новое событие" else "Изменить", fontSize = 18.sp, fontWeight = FontWeight.SemiBold, modifier = Modifier.weight(1f))
                    if (draft.busy) CircularProgressIndicator(Modifier.size(24.dp).padding(end = 8.dp), strokeWidth = 2.dp)
                    else FilledIconButton(onClick = vm::save, enabled = draft.calendarId != 0L) { Icon(Icons.Filled.Check, "Сохранить") }
                    Spacer(Modifier.width(8.dp))
                }
                Column(Modifier.verticalScroll(rememberScrollState()).padding(horizontal = 16.dp).padding(bottom = 16.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
                    draft.error?.let {
                        Text(it, color = MaterialTheme.colorScheme.error, modifier = Modifier.fillMaxWidth().clip(MaterialTheme.shapes.small).background(MaterialTheme.colorScheme.errorContainer).padding(10.dp))
                    }
                    OutlinedTextField(draft.summary, { v -> vm.editDraft { it.copy(summary = v) } }, Modifier.fillMaxWidth(), placeholder = { Text("Название") }, singleLine = true, textStyle = MaterialTheme.typography.titleMedium)
                    if (draft.original == null) {
                        val writable = cals.filter { it.canWrite }
                        if (writable.isEmpty()) Text("Нет календарей, доступных на запись", color = MaterialTheme.colorScheme.error)
                        // Календаря по умолчанию нет — выбор обязателен, и это видно сразу (§12).
                        else if (draft.calendarId == 0L) Text("Куда записать? Календарь по умолчанию — звёздочкой в списке календарей.", fontSize = 13.sp, color = MaterialTheme.colorScheme.error)
                        androidx.compose.foundation.layout.FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                            writable.forEach { c ->
                                FilterChip(
                                    selected = draft.calendarId == c.id,
                                    onClick = { vm.editDraft { it.copy(calendarId = c.id) } },
                                    label = { Text(c.name, maxLines = 1) },
                                    leadingIcon = { Box(Modifier.size(10.dp).clip(CircleShape).background(CalendarColors.of(c, c.id))) },
                                )
                            }
                        }
                    }
                    OutlinedTextField(draft.location, { v -> vm.editDraft { it.copy(location = v) } }, Modifier.fillMaxWidth(), placeholder = { Text("Место или ссылка") }, singleLine = true, leadingIcon = { Icon(Icons.Filled.Place, null) })
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Icon(Icons.Filled.Today, null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
                        Spacer(Modifier.width(12.dp))
                        Text("Весь день", Modifier.weight(1f))
                        Switch(draft.allDay, { v -> vm.editDraft { it.copy(allDay = v) } })
                    }
                    DateTimeRow("Начало", draft.start, draft.allDay, { pick = "sd" }, { pick = "st" })
                    DateTimeRow("Конец", draft.end, draft.allDay, { pick = "ed" }, { pick = "et" })
                    if (draft.original?.recurring == true) {
                        Text("Изменить", fontSize = 13.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        SingleChoiceSegmentedButtonRow(Modifier.fillMaxWidth()) {
                            listOf("all" to "Все повторы", "future" to "Это и следующие").forEachIndexed { i, (k, label) ->
                                SegmentedButton(draft.scope == k, { vm.editDraft { it.copy(scope = k) } }, SegmentedButtonDefaults.itemShape(i, 2)) { Text(label, fontSize = 13.sp) }
                            }
                        }
                    }
                    OutlinedTextField(draft.description, { v -> vm.editDraft { it.copy(description = v) } }, Modifier.fillMaxWidth(), placeholder = { Text("Заметки") }, minLines = 3, leadingIcon = { Icon(Icons.Filled.Notes, null) })
                }
            }
        }
    }
    when (pick) {
        "sd", "ed" -> {
            val cur = if (pick == "sd") draft.start else draft.end
            val st = rememberDatePickerState(initialSelectedDateMillis = cur.toLocalDate().atStartOfDay().toInstant(ZoneOffset.UTC).toEpochMilli())
            DatePickerDialog(
                onDismissRequest = { pick = null },
                confirmButton = {
                    IconButton(onClick = {
                        st.selectedDateMillis?.let { ms ->
                            val date = Instant.ofEpochMilli(ms).atZone(ZoneOffset.UTC).toLocalDate()
                            vm.editDraft { dr ->
                                if (pick == "sd") {
                                    // Начало переехало — конец едет следом, длительность сохраняется.
                                    val dur = java.time.Duration.between(dr.start, dr.end)
                                    val ns = LocalDateTime.of(date, dr.start.toLocalTime())
                                    dr.copy(start = ns, end = ns.plus(dur))
                                } else dr.copy(end = LocalDateTime.of(date, dr.end.toLocalTime()))
                            }
                        }
                        pick = null
                    }) { Icon(Icons.Filled.Check, "Готово") }
                },
                dismissButton = { IconButton(onClick = { pick = null }) { Icon(Icons.Filled.Close, "Отмена") } },
            ) { DatePicker(st, showModeToggle = false) }
        }
        "st", "et" -> {
            val cur = if (pick == "st") draft.start else draft.end
            val st = rememberTimePickerState(cur.hour, cur.minute, is24Hour = true)
            AlertDialog(
                onDismissRequest = { pick = null },
                confirmButton = {
                    IconButton(onClick = {
                        vm.editDraft { dr ->
                            if (pick == "st") {
                                val dur = java.time.Duration.between(dr.start, dr.end)
                                val ns = dr.start.withHour(st.hour).withMinute(st.minute)
                                dr.copy(start = ns, end = ns.plus(dur))
                            } else dr.copy(end = dr.end.withHour(st.hour).withMinute(st.minute))
                        }
                        pick = null
                    }) { Icon(Icons.Filled.Check, "Готово") }
                },
                dismissButton = { IconButton(onClick = { pick = null }) { Icon(Icons.Filled.Close, "Отмена") } },
                text = { TimePicker(st) },
            )
        }
    }
}

@Composable
private fun DateTimeRow(label: String, v: LocalDateTime, allDay: Boolean, onDate: () -> Unit, onTime: () -> Unit) {
    Row(verticalAlignment = Alignment.CenterVertically) {
        Text(label, Modifier.width(72.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
        TextButton(onClick = onDate) { Text(shortDate(v.toLocalDate()), fontSize = 15.sp) }
        if (!allDay) TextButton(onClick = onTime) { Text(v.format(HHMM), fontSize = 15.sp) }
    }
}

/** «Напомнить позже» из уведомления: шаги, что ложатся до начала, и «в момент начала». */
@Composable
fun SnoozeDialog(app: ru.letotam.ddmail.DdmailApp) {
    val req by app.snoozeRequests.collectAsState()
    val (eventId, occStart) = req ?: return
    var row by remember(req) { mutableStateOf<ru.letotam.ddmail.db.ReminderEntity?>(null) }
    androidx.compose.runtime.LaunchedEffect(req) { row = app.reminders.row(eventId, occStart) }
    val scope = androidx.compose.runtime.rememberCoroutineScope()
    val close = { app.snoozeRequests.value = null }
    val minsUntil = (occStart - System.currentTimeMillis()) / 60_000
    fun choose(at: Long) {
        scope.launch { app.reminders.snooze(eventId, occStart, at) }
        close()
    }
    AlertDialog(
        onDismissRequest = close,
        title = { Text(row?.summary ?: "Напомнить позже", maxLines = 2) },
        text = {
            Column(Modifier.verticalScroll(rememberScrollState())) {
                if (minsUntil > 0) MenuLine(Icons.Filled.Notifications, "В момент начала") { choose(occStart) }
                ReminderPlanner.snoozeSteps(minsUntil).forEach { m ->
                    MenuLine(Icons.Filled.AccessTime, ReminderPlanner.snoozeLabel(m)) { choose(System.currentTimeMillis() + m * 60_000L) }
                }
            }
        },
        confirmButton = {},
        dismissButton = { IconButton(onClick = close) { Icon(Icons.Filled.Close, "Отмена") } },
    )
}

@Composable
private fun MenuLine(icon: ImageVector, text: String, click: () -> Unit) {
    Row(Modifier.fillMaxWidth().clickable(onClick = click).padding(vertical = 12.dp), verticalAlignment = Alignment.CenterVertically) {
        Icon(icon, null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
        Spacer(Modifier.width(14.dp))
        Text(text, fontSize = 16.sp)
    }
}
