package ru.letotam.ddmail.tasks

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.update
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.api.CalendarDto
import ru.letotam.ddmail.api.TaskApi
import ru.letotam.ddmail.api.TaskCreateRequest
import ru.letotam.ddmail.api.TaskDto
import ru.letotam.ddmail.api.TaskPatchRequest
import java.time.Instant
import java.time.LocalDate
import java.time.LocalDateTime
import java.time.LocalTime
import java.time.ZoneId
import java.time.ZoneOffset

/**
 * Форма задачи: новая ([original] = null) или правка. Срок — дата, а со
 * временем — дата и время в поясе устройства. «На весь день» уходит на
 * сервер полуночью UTC даты — так такие даты хранит сервер.
 */
data class TaskDraft(
    val original: TaskDto?,
    val summary: String = "",
    val description: String = "",
    /** Список задач новой задачи; 0 — списков нет (сохранить нельзя). */
    val calendarId: Long = 0,
    val hasDue: Boolean = false,
    val dueDate: LocalDate = LocalDate.now(),
    val allDay: Boolean = true,
    val dueTime: LocalTime = LocalTime.of(9, 0),
    val error: String? = null,
) {
    /** Задача из списка только для чтения: форма показывает, но не правит. */
    val readOnly: Boolean get() = original != null && !original.canWrite

    /** Срок в мс, как его ждёт сервер; null — без срока. */
    fun dueMs(zone: ZoneId = ZoneId.systemDefault()): Long? = when {
        !hasDue -> null
        allDay -> dueDate.atStartOfDay(ZoneOffset.UTC).toInstant().toEpochMilli()
        else -> LocalDateTime.of(dueDate, dueTime).atZone(zone).toInstant().toEpochMilli()
    }

    companion object {
        fun new(lists: List<CalendarDto>, today: LocalDate = LocalDate.now()) =
            TaskDraft(original = null, calendarId = lists.firstOrNull()?.id ?: 0, dueDate = today)

        fun of(t: TaskDto, zone: ZoneId = ZoneId.systemDefault()): TaskDraft {
            val due = t.due
            val at = due?.let {
                if (t.allDay) Instant.ofEpochMilli(it).atZone(ZoneOffset.UTC).toLocalDateTime()
                else Instant.ofEpochMilli(it).atZone(zone).toLocalDateTime()
            }
            return TaskDraft(
                original = t, summary = t.summary, description = t.description, calendarId = t.calendarId,
                hasDue = due != null, dueDate = at?.toLocalDate() ?: LocalDate.now(zone),
                allDay = due == null || t.allDay, dueTime = at?.toLocalTime()?.takeIf { !t.allDay } ?: LocalTime.of(9, 0),
            )
        }
    }
}

/**
 * Список задач и его правки (контракт §14): галочка, создание, правка и
 * удаление — оптимистичны. Строка меняется сразу, сервер узнаёт следом;
 * отказ возвращает строку как была и оставляет сообщение в [notice].
 * Без Android: проверяется юнит-тестами на подменённом [TaskApi].
 */
class TaskBoard(private val api: TaskApi, private val onDead: () -> Unit) {
    /** null — ещё грузится («ещё грузится» и «правда пусто» различаются). */
    val tasks = MutableStateFlow<List<TaskDto>?>(null)
    /** Списки, куда можно создать задачу; первый — по умолчанию. */
    val lists = MutableStateFlow<List<CalendarDto>>(emptyList())
    val notice = MutableStateFlow<String?>(null)

    private var tempIds = 0L

    suspend fun load(includeCompleted: Boolean) {
        try {
            tasks.value = api.tasks(includeCompleted)
        } catch (e: ApiException) {
            fail(e, "Не удалось загрузить задачи")
        }
    }

    suspend fun loadLists() {
        try {
            lists.value = api.calendars().filter { it.acceptsTasks }
        } catch (e: ApiException) {
            if (e is ApiException.Unauthorized) onDead()
        }
    }

    suspend fun toggle(t: TaskDto) {
        if (!t.canWrite || t.id < 0) return
        val done = !t.completed
        replace(t.id) { it.copy(completed = done) }
        try {
            api.setTaskCompleted(t.id, done)
        } catch (e: ApiException) {
            replace(t.id) { it.copy(completed = t.completed) }
            fail(e, "Не удалось сохранить")
        }
    }

    /**
     * Проверка формы до отправки: ошибка — текст для формы, null — можно
     * сохранять (форма закрывается сразу, дальше — оптимистично).
     */
    fun validate(d: TaskDraft): String? = when {
        d.readOnly -> "Список только для чтения"
        d.summary.isBlank() -> "Нужен заголовок"
        d.original == null && d.calendarId == 0L -> "Нет списка задач, куда можно записать"
        else -> null
    }

    /** Сохраняет форму; к этому моменту [validate] уже вернул null. */
    suspend fun save(d: TaskDraft, zone: ZoneId = ZoneId.systemDefault()) {
        if (d.original == null) create(d, zone) else update(d.original, d, zone)
    }

    private suspend fun create(d: TaskDraft, zone: ZoneId) {
        val list = lists.value.firstOrNull { it.id == d.calendarId }
        val due = d.dueMs(zone)
        // Временный id — отрицательный: сервер раздаёт только положительные.
        val temp = TaskDto(
            id = -(++tempIds), calendarId = d.calendarId, summary = d.summary.trim(), description = d.description,
            due = due, allDay = due != null && d.allDay, status = "NEEDS-ACTION", canWrite = true,
            color = list?.color.orEmpty(), calendarName = list?.name.orEmpty(),
        )
        tasks.update { (it ?: emptyList()) + temp }
        try {
            val created = api.createTask(
                TaskCreateRequest(d.calendarId, temp.summary, d.description, due, due != null && d.allDay),
            )
            tasks.update { l -> l?.map { if (it.id == temp.id) created else it } }
        } catch (e: ApiException) {
            tasks.update { l -> l?.filterNot { it.id == temp.id } }
            fail(e, "Задача не создана")
        }
    }

    private suspend fun update(orig: TaskDto, d: TaskDraft, zone: ZoneId) {
        val req = patchOf(orig, d, zone)
        if (req.isEmpty) return
        replace(orig.id) {
            it.copy(
                summary = req.summary ?: it.summary,
                description = req.description ?: it.description,
                due = when (req.due) { null -> it.due; 0L -> null; else -> req.due },
                allDay = if (req.due == 0L) false else req.allDay ?: it.allDay,
            )
        }
        try {
            val saved = api.patchTask(orig.id, req)
            replace(orig.id) { saved }
        } catch (e: ApiException) {
            replace(orig.id) { orig }
            fail(e, "Изменения не сохранены")
        }
    }

    suspend fun delete(t: TaskDto) {
        if (!t.canWrite || t.id < 0) return
        val before = tasks.value
        val at = before?.indexOfFirst { it.id == t.id } ?: -1
        tasks.update { l -> l?.filterNot { it.id == t.id } }
        try {
            api.deleteTask(t.id)
        } catch (e: ApiException) {
            // Назад на прежнее место, а не в конец.
            tasks.update { l ->
                val cur = l ?: emptyList()
                if (cur.any { it.id == t.id }) cur
                else cur.toMutableList().apply { add(at.coerceIn(0, size), t) }
            }
            fail(e, "Задача не удалена")
        }
    }

    private fun replace(id: Long, f: (TaskDto) -> TaskDto) = tasks.update { l -> l?.map { if (it.id == id) f(it) else it } }

    private fun fail(e: ApiException, what: String) {
        if (e is ApiException.Unauthorized) {
            onDead()
            return
        }
        notice.value = when {
            e is ApiException.Server && e.code == 403 -> "$what: список только для чтения"
            e is ApiException.Server && e.code == 422 -> "$what: этот список не принимает задачи"
            e is ApiException.Network -> "$what: нет связи с сервером"
            else -> what
        }
    }

    companion object {
        /**
         * Правка — только изменённые поля: срок, которого не трогали, не
         * переписывается (у задачи Apple он может быть в своём поясе, и
         * пересохранение в UTC его бы сдвинуло в чужом клиенте).
         */
        fun patchOf(orig: TaskDto, d: TaskDraft, zone: ZoneId = ZoneId.systemDefault()): TaskPatchRequest {
            val summary = d.summary.trim().takeIf { it != orig.summary }
            val description = d.description.takeIf { it != orig.description }
            val due = d.dueMs(zone)
            val dueChanged = due != orig.due || (due != null && d.allDay != orig.allDay)
            return TaskPatchRequest(
                summary = summary,
                description = description,
                due = if (dueChanged) due ?: 0L else null,
                allDay = if (dueChanged && due != null) d.allDay else null,
            )
        }
    }
}
