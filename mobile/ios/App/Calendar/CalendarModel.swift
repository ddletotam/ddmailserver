import DDMailAPI
import DDMailCalendar
import DDMailDAV
import Foundation
import Observation

/// Локальные настройки календаря (Android §12): видимость — только на этом
/// устройстве (серверный `enabled` не трогаем), календарь и книга для новых
/// записей — только назначенные пользователем.
@MainActor @Observable
final class CalendarPrefs {
    static let shared = CalendarPrefs()
    private let d = UserDefaults.standard

    var hidden: Set<Int64> { didSet { d.set(hidden.map { NSNumber(value: $0) }, forKey: "cal.hidden") } }
    var defaultCalendar: Int64? { didSet { d.set(defaultCalendar.map { NSNumber(value: $0) }, forKey: "cal.default") } }
    var defaultAddressBook: Int64? { didSet { d.set(defaultAddressBook.map { NSNumber(value: $0) }, forKey: "cal.defaultBook") } }
    var reminders: Bool { didSet { d.set(reminders, forKey: "cal.reminders") } }
    var showCompleted: Bool { didSet { d.set(showCompleted, forKey: "cal.tasksDone") } }

    private init() {
        hidden = Set((d.array(forKey: "cal.hidden") as? [NSNumber] ?? []).map(\.int64Value))
        defaultCalendar = (d.object(forKey: "cal.default") as? NSNumber)?.int64Value
        defaultAddressBook = (d.object(forKey: "cal.defaultBook") as? NSNumber)?.int64Value
        reminders = d.object(forKey: "cal.reminders") as? Bool ?? true
        showCompleted = d.bool(forKey: "cal.tasksDone")
    }

    /// Выход из учётки: id календарей чужой сессии бессмысленны.
    func clear() {
        hidden = []
        defaultCalendar = nil
        defaultAddressBook = nil
    }
}

/// Черновик формы события.
struct EventDraft: Equatable {
    var original: Occ?
    var calendarID: Int64 = 0
    var summary = ""
    var location = ""
    var description = ""
    var allDay = false
    var start = Date()
    var end = Date()
    /// Охват правки повторяющегося: all | future.
    var scope = "all"
    var error: String?
    var busy = false
}

/// Календарь (порт `CalendarViewModel` Android): окна по месяцу, перечитывание
/// загруженного на возврат в приложение, по `calendar_updated` и после правки.
@MainActor @Observable
final class CalendarModel {
    enum ViewMode: String, CaseIterable, Identifiable {
        // Порядок переключателя — как в mobile-layout.md: «Месяц / Неделя / День»;
        // «Год» (iPad, Мак; Android §12) — перед ними, по убыванию охвата.
        case year, month, week, day
        var id: Self { self }
        var title: String {
            switch self {
            case .day: "День"
            case .week: "Неделя"
            case .month: "Месяц"
            case .year: "Год"
            }
        }
    }

    /// Перенос повторяющегося вхождения ждёт выбора охвата — как в форме
    /// («Все повторы» / «Это и следующие»).
    struct SeriesMove: Identifiable {
        let occ: Occ
        let start: Int64
        let end: Int64
        var id: String { occ.id }
    }

    /// ddmail и CalDAV внешних учёток вместе (внешние учётки §8).
    let api: any CalendarSource
    let prefs = CalendarPrefs.shared
    private let onDead: () -> Void
    var cal: Calendar = {
        var c = Calendar.current
        c.firstWeekday = 2
        return c
    }()

    private(set) var calendars: [CalendarDTO] = []
    private var raw: [Month: [EventDTO]] = [:]
    private(set) var index: [Month: [Day: [Occ]]] = [:]
    private var inflight: [Month: Task<Void, Never>] = [:]
    private(set) var loading = false
    /// Все загруженные события (для напоминаний).
    var allEvents: [EventDTO] { raw.values.flatMap { $0 } }

    var mode: ViewMode = .month
    var selected: Day
    var detail: Occ?
    var draft: EventDraft?
    var seriesMove: SeriesMove?
    var notice: String?
    var myEmails: Set<String> = []
    /// Вызывается после перечитывания: напоминания пересеваются.
    var onReloaded: (() -> Void)?

    init(api: any CalendarSource, onDead: @escaping () -> Void) {
        self.api = api
        self.onDead = onDead
        var c = Calendar.current
        c.firstWeekday = 2
        selected = Day(Date(), c)
    }

    var today: Day { Day(Date(), cal) }
    var writable: [CalendarDTO] { calendars.filter { $0.canWrite && !prefs.hidden.contains($0.id) } }

    func calendar(_ id: Int64) -> CalendarDTO? { calendars.first { $0.id == id } }

    func occs(_ d: Day) -> [Occ] { index[d.month1]?[d] ?? [] }

    func select(_ d: Day) {
        selected = d
        ensure(d.month1)
        ensure(d.adding(-7, cal).month1)
        ensure(d.adding(7, cal).month1)
    }

    // MARK: Загрузка

    func start() async {
        await loadCalendars()
        select(selected)
    }

    private func guarded(_ body: () async throws -> Void) async {
        do {
            try await body()
        } catch APIError.unauthorized {
            onDead()
        } catch is CancellationError {
        } catch {
            notice = error.localizedDescription
        }
    }

    private func loadCalendars() async {
        await guarded { calendars = try await api.calendars().sorted { $0.name.lowercased() < $1.name.lowercased() } }
        reindex()
    }

    /// Нужны события месяца: свежий запрос, если его ещё нет.
    func ensure(_ m: Month) {
        guard raw[m] == nil, inflight[m] == nil else { return }
        fetch(m)
    }

    private func fetch(_ m: Month) {
        inflight[m]?.cancel()
        loading = true
        inflight[m] = Task {
            let from = ms(m.first.start(cal)), to = ms(m.adding(1).first.start(cal))
            // Запас на сутки с каждой стороны: даты «весь день» лежат в полночь UTC.
            await guarded {
                raw[m] = try await api.calendarEvents(fromMs: from - 86_400_000, toMs: to + 86_400_000)
                reindex(m)
            }
            inflight[m] = nil
            loading = !inflight.isEmpty
            if inflight.isEmpty { onReloaded?() }
        }
    }

    /// Сервер сообщил об изменениях / вернулись в приложение: перечитать загруженное.
    func refresh() async {
        await loadCalendars()
        for m in raw.keys { fetch(m) }
        if raw.isEmpty { select(selected) }
    }

    func reindex(_ only: Month? = nil) {
        let cals = Dictionary(calendars.map { ($0.id, $0) }, uniquingKeysWith: { a, _ in a })
        if let only {
            index[only] = indexMonth(only, events: raw[only] ?? [], calendars: cals, hidden: prefs.hidden, calendar: cal)
        } else {
            index = raw.reduce(into: [:]) { $0[$1.key] = indexMonth($1.key, events: $1.value, calendars: cals, hidden: prefs.hidden, calendar: cal) }
        }
    }

    func toggleCalendar(_ id: Int64) {
        if prefs.hidden.contains(id) { prefs.hidden.remove(id) } else { prefs.hidden.insert(id) }
        reindex()
        onReloaded?()
    }

    /// Звезда в списке календарей: повторное нажатие снимает.
    func setDefault(_ id: Int64) {
        prefs.defaultCalendar = prefs.defaultCalendar == id ? nil : id
    }

    // MARK: Карточка и форма

    /// Открыть событие по id (из напоминания): среди загруженного, иначе дочитать месяц.
    func open(eventID: Int64, occStart: Int64) {
        let d = Occ.dateOf(occStart, allDay: false, cal)
        mode = .day
        select(d)
        Task {
            await inflight[d.month1]?.value
            detail = occs(d).first { $0.event.id == eventID && $0.start == occStart } ?? occs(d).first { $0.event.id == eventID }
        }
    }

    func newEvent(at: Date? = nil, allDay: Bool = false) {
        // Только назначенный пользователем календарь; иначе выбор в форме обязателен.
        let calID = writable.first { $0.id == prefs.defaultCalendar }?.id ?? 0
        let hour = cal.component(.hour, from: Date())
        let start = at ?? cal.date(bySettingHour: min(hour + 1, 23), minute: 0, second: 0, of: selected.start(cal)) ?? Date()
        draft = EventDraft(
            calendarID: calID, allDay: allDay, start: start,
            end: allDay ? start : start.addingTimeInterval(3600),
            error: writable.isEmpty ? "Нет календарей, доступных на запись" : nil
        )
    }

    func edit(_ o: Occ) {
        let s = o.allDay ? Occ.dateOf(o.start, allDay: true, cal).start(cal) : date(o.start)
        // У «весь день» конец исключён — в форме последний день включительно.
        var e = o.allDay ? Occ.dateOf(o.end, allDay: true, cal).adding(-1, cal).start(cal) : date(o.end)
        if e < s { e = s }
        draft = EventDraft(
            original: o, calendarID: o.event.calendarID, summary: o.event.summary, location: o.event.location,
            description: o.event.description, allDay: o.allDay, start: s, end: e
        )
        detail = nil
    }

    func save() {
        guard var d = draft, !d.busy else { return }
        let summary = d.summary.trimmingCharacters(in: .whitespaces)
        if summary.isEmpty { draft?.error = "Нужно название"; return }
        if d.calendarID == 0 { draft?.error = "Выберите календарь"; return }
        let startMs: Int64, endMs: Int64
        if d.allDay {
            let sd = Day(d.start, cal), ed = max(Day(d.end, cal), sd)
            startMs = ms(sd.start(cal))
            endMs = ms(ed.adding(1, cal).start(cal))
        } else {
            startMs = ms(d.start)
            endMs = ms(d.end)
            if endMs < startMs { draft?.error = "Конец раньше начала"; return }
        }
        d.busy = true
        d.error = nil
        draft = d
        Task {
            do {
                if let o = d.original {
                    // Правка «всех» у повторяющегося сдвигает мастер на ту же разницу,
                    // что и вхождение: иначе серия переехала бы на дату вхождения.
                    let shift = o.recurring && d.scope == "all" ? o.event.dtstart - o.start : 0
                    try await api.patchEvent(id: o.event.id, EventPatchRequest(
                        scope: o.recurring ? d.scope : "all", recurrenceID: o.recurring ? o.start : 0,
                        summary: summary, description: d.description, location: d.location,
                        dtstart: startMs + shift, dtend: endMs + shift, allDay: d.allDay
                    ))
                } else {
                    try await api.createEvent(EventCreateRequest(
                        calendarID: d.calendarID, summary: summary, description: d.description,
                        location: d.location, dtstart: startMs, dtend: endMs, allDay: d.allDay
                    ))
                }
                draft = nil
                notice = d.original == nil ? "Событие создано" : "Сохранено"
                await refresh()
            } catch {
                if case APIError.unauthorized = error { onDead() }
                // Форма закрывается по подтверждению, отказ — текстом в ней (§5б-бис).
                draft?.busy = false
                draft?.error = error.localizedDescription
            }
        }
    }

    /// Перенос или растяжение на шкале (Android §12, десктоп §5и; шаг и
    /// кромки — `EventDrag`). Разовое событие сразу встаёт на новое место и
    /// уходит на сервер; отказ — причина плашкой, перечитывание вернёт как
    /// было. Повторяющееся — сперва выбор охвата, как в форме.
    func drop(_ o: Occ, _ kind: EventDrag.Kind, minutes: Int, days: Int) {
        guard o.event.editable, !o.allDay, minutes != 0 || days != 0 else { return }
        let r = EventDrag.apply(kind, start: o.start, end: o.end, minutes: minutes, days: kind == .move ? days : 0, calendar: cal)
        guard r.start != o.start || r.end != o.end else { return }
        if o.recurring {
            seriesMove = SeriesMove(occ: o, start: r.start, end: r.end)
            return
        }
        placeLocally(o.event.id, start: r.start, end: r.end)
        patchTimes(o, scope: "all", start: r.start, end: r.end, done: kind == .move ? "Перенесено" : "Время изменено")
    }

    /// Ответ на вопрос об охвате переноса повторяющегося: `all` сдвигает
    /// мастер серии на ту же разницу, что и вхождение (как `save`); `future` —
    /// это вхождение и следующие.
    func confirmSeriesMove(scope: String) {
        guard let m = seriesMove else { return }
        seriesMove = nil
        let shift = scope == "all" ? m.occ.event.dtstart - m.occ.start : 0
        if scope == "all" {
            // Мастер на новом месте сразу; развёртку пересчитает индекс.
            placeLocally(m.occ.event.id, start: m.start + shift, end: m.end + shift)
        }
        patchTimes(m.occ, scope: scope, start: m.start + shift, end: m.end + shift, done: "Перенесено")
    }

    private func patchTimes(_ o: Occ, scope: String, start: Int64, end: Int64, done: String) {
        Task {
            await guarded {
                try await api.patchEvent(id: o.event.id, EventPatchRequest(
                    scope: o.recurring ? scope : "all", recurrenceID: o.recurring ? o.start : 0, dtstart: start, dtend: end
                ))
                notice = done
            }
            await refresh()
        }
    }

    /// Новое время события в загруженных месяцах — до ответа сервера.
    /// Событие кладётся во все месяцы: индекс месяца берёт только свои дни.
    private func placeLocally(_ eventID: Int64, start: Int64, end: Int64) {
        guard var ev = raw.values.lazy.flatMap({ $0 }).first(where: { $0.id == eventID }) else { return }
        ev.dtstart = start
        ev.dtend = end
        for m in raw.keys {
            raw[m]?.removeAll { $0.id == eventID }
            raw[m]?.append(ev)
        }
        reindex()
    }

    func delete(_ o: Occ) {
        Task {
            await guarded {
                try await api.deleteEvent(id: o.event.id)
                detail = nil
                notice = o.recurring ? "Серия удалена" : "Событие удалено"
            }
            await refresh()
        }
    }

    func rsvp(_ o: Occ, _ partstat: String) {
        Task {
            await guarded {
                try await api.rsvp(id: o.event.id, partstat: partstat)
                notice = partstat == "ACCEPTED" ? "Принято" : partstat == "DECLINED" ? "Отклонено" : "Под вопросом"
            }
            await refresh()
        }
    }

    /// Мой ответ на приглашение, если я среди участников.
    func myPartstat(_ o: Occ) -> String? {
        let mine = myEmails.union([o.event.identityEmail.lowercased()])
        guard let a = o.event.attendees.first(where: { mine.contains($0.email.lowercased()) }) else { return nil }
        return a.partstat.isEmpty ? "NEEDS-ACTION" : a.partstat
    }
}
