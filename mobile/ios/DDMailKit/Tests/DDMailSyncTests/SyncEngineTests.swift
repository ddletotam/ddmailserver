import DDMailAPI
import DDMailStore
import Foundation
import Synchronization
import Testing
@testable import DDMailSync

/// Поддельный сервер: ответы по пути (без запроса), журнал запросов.
final class FakeServer: HTTPTransport {
    let routes = Mutex<[String: (Int, String)]>([:])
    let log = Mutex<[String]>([])

    func on(_ pathAndQuery: String, _ code: Int, _ body: String) {
        routes.withLock { $0[pathAndQuery] = (code, body) }
    }

    func on(_ pathAndQuery: String, _ body: String) { on(pathAndQuery, 200, body) }

    func send(_ request: URLRequest) async throws -> (Data, HTTPURLResponse) {
        let u = request.url!
        let key = u.path().replacingOccurrences(of: "/api/desktop/v1", with: "") + (u.query().map { "?\($0)" } ?? "")
        log.withLock { $0.append("\(request.httpMethod ?? "GET") \(key)") }
        let (code, body) = routes.withLock { $0[key] } ?? (404, #"{"error":"no route \#(key)"}"#)
        return (Data(body.utf8), HTTPURLResponse(url: u, statusCode: code, httpVersion: nil, headerFields: nil)!)
    }

    var requests: [String] { log.withLock { $0 } }
}

actor FixedTokens: TokenStore {
    func current() -> String { "T" }
    func update(_ token: String) {}
}

private func conv(_ id: String, unread: Int = 0, ts: Int64, msgs: [(Int64, String)], spam: Bool = false) -> String {
    let refs = msgs.map { #"{"folder":"INBOX","uid":\#($0.0),"message_id":"\#($0.1)","seen":\#(unread == 0)}"# }
        .joined(separator: ",")
    return #"""
    {"id":"\#(id)","label":"\#(id)","received_by":"me@a.ru","counterparts":[{"name":"","addr":"\#(id)@x.ru"}],
     "is_group":false,"last_date_ts":\#(ts),"last_subject":"s","unread_count":\#(unread),"total_count":\#(msgs.count),
     "messages":[\#(refs)],"updated_at":1,"spam":\#(spam)}
    """#
}

struct SyncEngineTests {
    let server = FakeServer()
    let store: MailStore
    let engine: SyncEngine

    init() throws {
        store = try MailStore.inMemory()
        let api = SessionAPI(baseURL: "https://mail.a.ru", transport: server, tokens: FixedTokens())
        engine = SyncEngine(accountKey: "me@mail.a.ru", api: api, store: store, now: { 1_000_000 })
        server.on("/conversations?spam=1&limit=2000", "[\(conv("spammer", unread: 1, ts: 50, msgs: [(9, "<s@x>")], spam: true))]")
        server.on("/identities", #"[{"email":"me@a.ru","is_default":true}]"#)
        server.on("/identity-groups", #"{"version":1,"doc":{"groups":[]}}"#)
        server.on("/dialog-folders", #"{"version":3,"doc":{"folders":[{"id":"w","name":"Работа","rules":{"domains":["work.ru"]}}]}}"#)
    }

    @Test func firstSyncIsFullThenDelta() async throws {
        server.on("/changes?since=0", #"{"entries":[],"latest_seq":5}"#)
        server.on("/conversations?limit=5000&since=0", """
            {"server_now_ms":777,"conversations":[\(conv("bob", unread: 1, ts: 100, msgs: [(1, "<a@x>"), (2, "<b@x>")])),\(conv("ann", ts: 90, msgs: [(3, "<c@x>")]))]}
            """)
        try await engine.sync()

        let all = try engine.currentConversations()
        #expect(all.map(\.id) == ["bob", "ann", "spammer"])
        #expect(all.map(\.spam) == [false, false, true])
        #expect(all[0].messages.count == 2)
        // Документ папок в кэше, «Спам» добавлен клиентом.
        let folders = engine.currentFolders()
        #expect(folders.folders.map(\.id) == ["w", "spam"])
        #expect(engine.identities().first?.email == "me@a.ru")

        // Второй проход: журнал удалил письмо ann — беседа уходит; дельта с водяного знака.
        server.on("/changes?since=5", #"{"entries":[{"seq":6,"message_id":"<c@x>","kind":2}],"latest_seq":6}"#)
        server.on("/conversations?limit=5000&since=777", #"{"server_now_ms":888,"conversations":[]}"#)
        try await engine.sync()
        try #expect(engine.currentConversations().map(\.id) == ["bob", "spammer"])
        #expect(server.requests.contains("GET /conversations?limit=5000&since=777"))
    }

    @Test func resetForcesFullReplacement() async throws {
        server.on("/changes?since=0", #"{"entries":[],"latest_seq":5}"#)
        server.on("/conversations?limit=5000&since=0", #"{"server_now_ms":777,"conversations":[\#(conv("old", ts: 1, msgs: [(1, "<o@x>")]))]}"#)
        try await engine.sync()
        // Курсор протух — следующий проход полный, и полная выдача заменяет набор.
        server.on("/changes?since=5", #"{"entries":[],"latest_seq":40,"reset":true}"#)
        server.on("/conversations?limit=5000&since=0", #"{"server_now_ms":999,"conversations":[\#(conv("new", ts: 2, msgs: [(2, "<n@x>")]))]}"#)
        try await engine.sync()
        try #expect(engine.currentConversations().filter { !$0.spam }.map(\.id) == ["new"])
    }

    @Test func spamFailureDoesNotFailPass() async throws {
        server.on("/changes?since=0", #"{"entries":[],"latest_seq":1}"#)
        server.on("/conversations?limit=5000&since=0", #"{"server_now_ms":1,"conversations":[]}"#)
        server.on("/conversations?spam=1&limit=2000", 500, "boom")
        try await engine.sync()
        try #expect(engine.currentConversations().isEmpty)
    }

    @Test func markReadIsLocalFirst() async throws {
        server.on("/changes?since=0", #"{"entries":[],"latest_seq":1}"#)
        server.on("/conversations?limit=5000&since=0", #"{"server_now_ms":1,"conversations":[\#(conv("bob", unread: 2, ts: 1, msgs: [(1, "<a@x>"), (2, "<b@x>")]))]}"#)
        server.on("/messages/flags", "{}")
        try await engine.sync()
        let bob = try #require(engine.currentConversations().first { $0.id == "bob" })
        try await engine.markRead(bob)
        let after = try #require(engine.currentConversations().first { $0.id == "bob" })
        #expect(after.unreadCount == 0)
        let allSeen = after.messages.allSatisfy { $0.seen }
        #expect(allSeen)
        #expect(server.requests.contains("POST /messages/flags"))
    }

    @Test func bodiesComeFromCacheSecondTime() async throws {
        server.on("/conversations/messages", #"[{"uid":1,"subject":"Привет","text":"тело","message_id":"<a@x>"},{"uid":2,"message_id":"<b@x>"}]"#)
        let refs = [MessageRef(uid: 1, messageID: "<a@x>"), MessageRef(uid: 2, messageID: "<b@x>")]
        let first = try await engine.bodies(refs)
        #expect(first.map(\.uid) == [1, 2])
        // Пустое тело (uid 2) в кэше непригодно — перезапрашивается; полное — нет.
        server.log.withLock { $0.removeAll() }
        server.on("/conversations/messages", #"[{"uid":2,"text":"теперь есть","message_id":"<b@x>"}]"#)
        let second = try await engine.bodies(refs)
        #expect(second.map(\.text) == ["тело", "теперь есть"])
        #expect(server.requests == ["POST /conversations/messages"])
    }

    @Test func assignRetriesOn409OverFreshDoc() async throws {
        server.on("/changes?since=0", #"{"entries":[],"latest_seq":1}"#)
        server.on("/conversations?limit=5000&since=0", #"{"server_now_ms":1,"conversations":[\#(conv("bob", ts: 1, msgs: [(1, "<a@x>")]))]}"#)
        server.on("/dialog-folders", #"{"version":3,"doc":{"folders":[{"id":"w","name":"Работа"},{"id":"p","name":"Личное"}]}}"#)
        try await engine.sync()
        // Другая станция успела добавить папку: сервер отвечает 409 с текущим документом.
        server.on("/dialog-folders", 409, #"{"version":4,"doc":{"folders":[{"id":"w","name":"Работа"},{"id":"p","name":"Личное"},{"id":"n","name":"Новая"}]}}"#)
        let bob = try #require(engine.currentConversations().first { $0.id == "bob" })
        // Маршрут один, поэтому повторы тоже получат 409 — и движок обязан сказать об этом.
        await #expect(throws: APIError.self) { try await engine.assignFolder([bob], folderID: "p") }
        // Наша правка легла поверх свежего документа: чужая папка не потерялась.
        let doc = engine.currentFolders()
        #expect(doc.folders.map(\.id) == ["w", "p", "n", "spam"])
        #expect(doc.folderOf(bob) == 1)
        let puts = server.requests.filter { $0 == "PUT /dialog-folders" }.count
        #expect(puts == 3)
    }

    /// Поток папок отдаёт документ, записанный синком (экран табов живёт им).
    @Test func foldersStreamSeesSyncedDoc() async throws {
        server.on("/changes?since=0", #"{"entries":[],"latest_seq":1}"#)
        server.on("/conversations?limit=5000&since=0", #"{"server_now_ms":1,"conversations":[]}"#)
        let seen = Task {
            for await d in engine.folders() where d.folders.contains(where: { $0.id == "w" }) { return true }
            return false
        }
        try await engine.sync()
        let ok = await withTaskGroup(of: Bool.self) { g in
            g.addTask { await seen.value }
            g.addTask { try? await Task.sleep(for: .seconds(2)); seen.cancel(); return false }
            let first = await g.next() ?? false
            g.cancelAll()
            return first
        }
        #expect(ok, "поток папок не увидел документ синка")
    }

    /// То же на файловом кэше (`DatabasePool`, как в приложении).
    @Test func foldersStreamOnFilePool() async throws {
        let url = FileManager.default.temporaryDirectory.appending(path: "ddmail-test-\(UUID().uuidString)/cache.sqlite")
        defer { try? FileManager.default.removeItem(at: url.deletingLastPathComponent()) }
        let fileStore = try MailStore(url: url)
        let api = SessionAPI(baseURL: "https://mail.a.ru", transport: server, tokens: FixedTokens())
        let e = SyncEngine(accountKey: "me@mail.a.ru", api: api, store: fileStore, now: { 1_000_000 })
        server.on("/changes?since=0", #"{"entries":[],"latest_seq":1}"#)
        server.on("/conversations?limit=5000&since=0", #"{"server_now_ms":1,"conversations":[]}"#)
        let seen = Task {
            for await d in e.folders() where d.folders.contains(where: { $0.id == "w" }) { return true }
            return false
        }
        try await e.sync()
        let ok = await withTaskGroup(of: Bool.self) { g in
            g.addTask { await seen.value }
            g.addTask { try? await Task.sleep(for: .seconds(2)); seen.cancel(); return false }
            let first = await g.next() ?? false
            g.cancelAll()
            return first
        }
        #expect(ok, "поток папок на DatabasePool не увидел документ синка")
    }
}
