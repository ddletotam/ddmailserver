import Foundation

/// Откуда API берёт токен и куда кладёт обновлённый (в приложении — Keychain).
public protocol TokenStore: Sendable {
    func current() async -> String
    func update(_ token: String) async
}

/// Запросы от имени сессии (контракт §2, как `SessionApi` Android). На 401 —
/// один раз обменять токен через `/auth/refresh` и повторить; refresh тоже
/// отказал — сессия мертва, `APIError.unauthorized` уходит наверх («нужен
/// вход»). Обмен общий: десять запросов, разом упёршихся в 401, делают один
/// refresh, а не десять.
public final class SessionAPI: Sendable {
    public let baseURL: String
    let transport: HTTPTransport
    let tokens: TokenStore
    let raw: DdmailAPI
    private let gate = RefreshGate()

    public init(baseURL: String, transport: HTTPTransport, tokens: TokenStore) {
        self.baseURL = baseURL
        self.transport = transport
        self.tokens = tokens
        raw = DdmailAPI(baseURL: baseURL, transport: transport)
    }

    func url(_ path: String) -> URL { raw.url(path) }

    /// Запрос с токеном; на 401 — обмен и один повтор.
    func authed(_ build: (String) throws -> URLRequest) async throws -> Data {
        try await authedResponse(build).0
    }

    func authedResponse(_ build: (String) throws -> URLRequest) async throws -> (Data, HTTPURLResponse) {
        let token = await tokens.current()
        do {
            return try await transport.checked(build(token))
        } catch APIError.unauthorized {
            let fresh = try await gate.refreshed(stale: token, tokens: tokens, api: raw)
            return try await transport.checked(build(fresh))
        }
    }

    /// Текущий токен (WebSocket ставит его в заголовок рукопожатия сам).
    public func currentToken() async -> String { await tokens.current() }

    /// Обменять токен вне запроса: WebSocket отвергнут как неавторизованный.
    public func refreshToken() async throws {
        let token = await tokens.current()
        _ = try await gate.refreshed(stale: token, tokens: tokens, api: raw)
    }

    func get<T: Decodable>(_ path: String, as type: T.Type) async throws -> T {
        let data = try await authed { URLRequest(url: url(path)).bearer($0) }
        return try decode(type, from: data)
    }

    @discardableResult
    func send(_ method: String, _ path: String, body: some Encodable) async throws -> Data {
        try await authed { try URLRequest.json(url(path), method: method, body: body).bearer($0) }
    }

    /// Свои адреса отправки. Заодно — дешёвая проверка, жива ли сессия.
    public func identities() async throws -> [Identity] {
        try await get("/identities", as: [Identity].self)
    }
}

/// Один обмен токена на всех, кто упёрся в 401.
actor RefreshGate {
    private var inflight: Task<String, Error>?

    func refreshed(stale: String, tokens: TokenStore, api: DdmailAPI) async throws -> String {
        // Пока ждали своей очереди, токен мог уже обменять соседний запрос.
        let now = await tokens.current()
        if now != stale { return now }
        if let t = inflight { return try await t.value }
        let t = Task {
            let r = try await api.refresh(stale)
            await tokens.update(r.token)
            return r.token
        }
        inflight = t
        defer { inflight = nil }
        return try await t.value
    }
}
