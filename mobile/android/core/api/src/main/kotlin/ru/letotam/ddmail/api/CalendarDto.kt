package ru.letotam.ddmail.api

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

// Календарь, задачи, контакты — зеркало Desktop*-структур
// internal/web/handlers_desktop_{calendar,tasks,contacts,event_*}.go.

@Serializable
data class CalendarDto(
    val id: Long,
    val name: String = "",
    val description: String = "",
    val color: String = "",
    @SerialName("source_type") val sourceType: String = "",
    @SerialName("can_write") val canWrite: Boolean = false,
    val enabled: Boolean = true,
    val timezone: String = "",
    /** Что коллекция принимает: VEVENT, VTODO или оба. Пусто — старый сервер, только события. */
    val components: List<String> = emptyList(),
) {
    /** Можно ли создать здесь задачу: пишется и принимает VTODO (списки Напоминаний Apple — только VTODO). */
    val acceptsTasks: Boolean
        get() = canWrite && sourceType != "ics_url" && sourceType != "ics_import" &&
            components.any { it.equals("VTODO", ignoreCase = true) }
}

@Serializable
data class AttendeeDto(
    val email: String = "",
    val name: String = "",
    val role: String = "",
    val partstat: String = "",
)

@Serializable
data class EventExtraDto(val name: String = "", val value: String = "")

/** Событие. Повторяющееся приходит мастером — развёртывает клиент. */
@Serializable
data class EventDto(
    val id: Long,
    @SerialName("calendar_id") val calendarId: Long = 0,
    val uid: String = "",
    val summary: String = "",
    val description: String = "",
    val location: String = "",
    /** Миллисекунды. */
    val dtstart: Long = 0,
    val dtend: Long? = null,
    @SerialName("all_day") val allDay: Boolean = false,
    @SerialName("organizer_email") val organizerEmail: String = "",
    @SerialName("organizer_name") val organizerName: String = "",
    val status: String = "",
    val rrule: String = "",
    @SerialName("recurrence_id") val recurrenceId: String = "",
    val exdates: List<Long> = emptyList(),
    val attendees: List<AttendeeDto> = emptyList(),
    @SerialName("alarm_lead_min") val alarmLeadMin: Int = 0,
    @SerialName("alarm_leads") val alarmLeads: List<Int> = emptyList(),
    val extras: List<EventExtraDto> = emptyList(),
    val color: String = "",
    val editable: Boolean = false,
    val deletable: Boolean = false,
    @SerialName("identity_email") val identityEmail: String = "",
)

@Serializable
data class EventCreateRequest(
    @SerialName("calendar_id") val calendarId: Long,
    val summary: String,
    val description: String = "",
    val location: String = "",
    val dtstart: Long,
    val dtend: Long?,
    @SerialName("all_day") val allDay: Boolean,
)

/** Правка: `null` — поле не трогать. `scope` — all | future (single сервер пока не умеет). */
@Serializable
data class EventPatchRequest(
    val scope: String = "all",
    @SerialName("recurrence_id") val recurrenceId: Long = 0,
    val summary: String? = null,
    val description: String? = null,
    val location: String? = null,
    val dtstart: Long? = null,
    val dtend: Long? = null,
    @SerialName("all_day") val allDay: Boolean? = null,
)

@Serializable
data class TaskDto(
    val id: Long,
    @SerialName("calendar_id") val calendarId: Long = 0,
    val uid: String = "",
    val summary: String = "",
    val description: String = "",
    val start: Long? = null,
    val due: Long? = null,
    @SerialName("all_day") val allDay: Boolean = false,
    val status: String = "",
    val completed: Boolean = false,
    @SerialName("completed_at") val completedAt: Long? = null,
    @SerialName("percent_complete") val percentComplete: Int? = null,
    val priority: Int? = null,
    val rrule: String = "",
    val color: String = "",
    @SerialName("can_write") val canWrite: Boolean = false,
    @SerialName("identity_email") val identityEmail: String = "",
    @SerialName("calendar_name") val calendarName: String = "",
)

/** Новая задача (POST /tasks). [due] — мс; при [allDay] — полночь UTC нужной даты. */
@Serializable
data class TaskCreateRequest(
    @SerialName("calendar_id") val calendarId: Long,
    val summary: String,
    val description: String = "",
    val due: Long? = null,
    @SerialName("all_day") val allDay: Boolean = false,
)

/**
 * Правка задачи (PATCH /tasks/{id}): `null` — поле не трогать (в JSON не
 * пишется), `due = 0` — снять срок. Коллекцию правкой не меняют.
 */
@Serializable
data class TaskPatchRequest(
    val summary: String? = null,
    val description: String? = null,
    val due: Long? = null,
    @SerialName("all_day") val allDay: Boolean? = null,
) {
    val isEmpty: Boolean get() = summary == null && description == null && due == null && allDay == null
}

@Serializable
data class ContactDto(
    val id: Long,
    @SerialName("full_name") val fullName: String = "",
    val emails: List<String> = emptyList(),
    val phones: List<String> = emptyList(),
    val organization: String = "",
    val title: String = "",
    @SerialName("photo_url") val photoUrl: String = "",
    /** Книга контакта (у внешних CardDAV — отрицательный id книги телефона). */
    @SerialName("address_book_id") val addressBookId: Long = 0,
    /**
     * Можно ли править и удалять (контракт §15): `false` — книга только для
     * чтения (корпоративная, LDAP). Старый сервер поля не шлёт — правка есть,
     * отказ придёт 403 `address_book_read_only`.
     */
    @SerialName("can_write") val canWrite: Boolean = true,
)

/** Адресная книга — куда записать новый контакт (выбирает пользователь, не сервер). */
@Serializable
data class AddressBookDto(
    val id: Long,
    val name: String = "",
    @SerialName("source_name") val sourceName: String = "",
    @SerialName("source_type") val sourceType: String = "",
    @SerialName("identity_email") val identityEmail: String = "",
    @SerialName("can_write") val canWrite: Boolean = false,
)

@Serializable
data class ContactWriteRequest(
    @SerialName("full_name") val fullName: String,
    val emails: List<String>,
    val phones: List<String>,
    val organization: String = "",
    val title: String = "",
    @SerialName("from_identity") val fromIdentity: String = "",
    /** Книга нового контакта; 0 — не передаётся (правка). */
    @SerialName("address_book_id") val addressBookId: Long = 0,
)
