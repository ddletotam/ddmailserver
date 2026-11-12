import DDMailAPI
import DDMailNet
import Foundation
import Testing
@testable import DDMailAuth

struct AccountsTests {
    final class LegacyStore: SessionStore, @unchecked Sendable {
        var s: Session?
        init(_ s: Session?) { self.s = s }
        func load() -> Session? { s }
        func save(_ session: Session) throws { s = session }
        func clear() { s = nil }
    }

    @Test func imapKeyAndOwnAddresses() {
        var a = MailAccount.imap(
            email: "Me@Example.org", login: "me@example.org",
            imap: MailEndpoint(host: "imap.example.org", port: 993, security: .tls),
            smtp: MailEndpoint(host: "smtp.example.org", port: 465, security: .tls)
        )
        a.extraAddresses = ["alias@example.org", "ME@example.org", "не-адрес"]
        #expect(a.key == "me@example.org@imap.example.org")
        #expect(a.ownAddresses == ["me@example.org", "alias@example.org"])
    }

    @Test func upsertReplaceRemove() throws {
        let store = MemoryAccountStore()
        let s = Session(serverURL: "https://mail.example.org", username: "bob", userID: 1, token: "t")
        try store.upsert(.ddmail(s), secret: .session(s))
        try store.upsert(.ddmail(s, email: "bob@example.org"), secret: nil)
        #expect(store.accounts().count == 1)
        #expect(store.accounts()[0].email == "bob@example.org")
        #expect(store.secret("bob@mail.example.org")?.session?.token == "t")
        // Обменянный токен — в Keychain учётки.
        try AccountSessionStore(key: "bob@mail.example.org", store: store).save(Session(serverURL: s.serverURL, username: "bob", userID: 1, token: "t2"))
        #expect(store.secret("bob@mail.example.org")?.session?.token == "t2")
        try store.remove("bob@mail.example.org")
        #expect(store.accounts().isEmpty)
        #expect(store.secret("bob@mail.example.org") == nil)
    }

    @Test func legacySessionBecomesFirstAccount() throws {
        let dir = URL.temporaryDirectory.appending(path: UUID().uuidString)
        let store = KeychainAccountStore(file: dir.appending(path: "accounts.json"), service: "test.\(UUID().uuidString)")
        // Без Keychain (тест без подписи) секрет может не записаться — проверяем список.
        let legacy = LegacyStore(Session(serverURL: "https://mail.example.org", username: "bob", userID: 7, token: "t"))
        store.migrateLegacy(legacy)
        if store.accounts().isEmpty {
            // Keychain недоступен в этой среде — миграция честно ничего не сделала и старую сессию не стёрла.
            #expect(legacy.s != nil)
        } else {
            #expect(store.accounts().map(\.key) == ["bob@mail.example.org"])
            #expect(legacy.s == nil)
            store.removeAll()
        }
        try? FileManager.default.removeItem(at: dir)
    }

    @Test func accountDecodesOlderJSON() throws {
        let json = #"{"kind":"imap","key":"u@h","email":"u@x"}"#
        let a = try JSONDecoder().decode(MailAccount.self, from: Data(json.utf8))
        #expect(a.smtpAuth)
        #expect(a.extraAddresses.isEmpty)
    }
}
