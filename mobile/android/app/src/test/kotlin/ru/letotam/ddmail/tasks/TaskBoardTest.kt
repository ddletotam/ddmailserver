package ru.letotam.ddmail.tasks

import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineStart
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.api.CalendarDto
import ru.letotam.ddmail.api.TaskApi
import ru.letotam.ddmail.api.TaskCreateRequest
import ru.letotam.ddmail.api.TaskDto
import ru.letotam.ddmail.api.TaskPatchRequest
import java.time.LocalDate
import java.time.LocalTime
import java.time.ZoneId

/** Оптимистичные правки задач на фальшивом API: строка — сразу, отказ — откат. */
class TaskBoardTest {
    private val moscow = ZoneId.of("Europe/Moscow")

    /** Фальшивый сервер: каждый вызов ждёт [gate] (если задан) и отвечает [fail] или успехом. */
    private class FakeApi : TaskApi {
        var fail: ApiException? = null
        var gate: CompletableDeferred<Unit>? = null
        val calls = mutableListOf<String>()
        var lastCreate: TaskCreateRequest? = null
        var lastPatch: TaskPatchRequest? = null
        var server = mutableListOf(
            TaskDto(1, calendarId = 10, summary = "Оплатить офис", canWrite = true, calendarName = "Напоминания"),
            TaskDto(2, calendarId = 30, summary = "Чужое", canWrite = false),
        )

        private suspend fun step(call: String) {
            calls += call
            gate?.await()
            fail?.let { throw it }
        }

        override suspend fun tasks(includeCompleted: Boolean) = server.toList().also { step("tasks") }
        override suspend fun setTaskCompleted(id: Long, completed: Boolean) = step("done $id $completed")
        override suspend fun calendars() = listOf(
            CalendarDto(10, "Напоминания", sourceType = "caldav", canWrite = true, components = listOf("VTODO")),
            CalendarDto(20, "Дом", sourceType = "caldav", canWrite = true, components = listOf("VEVENT")),
            CalendarDto(30, "Подписка", sourceType = "ics_url", canWrite = false, components = listOf("VTODO")),
            CalendarDto(40, "Работа", sourceType = "caldav", canWrite = true, components = listOf("VEVENT", "VTODO")),
        )

        override suspend fun createTask(req: TaskCreateRequest): TaskDto {
            lastCreate = req
            step("create")
            return TaskDto(500, calendarId = req.calendarId, summary = req.summary, description = req.description, due = req.due, allDay = req.allDay, canWrite = true, uid = "x@ddmailserver")
        }

        override suspend fun patchTask(id: Long, req: TaskPatchRequest): TaskDto {
            lastPatch = req
            step("patch $id")
            val o = server.first { it.id == id }
            return o.copy(summary = req.summary ?: o.summary, description = req.description ?: o.description, status = "NEEDS-ACTION")
        }

        override suspend fun deleteTask(id: Long) = step("delete $id")
    }

    private fun board(api: FakeApi, dead: () -> Unit = {}) = TaskBoard(api, dead)

    @Test
    fun taskListsAreWritableVtodoCollectionsAndFirstIsDefault() = runBlocking {
        val b = board(FakeApi())
        b.loadLists()
        assertEquals(listOf(10L, 40L), b.lists.value.map { it.id })
        assertEquals(10L, TaskDraft.new(b.lists.value).calendarId)
        assertEquals(0L, TaskDraft.new(emptyList()).calendarId)
    }

    @Test
    fun validateNeedsTitleAndList() {
        val b = board(FakeApi())
        assertEquals("Нужен заголовок", b.validate(TaskDraft(null, summary = "  ", calendarId = 10)))
        assertEquals("Нет списка задач, куда можно записать", b.validate(TaskDraft(null, summary = "x", calendarId = 0)))
        assertNull(b.validate(TaskDraft(null, summary = "x", calendarId = 10)))
        val ro = TaskDraft.of(TaskDto(2, summary = "Чужое", canWrite = false))
        assertEquals("Список только для чтения", b.validate(ro))
    }

    @Test
    fun createShowsRowAtOnceThenSwapsInServerTask() = runBlocking {
        val api = FakeApi()
        val b = board(api)
        b.load(false)
        b.loadLists()
        api.gate = CompletableDeferred()
        val d = TaskDraft.new(b.lists.value).copy(summary = " Купить хлеб ", description = "чёрный", hasDue = true, dueDate = LocalDate.of(2026, 11, 13), allDay = true)
        val job = launch(start = CoroutineStart.UNDISPATCHED) { b.save(d, moscow) }

        // Ответа ещё нет, а строка уже в списке — с временным отрицательным id.
        val pending = b.tasks.value!!.last()
        assertTrue(pending.id < 0)
        assertEquals("Купить хлеб", pending.summary)
        assertEquals("Напоминания", pending.calendarName)

        api.gate!!.complete(Unit)
        job.join()
        val created = b.tasks.value!!.last()
        assertEquals(500L, created.id)
        assertEquals(3, b.tasks.value!!.size)
        // Срок датой — полночь UTC, не полночь Москвы.
        assertEquals(1794528000000L, api.lastCreate!!.due)
        assertTrue(api.lastCreate!!.allDay)
        assertEquals(10L, api.lastCreate!!.calendarId)
        assertNull(b.notice.value)
    }

    @Test
    fun createRefusedRemovesRowAndTellsWhy() = runBlocking {
        val api = FakeApi()
        val b = board(api)
        b.load(false)
        api.fail = ApiException.Server(403, "calendar is read-only")
        b.save(TaskDraft(null, summary = "x", calendarId = 10), moscow)
        assertEquals(listOf(1L, 2L), b.tasks.value!!.map { it.id })
        assertEquals("Задача не создана: список только для чтения", b.notice.value)
    }

    @Test
    fun timedDueIsDeviceWallClock() {
        val d = TaskDraft(null, summary = "x", hasDue = true, allDay = false, dueDate = LocalDate.of(2026, 11, 13), dueTime = LocalTime.of(12, 30))
        assertEquals(1794562200000L, d.dueMs(moscow)) // 09:30Z
        assertNull(d.copy(hasDue = false).dueMs(moscow))
    }

    @Test
    fun editSendsOnlyChangedFieldsAndRollsBackOnFailure() = runBlocking {
        val api = FakeApi()
        val b = board(api)
        b.load(false)
        val orig = b.tasks.value!!.first()
        val d = TaskDraft.of(orig, moscow).copy(summary = "Оплатить офис и склад")
        assertEquals(TaskPatchRequest(summary = "Оплатить офис и склад"), TaskBoard.patchOf(orig, d, moscow))

        api.gate = CompletableDeferred()
        val job = launch(start = CoroutineStart.UNDISPATCHED) { b.save(d, moscow) }
        assertEquals("Оплатить офис и склад", b.tasks.value!!.first().summary)
        api.fail = ApiException.Network(java.io.IOException("timeout"))
        api.gate!!.complete(Unit)
        job.join()
        assertEquals("Оплатить офис", b.tasks.value!!.first().summary)
        assertEquals("Изменения не сохранены: нет связи с сервером", b.notice.value)
    }

    @Test
    fun unchangedFormSendsNothing() = runBlocking {
        val api = FakeApi()
        val b = board(api)
        b.load(false)
        b.save(TaskDraft.of(b.tasks.value!!.first(), moscow), moscow)
        assertFalse(api.calls.any { it.startsWith("patch") })
    }

    @Test
    fun clearingDueSendsZero() {
        val orig = TaskDto(1, summary = "x", due = 1794528000000L, allDay = true, canWrite = true)
        val d = TaskDraft.of(orig, moscow)
        assertEquals(LocalDate.of(2026, 11, 13), d.dueDate)
        assertEquals(TaskPatchRequest(due = 0), TaskBoard.patchOf(orig, d.copy(hasDue = false), moscow))
        // Дата та же, но со временем — меняется и вид срока.
        val timed = TaskBoard.patchOf(orig, d.copy(allDay = false, dueTime = LocalTime.of(12, 30)), moscow)
        assertEquals(1794562200000L, timed.due)
        assertEquals(false, timed.allDay)
    }

    @Test
    fun deleteRemovesAtOnceAndRestoresInPlaceOnFailure() = runBlocking {
        val api = FakeApi()
        val b = board(api)
        b.load(false)
        api.gate = CompletableDeferred()
        val first = b.tasks.value!!.first()
        val job = launch(start = CoroutineStart.UNDISPATCHED) { b.delete(first) }
        assertEquals(listOf(2L), b.tasks.value!!.map { it.id })
        api.fail = ApiException.Server(500, "boom")
        api.gate!!.complete(Unit)
        job.join()
        assertEquals(listOf(1L, 2L), b.tasks.value!!.map { it.id })
        assertEquals("Задача не удалена", b.notice.value)
    }

    @Test
    fun readOnlyRowsAreNotTouched() = runBlocking {
        val api = FakeApi()
        val b = board(api)
        b.load(false)
        val ro = b.tasks.value!!.last()
        b.toggle(ro)
        b.delete(ro)
        assertEquals(listOf("tasks"), api.calls)
    }

    @Test
    fun deadSessionLogsOutInsteadOfNotice() = runBlocking {
        val api = FakeApi()
        var dead = false
        val b = board(api) { dead = true }
        b.load(false)
        api.fail = ApiException.Unauthorized("token expired")
        b.toggle(b.tasks.value!!.first())
        assertTrue(dead)
        assertNull(b.notice.value)
        assertFalse(b.tasks.value!!.first().completed)
    }
}
