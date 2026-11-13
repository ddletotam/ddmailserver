import DDMailNet
import Foundation
import Testing
@testable import DDMailIMAP

/// Живая проверка на настоящем сервере — только когда учётка задана в
/// окружении (`DDMAIL_LIVE_IMAP=host:port`, `DDMAIL_LIVE_USER`,
/// `DDMAIL_LIVE_PASSWORD`); в git учётных данных нет.
struct LiveIMAPTests {
    static var env: [String: String] { ProcessInfo.processInfo.environment }

    @Test(.enabled(if: env["DDMAIL_LIVE_IMAP"] != nil))
    func listSelectFetch() async throws {
        let hp = Self.env["DDMAIL_LIVE_IMAP"]!.split(separator: ":")
        let port = Int(hp[1])!
        let c = try await IMAPClient.connect(MailEndpoint(host: String(hp[0]), port: port, security: .imap(port: port)), connector: SystemMailConnector())
        try await c.login(user: Self.env["DDMAIL_LIVE_USER"]!, password: Self.env["DDMAIL_LIVE_PASSWORD"]!)
        print("caps:", await c.capabilities.sorted().joined(separator: " "))
        let boxes = try await c.list()
        print("folders:", boxes.map { "\($0.name) \($0.attributes)" })
        let s = try await c.select("INBOX", readOnly: true)
        print("INBOX exists \(s.exists) uidvalidity \(s.uidValidity) uidnext \(s.uidNext) modseq \(s.highestModSeq)")
        if s.exists > 0 {
            let from = max(1, s.exists - 2)
            let floor = try await c.fetchUID(seq: from) ?? 1
            let r = try await c.uidFetch("\(floor):*", "UID FLAGS ENVELOPE INTERNALDATE BODY.PEEK[HEADER.FIELDS (FROM SUBJECT MESSAGE-ID)]")
            for f in r.fetches { print(f.uid ?? 0, f.flags ?? [], f.envelope?.messageID ?? "", f.header?.count ?? 0) }
            #expect(!r.fetches.isEmpty)
        }
        await c.logout()
    }

    /// «Сообщения» поиска на настоящем сервере: ASCII и кириллица
    /// (`CHARSET UTF-8` литералом) принимаются, найденное — UID папки.
    /// Запрос — `DDMAIL_LIVE_SEARCH` (по умолчанию «ddmail»).
    @Test(.enabled(if: env["DDMAIL_LIVE_IMAP"] != nil))
    func searchTextAsciiAndCyrillic() async throws {
        let hp = Self.env["DDMAIL_LIVE_IMAP"]!.split(separator: ":")
        let port = Int(hp[1])!
        let c = try await IMAPClient.connect(MailEndpoint(host: String(hp[0]), port: port, security: .imap(port: port)), connector: SystemMailConnector())
        try await c.login(user: Self.env["DDMAIL_LIVE_USER"]!, password: Self.env["DDMAIL_LIVE_PASSWORD"]!)
        let s = try await c.select("INBOX", readOnly: true)
        let q = Self.env["DDMAIL_LIVE_SEARCH"] ?? "ddmail"
        let ascii = try await c.searchText(q)
        let cyr = try await c.searchText("Проверка")
        print("search «\(q)»: \(ascii.count), «Проверка»: \(cyr.count) из \(s.exists)")
        #expect((ascii + cyr).allSatisfy { $0 > 0 && $0 < s.uidNext })
        await c.logout()
    }
}
