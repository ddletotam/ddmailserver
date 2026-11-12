import DDMailAPI
import Foundation

/// Источник календаря: сервер ddmail (`SessionAPI`) или CalDAV внешней
/// учётки. Имена — как у API сервера: экран календаря работает с любым.
public protocol CalendarSource: Sendable {
    func calendars() async throws -> [CalendarDTO]
    func calendarEvents(fromMs: Int64, toMs: Int64) async throws -> [EventDTO]
    func createEvent(_ r: EventCreateRequest) async throws
    func patchEvent(id: Int64, _ r: EventPatchRequest) async throws
    func deleteEvent(id: Int64) async throws
    func rsvp(id: Int64, partstat: String) async throws
}

/// Источник контактов: сервер ddmail или CardDAV внешней учётки.
public protocol ContactsSource: Sendable {
    func contacts(limit: Int) async throws -> [ContactDTO]
    func addressBooks() async throws -> [AddressBookDTO]
    func createContact(_ r: ContactWriteRequest) async throws
    func patchContact(id: Int64, _ r: ContactWriteRequest) async throws
    func deleteContact(id: Int64) async throws
}

extension SessionAPI: CalendarSource, ContactsSource {}

/// Стабильный отрицательный id для объектов DAV (FNV-1a, как
/// `event_id_from_uid` десктопа): с положительными id сервера не пересекается.
public func davID(_ parts: String...) -> Int64 {
    var h: UInt64 = 0xcbf29ce484222325
    for b in parts.joined(separator: "\u{1F}").utf8 {
        h ^= UInt64(b)
        h = h &* 0x100000001b3
    }
    return -Int64((h >> 2) | 1)
}

/// CalDAV учётки (контракт внешних учёток §8): все календари home-set,
/// `REPORT` с time-range, запись `PUT If-None-Match`/`If-Match`, удаление
/// `DELETE If-Match`. Элементы помечены учёткой (`identityEmail`).
public actor DAVCalendarSource: CalendarSource {
    public nonisolated let accountKey: String
    let client: DAVClient
    let start: URL
    let email: String
    private var collections: [Int64: DAVCollection] = [:]
    /// id события → ресурс и его ETag.
    private var resources: [Int64: (url: URL, etag: String?, calendarID: Int64)] = [:]

    public init(accountKey: String, url: URL, email: String, client: DAVClient) {
        self.accountKey = accountKey
        start = url
        self.email = email.lowercased()
        self.client = client
    }

    public func calendars() async throws -> [CalendarDTO] {
        let found = try await client.discover(start, kind: .calendar)
        collections = Dictionary(found.map { (davID(accountKey, $0.url.absoluteString), $0) }, uniquingKeysWith: { a, _ in a })
        return found.map { c in
            var d = CalendarDTO(id: davID(accountKey, c.url.absoluteString), name: c.name, color: Self.color(c.color), canWrite: c.canWrite)
            d.sourceType = "caldav"
            d.description = email
            return d
        }
    }

    /// `#RRGGBBAA` (Apple) → `#RRGGBB`.
    static func color(_ c: String) -> String {
        let t = c.trimmingCharacters(in: .whitespaces)
        return t.count == 9 && t.hasPrefix("#") ? String(t.prefix(7)) : t
    }

    public func calendarEvents(fromMs: Int64, toMs: Int64) async throws -> [EventDTO] {
        if collections.isEmpty { _ = try await calendars() }
        let body = """
        <?xml version="1.0" encoding="utf-8"?>
        <C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
          <D:prop><D:getetag/><C:calendar-data/></D:prop>
          <C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">
            <C:time-range start="\(ICal.utc(fromMs))" end="\(ICal.utc(toMs))"/>
          </C:comp-filter></C:comp-filter></C:filter>
        </C:calendar-query>
        """
        var out: [EventDTO] = []
        for (calID, col) in collections {
            let x = try await client.report(col.url, body: body)
            for r in x.descendants("response") {
                guard let href = r.first("href")?.text.trimmingCharacters(in: .whitespacesAndNewlines),
                      let url = URL(string: href, relativeTo: col.url)?.absoluteURL,
                      let data = r.descendants("calendar-data").first?.text, !data.isEmpty else { continue }
                let etag = r.descendants("getetag").first?.text.trimmingCharacters(in: .whitespacesAndNewlines)
                let account = accountKey
                for var e in ICal.events(data, calendarID: calID, makeID: { _, rid in davID(account, url.absoluteString, rid) }) {
                    e.editable = col.canWrite
                    e.deletable = col.canWrite
                    e.identityEmail = email
                    resources[e.id] = (url, etag, calID)
                    out.append(e)
                }
            }
        }
        return out
    }

    public func createEvent(_ r: EventCreateRequest) async throws {
        guard let col = collections[r.calendar_id] else { throw APIError.server(code: 404, message: "Календарь не найден.") }
        let uid = UUID().uuidString.lowercased()
        let ics = ICal.build(uid: uid, summary: r.summary, description: r.description, location: r.location,
                             startMs: r.dtstart, endMs: r.dtend, allDay: r.all_day)
        _ = try await client.request("PUT", col.url.appending(path: "\(uid).ics"), body: ics,
                                     contentType: "text/calendar; charset=utf-8", headers: ["If-None-Match": "*"])
    }

    private func resource(_ id: Int64) throws -> (url: URL, etag: String?, calendarID: Int64) {
        guard let r = resources[id] else { throw APIError.server(code: 404, message: "Событие не найдено — обновите календарь.") }
        return r
    }

    public func patchEvent(id: Int64, _ r: EventPatchRequest) async throws {
        if r.scope == "future" {
            throw APIError.server(code: 0, message: "Во внешнем календаре правится вся серия целиком.")
        }
        let res = try resource(id)
        let cur = try await client.request("GET", res.url)
        let merged = ICal.merge(cur.text, summary: r.summary, description: r.description, location: r.location,
                                startMs: r.dtstart, endMs: r.dtend, allDay: r.all_day)
        _ = try await client.request("PUT", res.url, body: merged, contentType: "text/calendar; charset=utf-8",
                                     headers: (cur.etag ?? res.etag).map { ["If-Match": $0] } ?? [:])
    }

    public func deleteEvent(id: Int64) async throws {
        let res = try resource(id)
        _ = try await client.request("DELETE", res.url, headers: res.etag.map { ["If-Match": $0] } ?? [:])
        resources.removeValue(forKey: id)
    }

    public func rsvp(id: Int64, partstat: String) async throws {
        let res = try resource(id)
        let cur = try await client.request("GET", res.url)
        let updated = ICal.setPartstat(cur.text, emails: [email], partstat: partstat)
        _ = try await client.request("PUT", res.url, body: updated, contentType: "text/calendar; charset=utf-8",
                                     headers: (cur.etag ?? res.etag).map { ["If-Match": $0] } ?? [:])
    }
}

/// CardDAV учётки: все адресные книги home-set; запись vCard через
/// `build_vcard`, правка — только своих полей.
public actor DAVContactsSource: ContactsSource {
    public nonisolated let accountKey: String
    let client: DAVClient
    let start: URL
    let email: String
    private var books: [Int64: DAVCollection] = [:]
    private var resources: [Int64: (url: URL, etag: String?)] = [:]

    public init(accountKey: String, url: URL, email: String, client: DAVClient) {
        self.accountKey = accountKey
        start = url
        self.email = email
        self.client = client
    }

    public func addressBooks() async throws -> [AddressBookDTO] {
        let found = try await client.discover(start, kind: .addressBook)
        books = Dictionary(found.map { (davID(accountKey, $0.url.absoluteString), $0) }, uniquingKeysWith: { a, _ in a })
        return found.map { AddressBookDTO(id: davID(accountKey, $0.url.absoluteString), name: $0.name, sourceName: email, canWrite: $0.canWrite) }
    }

    public func contacts(limit: Int) async throws -> [ContactDTO] {
        if books.isEmpty { _ = try await addressBooks() }
        let body = """
        <?xml version="1.0" encoding="utf-8"?>
        <A:addressbook-query xmlns:D="DAV:" xmlns:A="urn:ietf:params:xml:ns:carddav">
          <D:prop><D:getetag/><A:address-data/></D:prop>
        </A:addressbook-query>
        """
        var out: [ContactDTO] = []
        for (_, book) in books {
            let x = try await client.report(book.url, body: body)
            for r in x.descendants("response") {
                guard let href = r.first("href")?.text.trimmingCharacters(in: .whitespacesAndNewlines),
                      let url = URL(string: href, relativeTo: book.url)?.absoluteURL,
                      let data = r.descendants("address-data").first?.text, !data.isEmpty,
                      let card = VCard.parse(data).first else { continue }
                let id = davID(accountKey, url.absoluteString)
                resources[id] = (url, r.descendants("getetag").first?.text.trimmingCharacters(in: .whitespacesAndNewlines))
                out.append(ContactDTO(id: id, fullName: card.fullName, emails: card.emails, phones: card.phones,
                                      organization: card.organization, title: card.title))
                if out.count >= limit { return out }
            }
        }
        return out
    }

    public func createContact(_ r: ContactWriteRequest) async throws {
        guard let id = r.address_book_id, let book = books[id] else { throw APIError.server(code: 404, message: "Адресная книга не найдена.") }
        let uid = UUID().uuidString.lowercased()
        let card = VCard.build(uid: uid, fullName: r.full_name, emails: r.emails, phones: r.phones, organization: r.organization, title: r.title)
        _ = try await client.request("PUT", book.url.appending(path: "\(uid).vcf"), body: card,
                                     contentType: "text/vcard; charset=utf-8", headers: ["If-None-Match": "*"])
    }

    public func patchContact(id: Int64, _ r: ContactWriteRequest) async throws {
        guard let res = resources[id] else { throw APIError.server(code: 404, message: "Контакт не найден — обновите список.") }
        let cur = try await client.request("GET", res.url)
        let card = VCard.merge(cur.text, fullName: r.full_name, emails: r.emails, phones: r.phones, organization: r.organization, title: r.title)
        _ = try await client.request("PUT", res.url, body: card, contentType: "text/vcard; charset=utf-8",
                                     headers: (cur.etag ?? res.etag).map { ["If-Match": $0] } ?? [:])
    }

    public func deleteContact(id: Int64) async throws {
        guard let res = resources[id] else { return }
        _ = try await client.request("DELETE", res.url, headers: res.etag.map { ["If-Match": $0] } ?? [:])
        resources.removeValue(forKey: id)
    }
}

/// Общий календарь: ddmail и CalDAV внешних учёток вместе (§8). Действие
/// уходит источнику календаря или события; отказ одного источника не мешает
/// остальным — о нём сообщается `onFailure` (учётка — «нужен вход» или
/// «нет связи»), а ошибка наружу — только если не ответил никто.
public actor CalendarHub: CalendarSource {
    public typealias Failure = @Sendable (String, Error) async -> Void
    let sources: [(key: String, source: any CalendarSource)]
    let onFailure: Failure
    private var calendarOwner: [Int64: Int] = [:]
    private var eventOwner: [Int64: Int] = [:]

    public init(_ sources: [(key: String, source: any CalendarSource)], onFailure: @escaping Failure) {
        self.sources = sources
        self.onFailure = onFailure
    }

    public var isEmpty: Bool { sources.isEmpty }

    private func gather<T: Sendable>(_ f: @escaping @Sendable (any CalendarSource) async throws -> [T]) async throws -> [(Int, [T])] {
        var ok: [(Int, [T])] = []
        var firstError: Error?
        await withTaskGroup(of: (Int, Result<[T], Error>).self) { group in
            for (i, s) in sources.enumerated() {
                group.addTask { do { return (i, .success(try await f(s.source))) } catch { return (i, .failure(error)) } }
            }
            for await (i, r) in group {
                switch r {
                case let .success(v): ok.append((i, v))
                case let .failure(e):
                    if e is CancellationError { continue }
                    firstError = firstError ?? e
                    await onFailure(sources[i].key, e)
                }
            }
        }
        if ok.isEmpty, let firstError { throw firstError }
        return ok
    }

    public func calendars() async throws -> [CalendarDTO] {
        let got = try await gather { try await $0.calendars() }
        for (i, list) in got { for c in list { calendarOwner[c.id] = i } }
        return got.flatMap(\.1)
    }

    public func calendarEvents(fromMs: Int64, toMs: Int64) async throws -> [EventDTO] {
        let got = try await gather { try await $0.calendarEvents(fromMs: fromMs, toMs: toMs) }
        for (i, list) in got { for e in list { eventOwner[e.id] = i } }
        return got.flatMap(\.1)
    }

    private func source(calendar id: Int64) throws -> any CalendarSource {
        guard let i = calendarOwner[id] else { throw APIError.server(code: 404, message: "Календарь не найден.") }
        return sources[i].source
    }

    private func source(event id: Int64) throws -> any CalendarSource {
        guard let i = eventOwner[id] else { throw APIError.server(code: 404, message: "Событие не найдено — обновите календарь.") }
        return sources[i].source
    }

    public func createEvent(_ r: EventCreateRequest) async throws { try await source(calendar: r.calendar_id).createEvent(r) }
    public func patchEvent(id: Int64, _ r: EventPatchRequest) async throws { try await source(event: id).patchEvent(id: id, r) }
    public func deleteEvent(id: Int64) async throws { try await source(event: id).deleteEvent(id: id) }
    public func rsvp(id: Int64, partstat: String) async throws { try await source(event: id).rsvp(id: id, partstat: partstat) }
}

/// Общая книга контактов: ddmail и CardDAV внешних учёток.
public actor ContactsHub: ContactsSource {
    public typealias Failure = @Sendable (String, Error) async -> Void
    let sources: [(key: String, source: any ContactsSource)]
    let onFailure: Failure
    private var bookOwner: [Int64: Int] = [:]
    private var contactOwner: [Int64: Int] = [:]

    public init(_ sources: [(key: String, source: any ContactsSource)], onFailure: @escaping Failure) {
        self.sources = sources
        self.onFailure = onFailure
    }

    private func gather<T: Sendable>(_ f: @escaping @Sendable (any ContactsSource) async throws -> [T]) async throws -> [(Int, [T])] {
        var ok: [(Int, [T])] = []
        var firstError: Error?
        await withTaskGroup(of: (Int, Result<[T], Error>).self) { group in
            for (i, s) in sources.enumerated() {
                group.addTask { do { return (i, .success(try await f(s.source))) } catch { return (i, .failure(error)) } }
            }
            for await (i, r) in group {
                switch r {
                case let .success(v): ok.append((i, v))
                case let .failure(e):
                    if e is CancellationError { continue }
                    firstError = firstError ?? e
                    await onFailure(sources[i].key, e)
                }
            }
        }
        if ok.isEmpty, let firstError { throw firstError }
        return ok
    }

    public func contacts(limit: Int) async throws -> [ContactDTO] {
        let got = try await gather { try await $0.contacts(limit: limit) }
        for (i, list) in got { for c in list { contactOwner[c.id] = i } }
        return got.flatMap(\.1)
    }

    public func addressBooks() async throws -> [AddressBookDTO] {
        let got = try await gather { try await $0.addressBooks() }
        for (i, list) in got { for b in list { bookOwner[b.id] = i } }
        return got.flatMap(\.1)
    }

    public func createContact(_ r: ContactWriteRequest) async throws {
        guard let b = r.address_book_id, let i = bookOwner[b] else { throw APIError.server(code: 404, message: "Адресная книга не найдена.") }
        try await sources[i].source.createContact(r)
    }

    public func patchContact(id: Int64, _ r: ContactWriteRequest) async throws {
        guard let i = contactOwner[id] else { throw APIError.server(code: 404, message: "Контакт не найден.") }
        try await sources[i].source.patchContact(id: id, r)
    }

    public func deleteContact(id: Int64) async throws {
        guard let i = contactOwner[id] else { return }
        try await sources[i].source.deleteContact(id: id)
    }
}
