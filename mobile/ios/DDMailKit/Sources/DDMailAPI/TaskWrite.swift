import Foundation

/// Задачи (VTODO) — только у сервера ddmail: внешних CalDAV-источников у
/// задач на клиенте нет. Протокол — чтобы экран задач проверялся на
/// подменённом API (как `TaskApi` Android). Ошибки — `APIError`.
public protocol TaskAPI: Sendable {
    func tasks(includeCompleted: Bool) async throws -> [TaskDTO]
    func setTaskCompleted(id: Int64, completed: Bool) async throws
    /// Коллекции пользователя; списки задач — с `CalendarDTO.acceptsTasks`.
    func calendars() async throws -> [CalendarDTO]
    /// Создаёт задачу; ответ — она же, как её отдаёт GET /tasks.
    func createTask(_ r: TaskCreateRequest) async throws -> TaskDTO
    func patchTask(id: Int64, _ r: TaskPatchRequest) async throws -> TaskDTO
    func deleteTask(id: Int64) async throws
}

/// Новая задача (POST /tasks). `due` — мс; при `allDay` — полночь UTC нужной даты.
public struct TaskCreateRequest: Encodable, Equatable, Sendable {
    public var calendar_id: Int64
    public var summary: String
    public var description: String
    public var due: Int64?
    public var all_day: Bool

    public init(calendarID: Int64, summary: String, description: String = "", due: Int64? = nil, allDay: Bool = false) {
        calendar_id = calendarID; self.summary = summary; self.description = description; self.due = due; all_day = allDay
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(calendar_id, forKey: .calendar_id)
        try c.encode(summary, forKey: .summary)
        try c.encode(description, forKey: .description)
        try c.encodeIfPresent(due, forKey: .due)
        try c.encode(all_day, forKey: .all_day)
    }

    enum CodingKeys: String, CodingKey { case calendar_id, summary, description, due, all_day }
}

/// Правка задачи (PATCH /tasks/{id}): `nil` — поле не трогать (в JSON не
/// пишется), `due = 0` — снять срок. Коллекцию правкой не меняют.
public struct TaskPatchRequest: Encodable, Equatable, Sendable {
    public var summary: String?
    public var description: String?
    public var due: Int64?
    public var all_day: Bool?

    public init(summary: String? = nil, description: String? = nil, due: Int64? = nil, allDay: Bool? = nil) {
        self.summary = summary; self.description = description; self.due = due; all_day = allDay
    }

    public var isEmpty: Bool { summary == nil && description == nil && due == nil && all_day == nil }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encodeIfPresent(summary, forKey: .summary)
        try c.encodeIfPresent(description, forKey: .description)
        try c.encodeIfPresent(due, forKey: .due)
        try c.encodeIfPresent(all_day, forKey: .all_day)
    }

    enum CodingKeys: String, CodingKey { case summary, description, due, all_day }
}

extension SessionAPI: TaskAPI {
    public func createTask(_ r: TaskCreateRequest) async throws -> TaskDTO {
        try decode(TaskDTO.self, from: try await send("POST", "/tasks", body: r))
    }

    public func patchTask(id: Int64, _ r: TaskPatchRequest) async throws -> TaskDTO {
        try decode(TaskDTO.self, from: try await send("PATCH", "/tasks/\(id)", body: r))
    }

    public func deleteTask(id: Int64) async throws {
        _ = try await authed {
            var r = URLRequest(url: url("/tasks/\(id)"))
            r.httpMethod = "DELETE"
            return r.bearer($0)
        }
    }
}
