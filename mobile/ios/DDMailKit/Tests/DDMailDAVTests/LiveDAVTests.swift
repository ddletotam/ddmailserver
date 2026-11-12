import DDMailAPI
import Foundation
import Testing
@testable import DDMailDAV

/// Живая проверка DAV (только чтение) — учётка из окружения:
/// `DDMAIL_LIVE_DAV_EMAIL`, `DDMAIL_LIVE_DAV_USER`, `DDMAIL_LIVE_DAV_PASSWORD`, `DDMAIL_LIVE_DAV_IMAP`.
struct LiveDAVTests {
    static var env: [String: String] { ProcessInfo.processInfo.environment }

    @Test(.enabled(if: env["DDMAIL_LIVE_DAV_EMAIL"] != nil))
    func discoverAndRead() async throws {
        let e = Self.env
        let pw = e["DDMAIL_LIVE_DAV_PASSWORD"]!
        let client = DAVClient(user: e["DDMAIL_LIVE_DAV_USER"]!, password: { pw })
        let email = e["DDMAIL_LIVE_DAV_EMAIL"]!, host = e["DDMAIL_LIVE_DAV_IMAP"]!
        let cal = await client.find(email: email, imapHost: host, kind: .calendar)
        let card = await client.find(email: email, imapHost: host, kind: .addressBook)
        print("CalDAV: \(cal?.absoluteString ?? "-"), CardDAV: \(card?.absoluteString ?? "-")")
        if let cal {
            let s = DAVCalendarSource(accountKey: "k", url: cal, email: email, client: client)
            let cals = try await s.calendars()
            print("календари: \(cals.map { "\($0.name)\($0.canWrite ? "" : " (только чтение)")" })")
            let now = Int64(Date().timeIntervalSince1970 * 1000)
            let evs = try await s.calendarEvents(fromMs: now - 60 * 86_400_000, toMs: now + 60 * 86_400_000)
            print("событий за ±60 дней: \(evs.count); с повтором: \(evs.filter { !$0.rrule.isEmpty }.count)")
        }
        if let card {
            let s = DAVContactsSource(accountKey: "k", url: card, email: email, client: client)
            let books = try await s.addressBooks()
            let cs = try await s.contacts(limit: 5000)
            print("книги: \(books.map(\.name)); контактов: \(cs.count)")
        }
    }
}
