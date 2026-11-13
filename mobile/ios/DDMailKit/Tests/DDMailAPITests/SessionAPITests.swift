import Foundation
import Synchronization
import Testing
@testable import DDMailAPI

/// Подмена сети: ответ по пути запроса, журнал запросов.
final class FakeTransport: HTTPTransport {
    typealias Handler = @Sendable (URLRequest) async throws -> (Int, String)
    private let handler: Handler
    let log = Mutex<[String]>([])

    init(_ handler: @escaping Handler) { self.handler = handler }

    func send(_ request: URLRequest) async throws -> (Data, HTTPURLResponse) {
        let path = request.url!.path()
        let auth = request.value(forHTTPHeaderField: "Authorization") ?? "-"
        log.withLock { $0.append("\(request.httpMethod ?? "GET") \(path) \(auth)") }
        let (code, body) = try await handler(request)
        return (Data(body.utf8), HTTPURLResponse(url: request.url!, statusCode: code, httpVersion: nil, headerFields: nil)!)
    }

    var refreshCount: Int { log.withLock { $0.filter { $0.contains("/auth/refresh") }.count } }
}

actor MemoryTokens: TokenStore {
    var token: String
    init(_ t: String) { token = t }
    func current() -> String { token }
    func update(_ t: String) { token = t }
}

private let base = "https://mail.example.ru"
private func login(_ token: String) -> String { #"{"token":"\#(token)","user_id":7,"username":"bob"}"# }

struct SessionAPITests {
    @Test func loginDecodesResponse() async throws {
        let t = FakeTransport { _ in (200, login("T1")) }
        let r = try await DdmailAPI(baseURL: base, transport: t).login(username: "bob", password: "pw")
        #expect(r == LoginResponse(token: "T1", userID: 7, username: "bob"))
    }

    @Test func wrongPasswordIsUnauthorized() async {
        let t = FakeTransport { _ in (401, #"{"error":"invalid credentials"}"#) }
        await #expect(throws: APIError.unauthorized("invalid credentials")) {
            try await DdmailAPI(baseURL: base, transport: t).login(username: "bob", password: "x")
        }
    }

    @Test func expiredTokenIsRefreshedOnceAndRetried() async throws {
        let tokens = MemoryTokens("OLD")
        let t = FakeTransport { req in
            let auth = req.value(forHTTPHeaderField: "Authorization")
            if req.url!.path().hasSuffix("/auth/refresh") { return (200, login("NEW")) }
            return auth == "Bearer NEW" ? (200, #"[{"email":"bob@example.ru","is_default":true}]"#) : (401, "")
        }
        let api = SessionAPI(baseURL: base, transport: t, tokens: tokens)
        let ids = try await api.identities()
        #expect(ids == [Identity(email: "bob@example.ru", isDefault: true)])
        #expect(await tokens.current() == "NEW")
        #expect(t.refreshCount == 1)
    }

    /// Десять запросов разом упёрлись в 401 — обмен один.
    @Test func concurrent401sShareOneRefresh() async throws {
        let tokens = MemoryTokens("OLD")
        let t = FakeTransport { req in
            if req.url!.path().hasSuffix("/auth/refresh") {
                try await Task.sleep(for: .milliseconds(50))
                return (200, login("NEW"))
            }
            return req.value(forHTTPHeaderField: "Authorization") == "Bearer NEW" ? (200, "[]") : (401, "")
        }
        let api = SessionAPI(baseURL: base, transport: t, tokens: tokens)
        try await withThrowingTaskGroup(of: Void.self) { g in
            for _ in 0..<10 { g.addTask { _ = try await api.identities() } }
            try await g.waitForAll()
        }
        #expect(t.refreshCount == 1)
    }

    /// Отказ самого refresh — «нужен вход», даже если пришёл 403.
    @Test func refusedRefreshMeansLogin() async {
        let t = FakeTransport { req in
            req.url!.path().hasSuffix("/auth/refresh") ? (403, #"{"error":"token too old"}"#) : (401, "")
        }
        let api = SessionAPI(baseURL: base, transport: t, tokens: MemoryTokens("OLD"))
        await #expect(throws: APIError.unauthorized("token too old")) { try await api.identities() }
    }

    /// 403 на обычном запросе — отказ в действии, а не мёртвая сессия.
    @Test func forbiddenActionIsNotLogout() async {
        let t = FakeTransport { _ in (403, #"{"error":"calendar is read-only"}"#) }
        let api = SessionAPI(baseURL: base, transport: t, tokens: MemoryTokens("T"))
        await #expect(throws: APIError.server(code: 403, message: "calendar is read-only")) { try await api.identities() }
        #expect(t.refreshCount == 0)
    }

    /// Сбой сервера на refresh — временное, не выход из сессии.
    @Test func refreshServerErrorIsTemporary() async {
        let t = FakeTransport { req in
            req.url!.path().hasSuffix("/auth/refresh") ? (502, "Bad Gateway") : (401, "")
        }
        let api = SessionAPI(baseURL: base, transport: t, tokens: MemoryTokens("OLD"))
        await #expect(throws: APIError.server(code: 502, message: "Bad Gateway")) { try await api.identities() }
    }

    @Test func nullFieldsTakeDefaults() async throws {
        let t = FakeTransport { _ in (200, #"[{"email":"a@b.ru","name":null,"extra":1}]"#) }
        let ids = try await SessionAPI(baseURL: base, transport: t, tokens: MemoryTokens("T")).identities()
        #expect(ids == [Identity(email: "a@b.ru")])
    }

    @Test func searchSkipsDeletedAndCalendar() async throws {
        let t = FakeTransport { _ in
            (200, #"[{"id":5,"subject":"Отчёт","from":"Иван <i@x.ru>","date":1700000000,"message_id":"<a@x>","type":"email"},"#
                + #"{"id":6,"type":"calendar"},{"id":7,"type":"email","soft_deleted":true}]"#)
        }
        let hits = try await SessionAPI(baseURL: base, transport: t, tokens: MemoryTokens("T")).searchMessages("отчёт")
        #expect(hits == [MessageSearchHit(id: 5, subject: "Отчёт", from: "Иван <i@x.ru>", date: 1700000000, messageID: "<a@x>")])
        #expect(t.log.withLock { $0 }.first?.hasPrefix("GET /api/desktop/v1/search") == true)
    }

    @Test func wellKnownProbe() async {
        let yes = FakeTransport { _ in (200, #"{"ddmail":true,"version":"x"}"#) }
        let no = FakeTransport { _ in (200, "<html>") }
        let gone = FakeTransport { _ in (404, "") }
        #expect(await DdmailAPI.isDdmail(host: "mail.x.ru", transport: yes))
        #expect(!(await DdmailAPI.isDdmail(host: "mail.x.ru", transport: no)))
        #expect(!(await DdmailAPI.isDdmail(host: "mail.x.ru", transport: gone)))
        #expect(yes.log.withLock { $0 } == ["GET /.well-known/ddmail -"])
    }
}

struct SendRequestTests {
    /// Форма `/send` — как у Android и десктопа: snake_case и base64 вложений.
    @Test func encodesLikeServerExpects() throws {
        let r = SendRequest(
            from: "\"Я\" <me@a.ru>", to: ["bob@x.ru"], subject: "Re: тема", html: "<div>x</div>", text: "x",
            inReplyTo: "<m1@x>", references: "<m0@x> <m1@x>",
            attachments: [OutgoingAttachment(filename: "a.txt", mimeType: "text/plain", content: Data("hi".utf8))]
        )
        let obj = try #require(JSONSerialization.jsonObject(with: JSONEncoder().encode(r)) as? [String: Any])
        #expect(obj["in_reply_to"] as? String == "<m1@x>")
        #expect(obj["cc"] as? [String] == [])
        let att = try #require((obj["attachments"] as? [[String: Any]])?.first)
        #expect(att["mime_type"] as? String == "text/plain")
        #expect(att["content"] as? String == "aGk=")
    }
}
