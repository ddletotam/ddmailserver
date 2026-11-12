import DDMailAPI
import DDMailAuth
import DDMailStore
import DDMailSync
import Foundation
import Synchronization
import Testing
@testable import DDMailPush

/// Поддельный сервер ddmail: ответ по методу и пути (с запросом), журнал
/// запросов с телами.
final class FakeServer: HTTPTransport {
    let routes = Mutex<[String: (Int, String)]>([:])
    let log = Mutex<[String]>([])
    let delay = Mutex<Duration>(.zero)

    func on(_ key: String, _ code: Int, _ body: String) {
        routes.withLock { $0[key] = (code, body) }
    }

    func on(_ key: String, _ body: String) { on(key, 200, body) }

    func send(_ request: URLRequest) async throws -> (Data, HTTPURLResponse) {
        let u = request.url!
        let path = u.path().replacingOccurrences(of: "/api/desktop/v1", with: "") + (u.query().map { "?\($0)" } ?? "")
        let method = request.httpMethod ?? "GET"
        let body = request.httpBody.map { String(decoding: $0, as: UTF8.self) } ?? ""
        log.withLock { $0.append("\(method) \(path) \(body)".trimmingCharacters(in: .whitespaces)) }
        let d = delay.withLock { $0 }
        if d > .zero { try await Task.sleep(for: d) }
        let (code, text) = routes.withLock { $0["\(method) \(path)"] ?? $0[path] } ?? (404, #"{"error":"no route"}"#)
        return (Data(text.utf8), HTTPURLResponse(url: u, statusCode: code, httpVersion: nil, headerFields: nil)!)
    }

    var requests: [String] { log.withLock { $0 } }
}

/// Настройки в своём наборе — тесты не трогают настройки процесса.
func freshDefaults() -> UserDefaults {
    let name = "ddmail.tests.\(UUID().uuidString)"
    let d = UserDefaults(suiteName: name)!
    d.removePersistentDomain(forName: name)
    return d
}

func tempDir() throws -> URL {
    let u = FileManager.default.temporaryDirectory.appending(path: "ddmail-push-\(UUID().uuidString)")
    try FileManager.default.createDirectory(at: u, withIntermediateDirectories: true)
    return u
}

private let token = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"

struct PushRegistryTests {
    let server = FakeServer()
    let state = SharedState(freshDefaults())

    init() {
        server.on("POST /push/token", #"{"kind":"apns","delivery":true}"#)
        server.on("DELETE /push/token", #"{"deleted":true}"#)
    }

    func target(_ key: String) -> PushTarget {
        PushTarget(accountKey: key, api: SessionAPI(baseURL: "https://mail.a.ru", transport: server, tokens: EphemeralTokens("T")))
    }

    /// Каждой учётке — один раз; повтор ничего не шлёт.
    @Test func registersEachAccountOnce() async {
        let r = PushRegistry(state: state, topic: "ru.letotam.ddmail", environment: .sandbox)
        await r.sync([target("a")]) // токена ещё нет — нечего слать
        #expect(server.requests.isEmpty)
        #expect(await r.setDeviceToken(token.uppercased()))
        await r.sync([target("a"), target("b")])
        await r.sync([target("a"), target("b")])
        let posts = server.requests.filter { $0.hasPrefix("POST") }
        #expect(posts.count == 2)
        #expect(posts[0].contains(#""token":"\#(token)""#))
        #expect(posts[0].contains(#""environment":"sandbox""#))
        #expect(posts[0].contains(#""topic":"ru.letotam.ddmail""#))
        #expect(state.registrations == ["a": "\(token)|sandbox", "b": "\(token)|sandbox"])
    }

    /// Новый токен: старый снимается, новый регистрируется.
    @Test func tokenChangeReplacesRegistration() async {
        let r = PushRegistry(state: state, topic: "t", environment: .production)
        await r.setDeviceToken(token)
        await r.sync([target("a")])
        #expect(await r.setDeviceToken("0011") == true)
        #expect(await r.setDeviceToken("0011") == false)
        await r.sync([target("a")])
        let tail = server.requests.suffix(2)
        #expect(tail.first?.hasPrefix("DELETE /push/token") == true && tail.first?.contains(token) == true)
        #expect(tail.last?.hasPrefix("POST /push/token") == true && tail.last?.contains(#""token":"0011""#) == true)
        #expect(state.registrations["a"] == "0011|production")
    }

    /// Уведомления выключили — регистрации сняты; включили — снова.
    @Test func disabledNotificationsUnregister() async {
        let r = PushRegistry(state: state, topic: "t", environment: .sandbox)
        await r.setDeviceToken(token)
        await r.sync([target("a")])
        state.notificationsEnabled = false
        await r.sync([target("a")])
        #expect(server.requests.last?.hasPrefix("DELETE /push/token") == true)
        #expect(state.registrations.isEmpty)
        state.notificationsEnabled = true
        await r.sync([target("a")])
        #expect(server.requests.last?.hasPrefix("POST /push/token") == true)
    }

    /// Сервер старой версии (404) — не записываем, повторим потом.
    @Test func failedRegistrationIsRetried() async {
        let old = FakeServer()
        let r = PushRegistry(state: state, topic: "t", environment: .sandbox)
        await r.setDeviceToken(token)
        let t = PushTarget(accountKey: "a", api: SessionAPI(baseURL: "https://x", transport: old, tokens: EphemeralTokens("T")))
        await r.sync([t])
        await r.sync([t])
        #expect(old.requests.count == 2)
        #expect(state.registrations.isEmpty)
    }

    /// Удаление учётки снимает её регистрацию.
    @Test func removeUnregisters() async {
        let r = PushRegistry(state: state, topic: "t", environment: .sandbox)
        await r.setDeviceToken(token)
        await r.sync([target("a")])
        await r.remove(target("a"))
        #expect(server.requests.last?.hasPrefix("DELETE /push/token") == true)
        #expect(state.registrations["a"] == nil)
    }

    @Test func hexToken() {
        #expect(Data([0x00, 0xab, 0xff]).hexString == "00abff")
    }
}

struct SharedStateTests {
    /// Рубежи и выключатель переезжают из настроек приложения один раз.
    @Test func migratesFromAppDefaults() {
        let old = freshDefaults()
        old.set(["a": NSNumber(value: Int64(42))], forKey: "notify.marks")
        old.set(false, forKey: "settings.notifications")
        let s = SharedState(freshDefaults())
        s.migrate(from: old)
        #expect(s.notifiedMarks == ["a": 42])
        #expect(s.notificationsEnabled == false)
        #expect(old.object(forKey: "notify.marks") == nil)
        // Повторный перенос не затирает новое.
        old.set(["a": NSNumber(value: Int64(1))], forKey: "notify.marks")
        s.notifiedMarks = ["a": 50]
        s.migrate(from: old)
        #expect(s.notifiedMarks == ["a": 50])
    }

    @Test func forgetDropsAccount() {
        let s = SharedState(freshDefaults())
        s.notifiedMarks = ["a": 1, "b": 2]
        s.registrations = ["a": "x|sandbox", "b": "x|sandbox"]
        s.forget(account: "a")
        #expect(s.notifiedMarks == ["b": 2])
        #expect(s.registrations == ["b": "x|sandbox"])
    }
}

private func conv(_ id: String, ts: Int64, msgs: [(uid: Int64, seen: Bool)], subject: String = "Тема", from: String? = nil, spam: Bool = false) -> String {
    let refs = msgs.map { #"{"folder":"INBOX","uid":\#($0.uid),"message_id":"<\#($0.uid)@x>","seen":\#($0.seen)}"# }
        .joined(separator: ",")
    let unread = msgs.filter { !$0.seen }.count
    return #"""
    {"id":"\#(id)","label":"\#(from ?? id)","received_by":"me@a.ru","counterparts":[{"name":"","addr":"\#(id)@x.ru"}],
     "is_group":false,"last_date_ts":\#(ts),"last_subject":"\#(subject)","unread_count":\#(unread),"total_count":\#(msgs.count),
     "messages":[\#(refs)],"updated_at":1,"spam":\#(spam)}
    """#
}

struct PushCatchUpTests {
    let server = FakeServer()
    let state = SharedState(freshDefaults())
    let dir: URL
    let accounts: MemoryAccountStore
    let account = "bob@mail.a.ru"

    init() throws {
        dir = try tempDir()
        let session = Session(serverURL: "https://mail.a.ru", username: "bob", userID: 7, token: "T")
        accounts = MemoryAccountStore([.ddmail(session)], secrets: [account: .session(session)])
        server.on("/conversations?spam=1&limit=2000", "[\(conv("spammer", ts: 50, msgs: [(90, false)], spam: true))]")
        server.on("/identities", #"[{"email":"bob@a.ru","is_default":true}]"#)
        server.on("/identity-groups", #"{"version":1,"doc":{"groups":[]}}"#)
        server.on("/dialog-folders", #"{"version":1,"doc":{"folders":[{"id":"h","name":"Скрыто","rules":{"domains":["hidden.ru"]},"hidden":true}]}}"#)
    }

    func catchUp() -> PushCatchUp {
        PushCatchUp(
            accounts: accounts, transport: server, state: state, cacheURL: dir.appending(path: "cache.sqlite"),
            syncLock: ProcessLock(url: dir.appending(path: "sync.lock")), notifyLock: ProcessLock(url: dir.appending(path: "notify.lock"))
        )
    }

    /// Первый проход запоминает рубеж; следующий push — настоящий текст
    /// новой беседы; спам и скрытая папка молчат.
    @Test func syncsAndPlansFromSharedCache() async throws {
        server.on("/changes?since=0", #"{"entries":[],"latest_seq":1}"#)
        server.on("/conversations?limit=5000&since=0", #"{"server_now_ms":100,"conversations":[\#(conv("ann", ts: 10, msgs: [(1, false)]))]}"#)
        let first = await catchUp().run(budget: .seconds(10))
        #expect(first.complete)
        #expect(first.items.isEmpty)
        #expect(first.badge == 1)
        #expect(state.notifiedMarks == [account: 1])

        server.on("/changes?since=1", #"{"entries":[],"latest_seq":2}"#)
        server.on("/conversations?limit=5000&since=100", """
            {"server_now_ms":200,"conversations":[\(conv("ivan", ts: 20, msgs: [(5, false), (6, false)], subject: "Отчёт", from: "Иван Петров")),\(conv("sec", ts: 30, msgs: [(7, false)]).replacingOccurrences(of: "sec@x.ru", with: "sec@hidden.ru"))]}
            """)
        let second = await catchUp().run(budget: .seconds(10))
        #expect(second.complete)
        #expect(second.items.map(\.conversation.id) == ["ivan"])
        let text = NotificationPlan.text(try #require(second.items.first))
        #expect(text.title == "Иван Петров")
        #expect(text.body == "Отчёт · новых: 2")
        #expect(second.badge == 2) // ann и ivan; скрытая и спам — нет
        #expect(state.notifiedMarks == [account: 7])

        // Тот же push ещё раз (или приложение уже уведомило) — показывать нечего.
        server.on("/changes?since=2", #"{"entries":[],"latest_seq":2}"#)
        server.on("/conversations?limit=5000&since=200", #"{"server_now_ms":300,"conversations":[]}"#)
        let third = await catchUp().run(budget: .seconds(10))
        #expect(third.complete && third.items.isEmpty)
    }

    /// Не успели за отведённое время — итог неполный (покажется обезличенный текст).
    @Test func budgetExceededIsIncomplete() async {
        server.delay.withLock { $0 = .seconds(5) }
        let start = ContinuousClock.now
        let r = await catchUp().run(budget: .milliseconds(300))
        #expect(!r.complete)
        #expect(ContinuousClock.now - start < .seconds(3))
    }

    /// Мёртвая сессия — узнать нечем, итог неполный.
    @Test func deadSessionIsIncomplete() async {
        let server401 = FakeServer()
        server401.on("/auth/refresh", 401, #"{"error":"expired"}"#)
        let c = PushCatchUp(
            accounts: accounts, transport: server401, state: state, cacheURL: dir.appending(path: "cache.sqlite"),
            syncLock: ProcessLock(url: dir.appending(path: "sync.lock")), notifyLock: ProcessLock(url: dir.appending(path: "notify.lock"))
        )
        let r = await c.run(budget: .seconds(5))
        #expect(!r.complete)
    }

    /// Учёток нет (вышли) — показывать нечего, и это точно.
    @Test func noAccounts() async {
        let c = PushCatchUp(accounts: MemoryAccountStore(), transport: server, state: state, cacheURL: dir.appending(path: "c.sqlite"))
        let r = await c.run(budget: .seconds(1))
        #expect(r.complete && r.items.isEmpty)
    }
}

struct ProcessLockTests {
    @Test func exclusiveUntilReleased() async throws {
        let url = try tempDir().appending(path: "x.lock")
        let a = ProcessLock(url: url), b = ProcessLock(url: url)
        let held = try #require(await a.acquire(timeout: .seconds(1)))
        #expect(await b.acquire(timeout: .milliseconds(200)) == nil)
        held.release()
        let again = await b.acquire(timeout: .seconds(1))
        #expect(again != nil)
        again?.release()
    }
}
