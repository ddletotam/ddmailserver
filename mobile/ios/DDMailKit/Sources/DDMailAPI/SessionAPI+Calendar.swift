import Foundation

/// Календарь, задачи, контакты — эндпоинты как у Android (`SessionApi.kt`).
extension SessionAPI {
    public func calendars() async throws -> [CalendarDTO] {
        try await get("/calendars", as: [CalendarDTO].self)
    }

    /// События окна `[fromMs, toMs)`; повторяющиеся — мастерами (развёртывает клиент).
    public func calendarEvents(fromMs: Int64, toMs: Int64) async throws -> [EventDTO] {
        try await get("/calendar-events?from=\(fromMs)&to=\(toMs)", as: [EventDTO].self)
    }

    public func createEvent(_ r: EventCreateRequest) async throws {
        try await send("POST", "/events", body: r)
    }

    public func patchEvent(id: Int64, _ r: EventPatchRequest) async throws {
        try await send("PATCH", "/events/\(id)", body: r)
    }

    public func deleteEvent(id: Int64) async throws {
        _ = try await authed {
            var r = URLRequest(url: url("/events/\(id)"))
            r.httpMethod = "DELETE"
            return r.bearer($0)
        }
    }

    /// Ответ на приглашение: ACCEPTED | TENTATIVE | DECLINED.
    public func rsvp(id: Int64, partstat: String) async throws {
        struct Body: Encodable { let partstat: String }
        try await send("POST", "/events/\(id)/rsvp", body: Body(partstat: partstat))
    }

    public func tasks(includeCompleted: Bool) async throws -> [TaskDTO] {
        try await get("/tasks" + (includeCompleted ? "?include_completed=1" : ""), as: [TaskDTO].self)
    }

    public func setTaskCompleted(id: Int64, completed: Bool) async throws {
        struct Body: Encodable { let completed: Bool }
        try await send("POST", "/tasks/\(id)/completion", body: Body(completed: completed))
    }

    public func contacts(limit: Int = 5000) async throws -> [ContactDTO] {
        try await get("/contacts?limit=\(limit)", as: [ContactDTO].self)
    }

    public func addressBooks() async throws -> [AddressBookDTO] {
        try await get("/address-books", as: [AddressBookDTO].self)
    }

    public func createContact(_ r: ContactWriteRequest) async throws {
        try await send("POST", "/contacts", body: r)
    }

    public func patchContact(id: Int64, _ r: ContactWriteRequest) async throws {
        try await send("PATCH", "/contacts/\(id)", body: r)
    }

    public func deleteContact(id: Int64) async throws {
        _ = try await authed {
            var r = URLRequest(url: url("/contacts/\(id)"))
            r.httpMethod = "DELETE"
            return r.bearer($0)
        }
    }
}
