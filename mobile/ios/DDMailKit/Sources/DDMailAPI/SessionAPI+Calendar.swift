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

    /// Страница `/contacts` — столько строк сервер отдаёт за раз (больше он режет).
    public static let contactsPage = 5000

    /// Сводная книга целиком, страницами по `contactsPage` (`offset`; аудит
    /// 2026-11-14 №5: у владельца 15 000 карточек, одна страница кончалась
    /// на «Ару…», и новый контакт не был виден). Короткая страница —
    /// последняя. Сервер, не знающий `offset`, отдаёт ту же первую страницу —
    /// её узнаём по повтору id и останавливаемся (`truncated`).
    public func contacts(limit: Int = .max) async throws -> [ContactDTO] {
        try await contactPages(limit: limit).contacts
    }

    /// Книга страницами; `truncated` — сервер отдал не всё (страницу без `offset`).
    public func contactPages(limit: Int = .max) async throws -> (contacts: [ContactDTO], truncated: Bool) {
        let page = Self.contactsPage
        var out: [ContactDTO] = []
        var seen = Set<Int64>()
        while out.count < limit {
            let got = try await get("/contacts?limit=\(page)&offset=\(out.count)", as: [ContactDTO].self)
            let fresh = got.filter { seen.insert($0.id).inserted }
            if fresh.count < got.count { return (out, true) }
            out += fresh
            if got.count < page { break }
        }
        return (Array(out.prefix(limit)), false)
    }

    /// Поиск по всей книге на сервере (`/contacts/search`, до 100): имя,
    /// адрес, телефон, организация — и среди того, что в список не попало.
    public func searchContacts(_ query: String, limit: Int = 100) async throws -> [ContactDTO] {
        try await get("/contacts/search?q=\(queryEscaped(query))&limit=\(limit)", as: [ContactDTO].self)
    }

    public func addressBooks() async throws -> [AddressBookDTO] {
        try await get("/address-books", as: [AddressBookDTO].self)
    }

    /// Новый контакт; ответ — его id (`{"id":…,"uid":…}`), если сервер его прислал.
    @discardableResult
    public func createContact(_ r: ContactWriteRequest) async throws -> Int64? {
        struct Created: Decodable { let id: Int64 }
        let data = try await send("POST", "/contacts", body: r)
        return (try? JSONDecoder().decode(Created.self, from: data))?.id
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
