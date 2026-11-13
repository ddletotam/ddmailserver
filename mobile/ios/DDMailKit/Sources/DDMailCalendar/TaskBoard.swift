import DDMailAPI
import Foundation
import Observation

/// Форма задачи: новая (`original == nil`) или правка. Срок — дата, а со
/// временем — момент в поясе устройства. «На весь день» уходит на сервер
/// полуночью UTC даты — так такие даты хранит сервер.
public struct TaskDraft: Equatable, Sendable {
    public var original: TaskDTO?
    public var summary: String
    public var description: String
    /// Список задач новой задачи; 0 — списков нет (сохранить нельзя).
    public var calendarID: Int64
    public var hasDue: Bool
    /// Дата срока (и время, если не `allDay`) — по часам устройства.
    public var due: Date
    public var allDay: Bool
    public var error: String?

    public init(original: TaskDTO? = nil, summary: String = "", description: String = "", calendarID: Int64 = 0,
                hasDue: Bool = false, due: Date, allDay: Bool = true, error: String? = nil) {
        self.original = original; self.summary = summary; self.description = description; self.calendarID = calendarID
        self.hasDue = hasDue; self.due = due; self.allDay = allDay; self.error = error
    }

    /// Задача из списка только для чтения: форма показывает, но не правит.
    public var readOnly: Bool { original.map { !$0.canWrite } ?? false }

    /// Новая задача: первый список по умолчанию, срок — сегодня в 9:00, выключен.
    public static func new(lists: [CalendarDTO], now: Date = Date(), calendar: Calendar = .current) -> TaskDraft {
        TaskDraft(calendarID: lists.first?.id ?? 0, due: nineOClock(of: now, calendar))
    }

    public static func of(_ t: TaskDTO, calendar: Calendar = .current) -> TaskDraft {
        var due = nineOClock(of: Date(), calendar)
        if let ms = t.due {
            let instant = Date(timeIntervalSince1970: TimeInterval(ms) / 1000)
            if t.allDay {
                // Дата «на весь день» — в UTC; переносим её на часы устройства.
                var utc = Calendar(identifier: .gregorian)
                utc.timeZone = TimeZone(identifier: "UTC")!
                let d = utc.dateComponents([.year, .month, .day], from: instant)
                due = calendar.date(from: DateComponents(year: d.year, month: d.month, day: d.day, hour: 9)) ?? instant
            } else {
                due = instant
            }
        }
        return TaskDraft(original: t, summary: t.summary, description: t.description, calendarID: t.calendarID,
                         hasDue: t.due != nil, due: due, allDay: t.due == nil || t.allDay)
    }

    /// Срок в мс, как его ждёт сервер; nil — без срока.
    public func dueMs(calendar: Calendar = .current) -> Int64? {
        guard hasDue else { return nil }
        if allDay {
            let d = calendar.dateComponents([.year, .month, .day], from: due)
            var utc = Calendar(identifier: .gregorian)
            utc.timeZone = TimeZone(identifier: "UTC")!
            guard let midnight = utc.date(from: DateComponents(year: d.year, month: d.month, day: d.day)) else { return nil }
            return Int64(midnight.timeIntervalSince1970) * 1000
        }
        // С точностью до минуты: выбор времени секунд не показывает.
        let s = Int64(due.timeIntervalSince1970.rounded(.down))
        return (s - s % 60) * 1000
    }

    static func nineOClock(of day: Date, _ calendar: Calendar) -> Date {
        calendar.date(bySettingHour: 9, minute: 0, second: 0, of: day) ?? day
    }
}

/// Список задач и его правки (Android §14, iOS §11): галочка, создание,
/// правка и удаление — оптимистичны. Строка меняется сразу, сервер узнаёт
/// следом; отказ возвращает строку как была и оставляет сообщение в `notice`.
@MainActor @Observable
public final class TaskBoard {
    public private(set) var tasks: [TaskDTO] = []
    /// «Ещё грузится» и «правда пусто» — разные состояния.
    public private(set) var loaded = false
    /// Списки, куда можно создать задачу; первый — по умолчанию.
    public private(set) var lists: [CalendarDTO] = []
    public var notice: String?

    private let api: any TaskAPI
    private let onDead: () -> Void
    private var tempIDs: Int64 = 0

    public init(api: any TaskAPI, onDead: @escaping () -> Void) {
        self.api = api
        self.onDead = onDead
    }

    public func load(includeCompleted: Bool) async {
        do {
            tasks = try await api.tasks(includeCompleted: includeCompleted)
            loaded = true
        } catch is CancellationError {
        } catch {
            fail(error, "Не удалось загрузить задачи")
        }
    }

    public func loadLists() async {
        do {
            lists = try await api.calendars().filter(\.acceptsTasks)
        } catch APIError.unauthorized {
            onDead()
        } catch {}
    }

    public func toggle(_ t: TaskDTO) async {
        guard t.canWrite, t.id > 0 else { return }
        let want = !t.completed
        replace(t.id) { $0.completed = want }
        do {
            try await api.setTaskCompleted(id: t.id, completed: want)
        } catch {
            replace(t.id) { $0.completed = t.completed }
            fail(error, "Не удалось сохранить")
        }
    }

    /// Проверка формы до отправки: текст ошибки для формы или nil — можно
    /// сохранять (форма закрывается сразу, дальше — оптимистично).
    public func validate(_ d: TaskDraft) -> String? {
        if d.readOnly { return "Список только для чтения" }
        if d.summary.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { return "Нужен заголовок" }
        if d.original == nil && d.calendarID == 0 { return "Нет списка задач, куда можно записать" }
        return nil
    }

    /// Сохраняет форму; к этому моменту `validate` уже вернул nil.
    public func save(_ d: TaskDraft, calendar: Calendar = .current) async {
        if let orig = d.original {
            await update(orig, d, calendar)
        } else {
            await create(d, calendar)
        }
    }

    private func create(_ d: TaskDraft, _ calendar: Calendar) async {
        let list = lists.first { $0.id == d.calendarID }
        let due = d.dueMs(calendar: calendar)
        tempIDs += 1
        // Временный id — отрицательный: сервер раздаёт только положительные.
        let temp = TaskDTO(
            id: -tempIDs, calendarID: d.calendarID, summary: d.summary.trimmingCharacters(in: .whitespacesAndNewlines),
            description: d.description, due: due, allDay: due != nil && d.allDay, color: list?.color ?? "",
            canWrite: true, calendarName: list?.name ?? ""
        )
        tasks.append(temp)
        do {
            let created = try await api.createTask(TaskCreateRequest(
                calendarID: d.calendarID, summary: temp.summary, description: d.description, due: due, allDay: temp.allDay
            ))
            if let i = tasks.firstIndex(where: { $0.id == temp.id }) { tasks[i] = created }
        } catch {
            tasks.removeAll { $0.id == temp.id }
            fail(error, "Задача не создана")
        }
    }

    private func update(_ orig: TaskDTO, _ d: TaskDraft, _ calendar: Calendar) async {
        let req = Self.patch(orig, d, calendar: calendar)
        if req.isEmpty { return }
        replace(orig.id) { t in
            if let s = req.summary { t.summary = s }
            if let s = req.description { t.description = s }
            if let due = req.due {
                t.due = due == 0 ? nil : due
                t.allDay = due != 0 && (req.all_day ?? t.allDay)
            }
        }
        do {
            let saved = try await api.patchTask(id: orig.id, req)
            replace(orig.id) { $0 = saved }
        } catch {
            replace(orig.id) { $0 = orig }
            fail(error, "Изменения не сохранены")
        }
    }

    public func delete(_ t: TaskDTO) async {
        guard t.canWrite, t.id > 0, let at = tasks.firstIndex(where: { $0.id == t.id }) else { return }
        tasks.remove(at: at)
        do {
            try await api.deleteTask(id: t.id)
        } catch {
            // Назад на прежнее место, а не в конец.
            if !tasks.contains(where: { $0.id == t.id }) { tasks.insert(t, at: min(at, tasks.count)) }
            fail(error, "Задача не удалена")
        }
    }

    /// Убрать из списка отмеченную, когда выполненные скрыты.
    public func dropIfDone(_ id: Int64) {
        tasks.removeAll { $0.id == id && $0.completed }
    }

    private func replace(_ id: Int64, _ f: (inout TaskDTO) -> Void) {
        guard let i = tasks.firstIndex(where: { $0.id == id }) else { return }
        f(&tasks[i])
    }

    private func fail(_ error: Error, _ what: String) {
        if case APIError.unauthorized = error {
            onDead()
            return
        }
        switch error {
        case APIError.server(403, _): notice = "\(what): список только для чтения"
        case APIError.server(422, _): notice = "\(what): этот список не принимает задачи"
        case APIError.network: notice = "\(what): нет связи с сервером"
        default: notice = what
        }
    }

    /// Правка — только изменённые поля: срок, которого не трогали, не
    /// переписывается (у задачи Apple он может быть в своём поясе, и
    /// пересохранение в UTC сдвинуло бы его в чужом клиенте).
    public nonisolated static func patch(_ orig: TaskDTO, _ d: TaskDraft, calendar: Calendar = .current) -> TaskPatchRequest {
        let summary = d.summary.trimmingCharacters(in: .whitespacesAndNewlines)
        let due = d.dueMs(calendar: calendar)
        let dueChanged = due != orig.due || (due != nil && d.allDay != orig.allDay)
        return TaskPatchRequest(
            summary: summary == orig.summary ? nil : summary,
            description: d.description == orig.description ? nil : d.description,
            due: dueChanged ? (due ?? 0) : nil,
            allDay: dueChanged && due != nil ? d.allDay : nil
        )
    }
}
