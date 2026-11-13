import DDMailAPI
import Foundation
import Testing
@testable import DDMailCalendar

/// Фальшивый сервер задач: ответы — сразу или по сигналу `release()`, отказ — `fail`.
final class FakeTaskAPI: TaskAPI, @unchecked Sendable {
    private let lock = NSLock()
    private var _fail: APIError?
    private var _calls: [String] = []
    private var _lastCreate: TaskCreateRequest?
    private var _lastPatch: TaskPatchRequest?
    private var gate: CheckedContinuation<Void, Never>?
    private var holding = false
    var server: [TaskDTO] = [
        TaskDTO(id: 1, calendarID: 10, summary: "Оплатить офис", canWrite: true, calendarName: "Напоминания"),
        TaskDTO(id: 2, calendarID: 30, summary: "Чужое", canWrite: false),
    ]

    var fail: APIError? {
        get { lock.withLock { _fail } }
        set { lock.withLock { _fail = newValue } }
    }
    var calls: [String] { lock.withLock { _calls } }
    var lastCreate: TaskCreateRequest? { lock.withLock { _lastCreate } }
    var lastPatch: TaskPatchRequest? { lock.withLock { _lastPatch } }

    /// Следующие вызовы ждут `release()`.
    func hold() { lock.withLock { holding = true } }

    func release() {
        let c: CheckedContinuation<Void, Never>? = lock.withLock {
            holding = false
            defer { gate = nil }
            return gate
        }
        c?.resume()
    }

    private func step(_ call: String) async throws {
        let wait = lock.withLock { () -> Bool in
            _calls.append(call)
            return holding
        }
        if wait {
            await withCheckedContinuation { c in lock.withLock { gate = c } }
        }
        if let f = fail { throw f }
    }

    func tasks(includeCompleted: Bool) async throws -> [TaskDTO] {
        try await step("tasks")
        return server
    }

    func setTaskCompleted(id: Int64, completed: Bool) async throws { try await step("done \(id) \(completed)") }

    func calendars() async throws -> [CalendarDTO] {
        try await step("calendars")
        return [
            CalendarDTO(id: 10, name: "Напоминания", sourceType: "caldav", canWrite: true, components: ["VTODO"]),
            CalendarDTO(id: 20, name: "Дом", sourceType: "caldav", canWrite: true, components: ["VEVENT"]),
            CalendarDTO(id: 30, name: "Подписка", sourceType: "ics_url", canWrite: true, components: ["VTODO"]),
            CalendarDTO(id: 40, name: "Работа", sourceType: "caldav", canWrite: true, components: ["VEVENT", "VTODO"]),
            CalendarDTO(id: 50, name: "Старый сервер", sourceType: "caldav", canWrite: true),
        ]
    }

    func createTask(_ r: TaskCreateRequest) async throws -> TaskDTO {
        lock.withLock { _lastCreate = r }
        try await step("create")
        return TaskDTO(id: 500, calendarID: r.calendar_id, summary: r.summary, description: r.description,
                       due: r.due, allDay: r.all_day, canWrite: true)
    }

    func patchTask(id: Int64, _ r: TaskPatchRequest) async throws -> TaskDTO {
        lock.withLock { _lastPatch = r }
        try await step("patch \(id)")
        var t = server.first { $0.id == id }!
        if let s = r.summary { t.summary = s }
        return t
    }

    func deleteTask(id: Int64) async throws { try await step("delete \(id)") }
}

private func moscow() -> Calendar {
    var c = Calendar(identifier: .gregorian)
    c.timeZone = TimeZone(identifier: "Europe/Moscow")!
    return c
}

private func at(_ y: Int, _ m: Int, _ d: Int, _ h: Int = 0, _ min: Int = 0) -> Date {
    moscow().date(from: DateComponents(year: y, month: m, day: d, hour: h, minute: min))!
}

/// Ждёт (до двух секунд), пока фоновая задача дойдёт до вызова API.
@MainActor
private func until(_ cond: () -> Bool) async {
    for _ in 0..<2000 where !cond() { try? await Task.sleep(for: .milliseconds(1)) }
}

@MainActor
private final class Flag { var on = false }

@MainActor
struct TaskBoardTests {
    @Test func taskListsAreWritableVtodoCollections() async {
        let b = TaskBoard(api: FakeTaskAPI()) {}
        await b.loadLists()
        #expect(b.lists.map(\.id) == [10, 40])
        #expect(TaskDraft.new(lists: b.lists).calendarID == 10)
        #expect(TaskDraft.new(lists: []).calendarID == 0)
    }

    @Test func validateNeedsTitleAndList() {
        let b = TaskBoard(api: FakeTaskAPI()) {}
        let now = Date()
        #expect(b.validate(TaskDraft(summary: "  ", calendarID: 10, due: now)) == "Нужен заголовок")
        #expect(b.validate(TaskDraft(summary: "x", calendarID: 0, due: now)) == "Нет списка задач, куда можно записать")
        #expect(b.validate(TaskDraft(summary: "x", calendarID: 10, due: now)) == nil)
        #expect(b.validate(TaskDraft.of(TaskDTO(id: 2, summary: "Чужое", canWrite: false))) == "Список только для чтения")
    }

    @Test func dueIsUTCMidnightForDatesAndWallClockForTimes() {
        var d = TaskDraft(summary: "x", hasDue: true, due: at(2026, 11, 13, 23, 30), allDay: true)
        #expect(d.dueMs(calendar: moscow()) == 1_794_528_000_000) // 2026-11-13T00:00Z, а не полночь Москвы
        d.allDay = false
        d.due = at(2026, 11, 13, 12, 30)
        #expect(d.dueMs(calendar: moscow()) == 1_794_562_200_000) // 09:30Z
        d.hasDue = false
        #expect(d.dueMs(calendar: moscow()) == nil)

        // И обратно: дата «на весь день» встаёт на тот же день по часам устройства.
        let back = TaskDraft.of(TaskDTO(id: 1, due: 1_794_528_000_000, allDay: true, canWrite: true), calendar: moscow())
        #expect(moscow().dateComponents([.year, .month, .day], from: back.due) == DateComponents(year: 2026, month: 11, day: 13))
        #expect(back.dueMs(calendar: moscow()) == 1_794_528_000_000)
    }

    @Test func createShowsRowAtOnceThenSwapsInServerTask() async {
        let api = FakeTaskAPI()
        let b = TaskBoard(api: api) {}
        await b.load(includeCompleted: false)
        await b.loadLists()
        var d = TaskDraft.new(lists: b.lists)
        d.summary = " Купить хлеб "
        d.hasDue = true
        d.due = at(2026, 11, 13, 9)
        api.hold()
        let job = Task { await b.save(d, calendar: moscow()) }
        await until { api.calls.contains("create") }

        let pending = b.tasks.last!
        #expect(pending.id < 0)
        #expect(pending.summary == "Купить хлеб")
        #expect(pending.calendarName == "Напоминания")

        api.release()
        await job.value
        #expect(b.tasks.last?.id == 500)
        #expect(b.tasks.count == 3)
        #expect(api.lastCreate == TaskCreateRequest(calendarID: 10, summary: "Купить хлеб", due: 1_794_528_000_000, allDay: true))
        #expect(b.notice == nil)
    }

    @Test func createRefusedRemovesRowAndTellsWhy() async {
        let api = FakeTaskAPI()
        let b = TaskBoard(api: api) {}
        await b.load(includeCompleted: false)
        api.fail = .server(code: 403, message: "calendar is read-only")
        await b.save(TaskDraft(summary: "x", calendarID: 10, due: Date()))
        #expect(b.tasks.map(\.id) == [1, 2])
        #expect(b.notice == "Задача не создана: список только для чтения")
    }

    @Test func editSendsOnlyChangedFieldsAndRollsBack() async {
        let api = FakeTaskAPI()
        let b = TaskBoard(api: api) {}
        await b.load(includeCompleted: false)
        let orig = b.tasks[0]
        var d = TaskDraft.of(orig, calendar: moscow())
        d.summary = "Оплатить офис и склад"
        #expect(TaskBoard.patch(orig, d, calendar: moscow()) == TaskPatchRequest(summary: "Оплатить офис и склад"))

        api.hold()
        let job = Task { await b.save(d, calendar: moscow()) }
        await until { api.calls.contains("patch 1") }
        #expect(b.tasks[0].summary == "Оплатить офис и склад")
        api.fail = .network("timeout")
        api.release()
        await job.value
        #expect(b.tasks[0].summary == "Оплатить офис")
        #expect(b.notice == "Изменения не сохранены: нет связи с сервером")
    }

    @Test func unchangedFormSendsNothing() async {
        let api = FakeTaskAPI()
        let b = TaskBoard(api: api) {}
        await b.load(includeCompleted: false)
        await b.save(TaskDraft.of(b.tasks[0]))
        #expect(!api.calls.contains { $0.hasPrefix("patch") })
    }

    @Test func clearingDueSendsZero() {
        let orig = TaskDTO(id: 1, summary: "x", due: 1_794_528_000_000, allDay: true, canWrite: true)
        var d = TaskDraft.of(orig, calendar: moscow())
        d.hasDue = false
        #expect(TaskBoard.patch(orig, d, calendar: moscow()) == TaskPatchRequest(due: 0))
        d.hasDue = true
        d.allDay = false
        d.due = at(2026, 11, 13, 12, 30)
        #expect(TaskBoard.patch(orig, d, calendar: moscow()) == TaskPatchRequest(due: 1_794_562_200_000, allDay: false))
    }

    @Test func deleteRemovesAtOnceAndRestoresInPlace() async {
        let api = FakeTaskAPI()
        let b = TaskBoard(api: api) {}
        await b.load(includeCompleted: false)
        let first = b.tasks[0]
        api.hold()
        let job = Task { await b.delete(first) }
        await until { api.calls.contains("delete 1") }
        #expect(b.tasks.map(\.id) == [2])
        api.fail = .server(code: 500, message: "boom")
        api.release()
        await job.value
        #expect(b.tasks.map(\.id) == [1, 2])
        #expect(b.notice == "Задача не удалена")
    }

    @Test func readOnlyRowsAreNotTouched() async {
        let api = FakeTaskAPI()
        let b = TaskBoard(api: api) {}
        await b.load(includeCompleted: false)
        await b.toggle(b.tasks[1])
        await b.delete(b.tasks[1])
        #expect(api.calls == ["tasks"])
    }

    @Test func deadSessionLogsOutInsteadOfNotice() async {
        let api = FakeTaskAPI()
        let dead = Flag()
        let b = TaskBoard(api: api) { dead.on = true }
        await b.load(includeCompleted: false)
        api.fail = .unauthorized("expired")
        await b.toggle(b.tasks[0])
        #expect(dead.on)
        #expect(b.notice == nil)
        #expect(b.tasks[0].completed == false)
    }
}
