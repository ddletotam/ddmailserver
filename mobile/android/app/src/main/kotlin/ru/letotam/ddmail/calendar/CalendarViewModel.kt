package ru.letotam.ddmail.calendar

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ru.letotam.ddmail.DdmailApp
import ru.letotam.ddmail.api.ApiException
import ru.letotam.ddmail.api.CalendarDto
import ru.letotam.ddmail.api.EventCreateRequest
import ru.letotam.ddmail.api.EventDto
import ru.letotam.ddmail.api.EventPatchRequest
import ru.letotam.ddmail.api.SessionApi
import java.time.LocalDate
import java.time.LocalDateTime
import java.time.YearMonth
import java.time.ZoneId

enum class CalView { Day, Week, Month, Year }

/** Форма события: новое ([original] = null) или правка вхождения. */
data class EventDraft(
    val original: Occ?,
    val calendarId: Long,
    val summary: String = "",
    val location: String = "",
    val description: String = "",
    val allDay: Boolean = false,
    val start: LocalDateTime,
    val end: LocalDateTime,
    /** all | future — у повторяющегося; «только это» сервер пока не умеет. */
    val scope: String = "all",
    val busy: Boolean = false,
    val error: String? = null,
)

/**
 * Календарь (контракт §12): окна по месяцам, развёртка на клиенте, правки —
 * через сервер с ожиданием ответа (карточка закрывается по подтверждению, как
 * §5б-бис десктопа). Видимость календарей — локальная.
 */
class CalendarViewModel(
    private val app: DdmailApp,
    private val api: SessionApi,
    private val onDead: () -> Unit,
) : ViewModel() {
    private val zone: ZoneId get() = ZoneId.systemDefault()

    val calendars = MutableStateFlow<List<CalendarDto>>(emptyList())
    val hidden = MutableStateFlow<Set<Long>>(emptySet())
    val defaultCalendar = MutableStateFlow<Long?>(null)
    val view = MutableStateFlow(CalView.Month)
    val selected = MutableStateFlow(LocalDate.now())
    val detail = MutableStateFlow<Occ?>(null)
    val draft = MutableStateFlow<EventDraft?>(null)
    val notice = MutableStateFlow<String?>(null)
    val loading = MutableStateFlow(false)

    /** Свои адреса — найти себя среди участников (RSVP). */
    val myEmails = MutableStateFlow<Set<String>>(emptySet())

    private val raw = HashMap<YearMonth, List<EventDto>>()
    private val inflight = HashMap<YearMonth, Job>()

    /** Месяц → день → вхождения (скрытые календари уже отфильтрованы). */
    private val _index = MutableStateFlow<Map<YearMonth, Map<LocalDate, List<Occ>>>>(emptyMap())
    val index: StateFlow<Map<YearMonth, Map<LocalDate, List<Occ>>>> = _index

    init {
        viewModelScope.launch {
            app.calendarPrefs.view()?.let { v -> CalView.entries.firstOrNull { it.name == v }?.let { view.value = it } }
        }
        viewModelScope.launch {
            app.calendarPrefs.hidden.collect {
                hidden.value = it
                reindex()
            }
        }
        viewModelScope.launch { app.calendarChanged.collect { refresh() } }
        viewModelScope.launch { app.calendarPrefs.defaultCalendar.collect { defaultCalendar.value = it } }
        loadCalendars()
        viewModelScope.launch {
            runCatching { api.identities() }.onSuccess { list -> myEmails.value = list.map { it.email.lowercase() }.toSet() }
        }
    }

    fun setView(v: CalView) {
        view.value = v
        viewModelScope.launch { app.calendarPrefs.setView(v.name) }
    }

    fun select(d: LocalDate) {
        selected.value = d
    }

    fun today() = select(LocalDate.now())

    private fun loadCalendars() = viewModelScope.launch {
        guard { calendars.value = api.calendars().sortedBy { it.name.lowercase() } }
        reindex()
    }

    /** Нужны события месяца: свежий запрос, если его ещё нет. */
    fun ensure(ym: YearMonth) {
        if (raw.containsKey(ym) || inflight.containsKey(ym)) return
        fetch(ym)
    }

    private fun fetch(ym: YearMonth) {
        inflight[ym]?.cancel()
        inflight[ym] = viewModelScope.launch {
            loading.value = true
            val from = ym.atDay(1).atStartOfDay(zone).toInstant().toEpochMilli()
            val to = ym.plusMonths(1).atDay(1).atStartOfDay(zone).toInstant().toEpochMilli()
            // Запас на сутки с каждой стороны: даты «весь день» лежат в полночь UTC.
            guard {
                raw[ym] = api.calendarEvents(from - 86_400_000L, to + 86_400_000L)
                reindex(ym)
            }
            inflight.remove(ym)
            loading.value = inflight.isNotEmpty()
        }
    }

    /** Сервер сообщил об изменениях / вернулись в приложение: перечитать загруженное. */
    fun refresh() {
        loadCalendars()
        raw.keys.toList().forEach { fetch(it) }
        viewModelScope.launch { runCatching { app.reminders.refresh(api) } }
    }

    private fun reindex(only: YearMonth? = null) {
        val cals = calendars.value.associateBy { it.id }
        val h = hidden.value
        _index.update { cur ->
            if (only != null) cur + (only to indexMonth(only, raw[only].orEmpty(), cals, h, zone))
            else raw.mapValues { (ym, evs) -> indexMonth(ym, evs, cals, h, zone) }
        }
    }

    fun occs(d: LocalDate): List<Occ> = _index.value[YearMonth.from(d)]?.get(d).orEmpty()

    fun toggleCalendar(id: Long) = viewModelScope.launch {
        val hide = id !in hidden.value
        app.calendarPrefs.setHidden(id, hide)
        if (hide) app.reminders.calendarHidden(id) else app.reminders.reseedFromCache()
    }

    val writable: List<CalendarDto> get() = calendars.value.filter { it.canWrite && it.id !in hidden.value }

    // ── Карточка и форма ──

    fun open(o: Occ?) {
        detail.value = o
    }

    /** Открыть событие по id из напоминания: ищем среди загруженного, иначе дочитываем месяц. */
    fun openById(eventId: Long, occStart: Long) {
        val d = Occ.dateOf(occStart, false, zone)
        select(d)
        if (view.value == CalView.Month || view.value == CalView.Year) setView(CalView.Day)
        val ym = YearMonth.from(d)
        viewModelScope.launch {
            if (!raw.containsKey(ym)) {
                fetch(ym)
                inflight[ym]?.join()
            }
            detail.value = occs(d).firstOrNull { it.ev.id == eventId && it.start == occStart }
                ?: occs(d).firstOrNull { it.ev.id == eventId }
        }
    }

    /** Звезда в списке календарей: повторное нажатие снимает. */
    fun setDefault(id: Long) = viewModelScope.launch {
        app.calendarPrefs.setDefaultCalendar(if (defaultCalendar.value == id) null else id)
    }

    fun newEvent(at: LocalDateTime? = null, allDay: Boolean = false) {
        // Только назначенный пользователем; иначе выбор в форме обязателен.
        val cal = writable.firstOrNull { it.id == defaultCalendar.value }
        val start = at ?: selected.value.atTime(LocalDateTime.now().hour, 0).plusHours(1)
        draft.value = EventDraft(
            original = null,
            calendarId = cal?.id ?: 0,
            allDay = allDay,
            start = start,
            end = if (allDay) start else start.plusHours(1),
            error = if (writable.isEmpty()) "Нет календарей, доступных на запись" else null,
        )
    }

    fun edit(o: Occ) {
        val zone = zone
        val s = if (o.allDay) Occ.dateOf(o.start, true, zone).atStartOfDay() else o.startTime(zone)
        // У «весь день» конец исключён — в форме показываем последний день включительно.
        val e = if (o.allDay) Occ.dateOf(o.end, true, zone).minusDays(1).coerceAtLeast(s.toLocalDate()).atStartOfDay() else o.endTime(zone)
        draft.value = EventDraft(
            original = o, calendarId = o.ev.calendarId, summary = o.ev.summary, location = o.ev.location,
            description = o.ev.description, allDay = o.allDay, start = s, end = e,
        )
        detail.value = null
    }

    fun editDraft(f: (EventDraft) -> EventDraft) = draft.update { it?.let { d -> f(d).copy(error = null) } }

    fun save() {
        val d = draft.value ?: return
        if (d.busy) return
        val summary = d.summary.trim()
        if (summary.isEmpty()) return draft.update { it?.copy(error = "Нужно название") }
        if (d.calendarId == 0L) return draft.update { it?.copy(error = "Выберите календарь") }
        val startMs: Long
        val endMs: Long
        if (d.allDay) {
            startMs = d.start.toLocalDate().atStartOfDay(zone).toInstant().toEpochMilli()
            endMs = d.end.toLocalDate().coerceAtLeast(d.start.toLocalDate()).plusDays(1).atStartOfDay(zone).toInstant().toEpochMilli()
        } else {
            startMs = d.start.atZone(zone).toInstant().toEpochMilli()
            endMs = d.end.atZone(zone).toInstant().toEpochMilli()
            if (endMs < startMs) return draft.update { it?.copy(error = "Конец раньше начала") }
        }
        draft.update { it?.copy(busy = true) }
        viewModelScope.launch {
            val ok = try {
                val o = d.original
                if (o == null) {
                    api.createEvent(EventCreateRequest(d.calendarId, summary, d.description, d.location, startMs, endMs, d.allDay))
                } else {
                    // Правка «всех» у повторяющегося сдвигает мастер на ту же разницу,
                    // что и вхождение: иначе серия переехала бы на дату вхождения.
                    val shift = if (o.recurring && d.scope == "all") o.ev.dtstart - o.start else 0L
                    api.patchEvent(
                        o.ev.id,
                        EventPatchRequest(
                            scope = if (o.recurring) d.scope else "all",
                            recurrenceId = if (o.recurring) o.start else 0,
                            summary = summary, description = d.description, location = d.location,
                            dtstart = startMs + shift, dtend = endMs + shift, allDay = d.allDay,
                        ),
                    )
                }
                true
            } catch (e: ApiException) {
                draft.update { it?.copy(busy = false, error = errorText(e)) }
                if (e is ApiException.Unauthorized) onDead()
                false
            }
            if (ok) {
                draft.value = null
                notice.value = if (d.original == null) "Событие создано" else "Сохранено"
                refresh()
            }
        }
    }

    /** Перенос драгом: та же длительность, новое начало. Повторяющиеся — только через форму. */
    fun move(o: Occ, newStart: Long, newEnd: Long) {
        if (!o.ev.editable || o.recurring) return
        viewModelScope.launch {
            guard {
                api.patchEvent(o.ev.id, EventPatchRequest(dtstart = newStart, dtend = newEnd))
                notice.value = "Перенесено"
            }
            refresh()
        }
    }

    fun delete(o: Occ) {
        viewModelScope.launch {
            guard {
                api.deleteEvent(o.ev.id)
                detail.value = null
                notice.value = if (o.recurring) "Серия удалена" else "Событие удалено"
            }
            refresh()
        }
    }

    fun rsvp(o: Occ, partstat: String) {
        viewModelScope.launch {
            guard {
                api.rsvp(o.ev.id, partstat)
                notice.value = when (partstat) {
                    "ACCEPTED" -> "Принято"
                    "DECLINED" -> "Отклонено"
                    else -> "Под вопросом"
                }
            }
            refresh()
        }
    }

    /** Мой ответ на приглашение, если я среди участников. */
    fun myPartstat(o: Occ): String? {
        val mine = myEmails.value + o.ev.identityEmail.lowercase()
        return o.ev.attendees.firstOrNull { it.email.lowercase() in mine }?.partstat?.ifEmpty { "NEEDS-ACTION" }
    }

    private suspend fun guard(block: suspend () -> Unit) {
        try {
            block()
        } catch (e: ApiException) {
            if (e is ApiException.Unauthorized) onDead() else notice.value = errorText(e)
        }
    }

    private fun errorText(e: ApiException): String = when (e) {
        is ApiException.Unauthorized -> "Нужно войти заново"
        is ApiException.Network -> "Нет связи с сервером"
        is ApiException.Server -> when {
            e.code == 403 -> "Календарь только для чтения"
            e.serverMessage.isNotBlank() -> "Сервер: ${e.serverMessage}"
            else -> "Ошибка сервера (${e.code})"
        }
    }
}
