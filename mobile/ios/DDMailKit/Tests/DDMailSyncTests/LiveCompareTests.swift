import DDMailAPI
import DDMailNet
import DDMailStore
import Foundation
import Testing
@testable import DDMailSync

/// Живая сверка (контракт внешних учёток §11): тот же ящик через ddmail и по
/// IMAP — беседы обязаны совпасть по id. Учётка — из окружения
/// (`DDMAIL_LIVE_SERVER`, `DDMAIL_LIVE_IMAP`, `DDMAIL_LIVE_USER`,
/// `DDMAIL_LIVE_PASSWORD`, `DDMAIL_LIVE_EMAIL`).
struct LiveCompareTests {
    static var env: [String: String] { ProcessInfo.processInfo.environment }

    actor Tokens: TokenStore {
        var t: String
        init(_ t: String) { self.t = t }
        func current() -> String { t }
        func update(_ token: String) { t = token }
    }

    @Test(.enabled(if: env["DDMAIL_LIVE_SERVER"] != nil && env["DDMAIL_LIVE_IMAP"] != nil))
    func sameMailboxSameIDs() async throws {
        let e = Self.env
        let user = e["DDMAIL_LIVE_USER"]!, pw = e["DDMAIL_LIVE_PASSWORD"]!, email = e["DDMAIL_LIVE_EMAIL"]!
        let login = try await DdmailAPI(baseURL: e["DDMAIL_LIVE_SERVER"]!, transport: URLSession.shared).login(username: user, password: pw)
        let api = SessionAPI(baseURL: e["DDMAIL_LIVE_SERVER"]!, transport: URLSession.shared, tokens: Tokens(login.token))
        let server = try await api.conversationsDelta(sinceMs: 0).conversations
        let byID = Dictionary(server.map { ($0.id, $0) }, uniquingKeysWith: { a, _ in a })

        let hp = e["DDMAIL_LIVE_IMAP"]!.split(separator: ":")
        let port = Int(hp[1])!
        let store = try MailStore.inMemory()
        let cfg = ImapAccountConfig(
            key: "\(user)@\(hp[0])", email: email, displayName: "", login: user,
            imap: MailEndpoint(host: String(hp[0]), port: port, security: .imap(port: port)),
            smtp: MailEndpoint(host: String(hp[0]), port: 465, security: .tls), smtpLogin: "", smtpAuth: true, ownAddresses: [email]
        )
        let engine = ImapEngine(config: cfg, store: store, password: { pw })
        let t0 = Date()
        try await engine.sync()
        let first = Date().timeIntervalSince(t0)
        let t1 = Date()
        try await engine.sync()
        let second = Date().timeIntervalSince(t1)
        let imap = try engine.currentConversations().filter { !$0.spam }
        print("ddmail: \(server.count) бесед; IMAP: \(imap.count); синк \(String(format: "%.1f", first)) с, повторный \(String(format: "%.1f", second)) с")
        print("айдентики IMAP: \(engine.identities().map(\.email))")
        var same = 0, missing: [String] = [], diff: [String] = []
        for c in imap.prefix(200) {
            guard let s = byID[c.id] else { missing.append(c.id); continue }
            same += 1
            if s.label != c.label || s.receivedBy != c.receivedBy || s.isGroup != c.isGroup {
                diff.append("\(c.id): \(s.label)|\(s.receivedBy) ≠ \(c.label)|\(c.receivedBy)")
                let sm = s.messages.map(\.messageID), im = c.messages.map(\.messageID)
                diff.append("    ddmail \(sm.count): \(sm.prefix(3)) … IMAP \(im.count): \(im.prefix(3))")
                let rows = try store.imapHeaders(cfg.key, roles: ["inbox", "sent"]).filter { r in im.prefix(3).contains(r.messageID) }
                for r in rows { diff.append("    \(r.messageID) \(r.dateTs) \(r.dateRaw) name=\(r.fromName)") }
            }
        }
        print("совпало \(same) из \(min(200, imap.count)); нет у ddmail: \(missing.count)")
        // Нет у ddmail только беседы, где своего адреса в заголовках нет:
        // сервер берёт адрес внешнего ящика, из которого письмо пришло, а IMAP
        // этого не сообщает — у них подставлен адрес учётки (как у десктопа).
        let other = missing.filter { !$0.hasPrefix(email.lowercased() + "|") }
        print("нет у ddmail, кроме подставленного адреса учётки: \(other.count)")
        for m in other.prefix(15) { print("  нет: \(m)") }
        for d in diff.prefix(15) { print("  разное: \(d)") }
        #expect(same > 0)
    }
}
