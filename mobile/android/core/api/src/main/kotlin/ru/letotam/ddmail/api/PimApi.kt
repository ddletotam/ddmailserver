package ru.letotam.ddmail.api

/**
 * Календарь и контакты так, как их видят экраны: сервер ddmail
 * ([SessionApi]) или сводка с внешними CalDAV/CardDAV (`PimHub`). Ошибки —
 * [ApiException].
 */
interface PimApi {
    suspend fun identities(): List<Identity>
    suspend fun calendars(): List<CalendarDto>

    /** События окна `[fromMs, toMs)`; повторяющиеся — мастерами (развёртывает клиент). */
    suspend fun calendarEvents(fromMs: Long, toMs: Long): List<EventDto>
    suspend fun createEvent(req: EventCreateRequest): String
    suspend fun patchEvent(id: Long, req: EventPatchRequest): String
    suspend fun deleteEvent(id: Long)

    /** Ответ на приглашение: ACCEPTED | TENTATIVE | DECLINED. */
    suspend fun rsvp(id: Long, partstat: String)
    suspend fun contacts(limit: Int = 5000): List<ContactDto>
    suspend fun addressBooks(): List<AddressBookDto>
    suspend fun createContact(req: ContactWriteRequest)
    suspend fun patchContact(id: Long, req: ContactWriteRequest)
    suspend fun deleteContact(id: Long)
}
