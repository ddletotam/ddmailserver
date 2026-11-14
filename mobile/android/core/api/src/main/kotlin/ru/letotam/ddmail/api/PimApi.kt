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
    /**
     * Вся книга (до [limit]). У сервера ddmail — страницами по
     * [SessionApi.CONTACTS_PAGE] через `offset`: до 0.0.40 клиент брал одну
     * страницу в 5000, и книга в 15 000 карточек обрывалась на «Ару…».
     */
    suspend fun contacts(limit: Int = Int.MAX_VALUE): List<ContactDto>

    /**
     * Поиск по всей книге на стороне источника (`/contacts/search`, до
     * [limit]): находит и то, чего нет в загруженном списке. Источник без
     * поиска — пусто (тогда хватает локального поиска по загруженному).
     */
    suspend fun searchContacts(query: String, limit: Int = 100): List<ContactDto> = emptyList()
    suspend fun addressBooks(): List<AddressBookDto>

    /** Новый контакт; ответ — его id, если источник его сообщил (ddmail: `{"id":…}`). */
    suspend fun createContact(req: ContactWriteRequest): Long?
    suspend fun patchContact(id: Long, req: ContactWriteRequest)
    suspend fun deleteContact(id: Long)
}
