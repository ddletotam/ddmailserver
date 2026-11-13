package ru.letotam.ddmail.api

/**
 * Задачи (VTODO) — только у сервера ddmail: внешних CalDAV-источников у
 * задач на клиенте нет. Отдельно от [PimApi], чтобы экран задач можно было
 * проверить на подменённом API. Ошибки — [ApiException].
 */
interface TaskApi {
    suspend fun tasks(includeCompleted: Boolean): List<TaskDto>
    suspend fun setTaskCompleted(id: Long, completed: Boolean)

    /** Коллекции пользователя; списки задач — с [CalendarDto.acceptsTasks]. */
    suspend fun calendars(): List<CalendarDto>

    /** Создаёт задачу; ответ — она же, как её отдаёт GET /tasks. */
    suspend fun createTask(req: TaskCreateRequest): TaskDto
    suspend fun patchTask(id: Long, req: TaskPatchRequest): TaskDto
    suspend fun deleteTask(id: Long)
}
