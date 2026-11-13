import DDMailAPI
import DDMailNet
import DDMailStore
import Foundation
import Testing
@testable import DDMailSync

/// Инкрементальный синк IMAP на фальшивом сервере (контракт внешних учёток §3–§5).
struct ImapEngineTests {
    let me = "me@example.org"

    func engine(_ server: FakeIMAPServer, store: MailStore? = nil) throws -> (ImapEngine, MailStore) {
        let s = try store ?? MailStore.inMemory()
        let cfg = ImapAccountConfig(
            key: "me@imap.example.org", email: me, displayName: "Я", login: "me",
            imap: MailEndpoint(host: "imap.example.org", port: 993, security: .tls),
            smtp: MailEndpoint(host: "smtp.example.org", port: 465, security: .tls),
            smtpLogin: "", smtpAuth: true, ownAddresses: [me]
        )
        let e = ImapEngine(config: cfg, store: s, password: { "secret" }, connector: server.connector, constrainedConnector: server.connector)
        return (e, s)
    }

    func fetches(_ s: FakeIMAPServer) -> [String] { s.with { $0.log.filter { $0.hasPrefix("UID FETCH") } } }

    @Test(arguments: [(true, true), (true, false), (false, false)])
    func initialThenIncremental(condstore: Bool, qresync: Bool) async throws {
        let server = FakeIMAPServer(condstore: condstore, qresync: qresync)
        let a = server.deliver("INBOX", from: "Боб <bob@example.net>", to: me, subject: "Привет", seen: true)
        server.deliver("Sent", from: me, to: "bob@example.net", subject: "Re: Привет", seen: true)
        server.deliver("Junk", from: "spam@bad.example", to: me, subject: "Выигрыш")
        let (e, _) = try engine(server)
        try await e.sync()
        var convs = try e.currentConversations()
        #expect(convs.filter { !$0.spam }.map(\.id) == ["me@example.org|bob@example.net"])
        #expect(convs.first { !$0.spam }?.messages.count == 2)
        #expect(convs.first { !$0.spam }?.label == "Боб")
        #expect(convs.filter(\.spam).map(\.id) == ["spam:me@example.org|spam@bad.example"])
        #expect(e.spamSupport() == .folder)

        // Новое письмо, флаг, удаление — второй проход забирает только разницу.
        server.with { $0.log.removeAll() }
        server.deliver("INBOX", from: "bob@example.net", to: me, subject: "Ещё")
        server.setFlag("INBOX", uid: a, "\\Flagged", true)
        server.setFlag("INBOX", uid: a, "\\Seen", false)
        try await e.sync()
        convs = try e.currentConversations()
        let c = try #require(convs.first { !$0.spam })
        #expect(c.messages.count == 3)
        #expect(c.unreadCount == 2)
        // Заголовки всего окна заново не тянутся: только «последний+1:*».
        #expect(!fetches(server).contains { $0.hasPrefix("UID FETCH 1:*") && $0.contains("HEADER.FIELDS") })

        server.expunge("INBOX", uid: a)
        try await e.sync()
        #expect(try e.currentConversations().first { !$0.spam }?.messages.count == 2)
    }

    @Test func noChangesFetchesNoHeaders() async throws {
        let server = FakeIMAPServer(condstore: false, qresync: false)
        server.deliver("INBOX", from: "bob@example.net", to: me, subject: "x")
        let (e, _) = try engine(server)
        try await e.sync()
        server.with { $0.log.removeAll() }
        try await e.sync()
        #expect(!fetches(server).contains { $0.contains("HEADER.FIELDS") })
    }

    @Test func uidValidityChangeResetsFolder() async throws {
        let server = FakeIMAPServer()
        server.deliver("INBOX", from: "bob@example.net", to: me, subject: "один")
        server.deliver("INBOX", from: "ann@example.net", to: me, subject: "два")
        let (e, store) = try engine(server)
        try await e.sync()
        let before = try e.currentConversations().flatMap(\.messages).map(\.uid)
        _ = try await e.bodies(try e.currentConversations().flatMap(\.messages))
        server.expunge("INBOX", uid: 1)
        server.renumber("INBOX")
        try await e.sync()
        let after = try e.currentConversations()
        #expect(after.map(\.id) == ["me@example.org|ann@example.net"])
        // Поколение выросло — id писем новые, тела старой нумерации выброшены.
        #expect(Set(after.flatMap(\.messages).map(\.uid)).isDisjoint(with: before))
        #expect(try store.cachedBodyUIDs("me@imap.example.org").isEmpty)
    }

    @Test func bodiesParsedAndPeek() async throws {
        let server = FakeIMAPServer()
        server.deliver("INBOX", from: "Боб <bob@example.net>", to: me, subject: "=?utf-8?B?0KLQtdC80LA=?=", body: "Текст письма")
        let (e, _) = try engine(server)
        try await e.sync()
        let c = try #require(try e.currentConversations().first)
        let b = try await e.bodies(c.messages)
        #expect(b.first?.text == "Текст письма\r\n")
        #expect(b.first?.subject == "Тема")
        #expect(b.first?.fromAddr == "bob@example.net")
        #expect(b.first?.isOutgoing == false)
        #expect(server.with { $0.log.contains { $0.contains("BODY.PEEK[]") } })
        // \Seen не появился от чтения тела.
        #expect(server.flags("INBOX")[1]?.contains("\\Seen") == false)
    }

    @Test func markReadDeleteAndSpam() async throws {
        let server = FakeIMAPServer(move: false)
        server.deliver("INBOX", from: "bob@example.net", to: me, subject: "x")
        server.deliver("INBOX", from: "ann@example.net", to: me, subject: "y")
        server.deliver("INBOX", from: "eve@example.net", to: me, subject: "z")
        let (e, _) = try engine(server)
        try await e.sync()
        let convs = try e.currentConversations()
        let bob = try #require(convs.first { $0.id.hasSuffix("bob@example.net") })
        try await e.markRead(bob)
        #expect(server.flags("INBOX")[1]?.contains("\\Seen") == true)
        #expect(try e.currentConversations().first { $0.id == bob.id }?.unreadCount == 0)

        let ann = try #require(convs.first { $0.id.hasSuffix("ann@example.net") })
        try await e.deleteConversations([ann])
        #expect(server.uids("INBOX") == [1, 3])

        let eve = try #require(convs.first { $0.id.hasSuffix("eve@example.net") })
        _ = try await e.reportSpam([eve], scope: "address")
        #expect(server.uids("INBOX") == [1])
        #expect(server.uids("Junk").count == 1)
        let spam = try #require(try e.currentConversations().first(where: \.spam))
        #expect(spam.id == "spam:me@example.org|eve@example.net")
        try await e.restoreSpam([spam], trust: false)
        #expect(server.uids("Junk").isEmpty)
        #expect(server.uids("INBOX").count == 2)
    }

    @Test func deleteOneMessageAndSource() async throws {
        let server = FakeIMAPServer()
        server.deliver("INBOX", from: "bob@example.net", to: me, subject: "x", body: "первое")
        server.deliver("INBOX", from: "bob@example.net", to: me, subject: "y", body: "второе")
        let (e, _) = try engine(server)
        try await e.sync()
        let c = try #require(try e.currentConversations().first)
        #expect(c.messages.count == 2)
        let first = try #require(c.messages.min { $0.uid < $1.uid })
        let raw = String(decoding: try await e.source(uid: first.uid), as: UTF8.self)
        #expect(raw.contains("Subject: x"))
        #expect(raw.contains("первое"))
        try await e.deleteMessages([first])
        #expect(server.uids("INBOX") == [2])
        #expect(try e.currentConversations().first?.messages.count == 1)
    }

    @Test func refusedChangeRollsBack() async throws {
        let server = FakeIMAPServer()
        server.deliver("INBOX", from: "bob@example.net", to: me, subject: "x")
        let (e, _) = try engine(server)
        try await e.sync()
        let c = try #require(try e.currentConversations().first)
        server.with { $0.failing = ["UID STORE"] }
        await #expect(throws: APIError.self) { try await e.markRead(c) }
        #expect(try e.currentConversations().first?.unreadCount == 1)
        await #expect(throws: APIError.self) { try await e.deleteConversations([c]) }
        #expect(try e.currentConversations().count == 1)
    }

    @Test func wrongPasswordIsUnauthorized() async throws {
        let server = FakeIMAPServer()
        server.with { $0.password = "other" }
        let (e, _) = try engine(server)
        await #expect(throws: APIError.unauthorized("Invalid credentials")) { try await e.sync() }
    }

    @Test func sentCopyAppendedWhenServerDidNotFileIt() async throws {
        let server = FakeIMAPServer()
        let (e, _) = try engine(server)
        try await e.sync()
        await e.ensureSentCopy(messageID: "abc@example.org", data: Data("Message-ID: <abc@example.org>\r\nSubject: s\r\n\r\nhi\r\n".utf8),
                               firstDelay: .zero, window: .zero)
        #expect(server.uids("Sent").count == 1)
        #expect(server.flags("Sent").values.first?.contains("\\Seen") == true)
        // Уже лежит — второй раз не кладём.
        await e.ensureSentCopy(messageID: "abc@example.org", data: Data(), firstDelay: .zero, window: .zero)
        #expect(server.uids("Sent").count == 1)
    }

    /// IDLE: новое письмо — событие; перевыдача по сроку не рвёт соединение.
    @Test func idleReportsNewMail() async throws {
        let server = FakeIMAPServer()
        let (e, _) = try engine(server)
        try await e.sync()
        let (events, sink) = AsyncStream.makeStream(of: Void.self)
        let watcher = Task {
            await e.watch(idleFor: .milliseconds(300), onEvent: { sink.yield() }, onAuthFailure: {})
        }
        try await Task.sleep(for: .milliseconds(800))
        server.deliver("INBOX", from: "bob@example.net", to: me, subject: "новое")
        var it = events.makeAsyncIterator()
        _ = await it.next()
        watcher.cancel()
        // Перевыдача IDLE по сроку шла на том же соединении.
        #expect(server.with { $0.log.filter { $0 == "IDLE" }.count } >= 2)
        #expect(server.with { $0.log.filter { $0.hasPrefix("LOGIN") }.count } == 2)
    }

    @Test func idleStopsOnWrongPassword() async throws {
        let server = FakeIMAPServer()
        server.with { $0.password = "other" }
        let (e, _) = try engine(server)
        let dead = Flag()
        await e.watch(onEvent: {}, onAuthFailure: { dead.set() })
        #expect(dead.value)
    }

    final class Flag: @unchecked Sendable {
        private let lock = NSLock()
        private var v = false
        func set() { lock.withLock { v = true } }
        var value: Bool { lock.withLock { v } }
    }
}
