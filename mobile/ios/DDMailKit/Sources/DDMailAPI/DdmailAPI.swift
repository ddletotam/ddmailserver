import Foundation

/// Ответ входа и обмена токена (`/auth/login`, `/auth/refresh`).
public struct LoginResponse: Decodable, Equatable, Sendable {
    public let token: String
    public let userID: Int64
    public let username: String

    enum CodingKeys: String, CodingKey {
        case token, username
        case userID = "user_id"
    }

    public init(token: String, userID: Int64, username: String) {
        self.token = token
        self.userID = userID
        self.username = username
    }
}

/// Запросы без сессии к `/api/desktop/v1` сервера `baseURL` (`https://хост`).
public struct DdmailAPI: Sendable {
    public let baseURL: String
    let transport: HTTPTransport

    public init(baseURL: String, transport: HTTPTransport) {
        self.baseURL = baseURL
        self.transport = transport
    }

    func url(_ path: String) -> URL {
        // Путь собираем мы сами из известных частей — разбор не упадёт.
        URL(string: "\(baseURL)/api/desktop/v1\(path)")!
    }

    /// Вход по логину и паролю → токен. Пароль дальше нигде не живёт (контракт §2).
    public func login(username: String, password: String) async throws -> LoginResponse {
        struct Body: Encodable { let username: String; let password: String }
        let req = try URLRequest.json(url("/auth/login"), method: "POST", body: Body(username: username, password: password))
        return try decode(LoginResponse.self, from: await transport.body(req))
    }

    /// Обменять токен (в том числе просроченный, не старше 30 суток) на свежий.
    /// Отказ в обмене — всегда «нужен вход», каким бы кодом он ни пришёл.
    public func refresh(_ token: String) async throws -> LoginResponse {
        var req = URLRequest(url: url("/auth/refresh"))
        req.httpMethod = "POST"
        do {
            return try decode(LoginResponse.self, from: await transport.body(req.bearer(token)))
        } catch let APIError.server(code, message) where (400..<500).contains(code) {
            throw APIError.unauthorized(message)
        }
    }

    /// Отвечает ли на `host` сервер ddmail: `GET https://host/.well-known/ddmail`
    /// с `{"ddmail": true}`. Любая ошибка — «нет».
    public static func isDdmail(host: String, transport: HTTPTransport) async -> Bool {
        guard let url = URL(string: "https://\(host)/.well-known/ddmail") else { return false }
        var req = URLRequest(url: url)
        req.timeoutInterval = 10
        guard let data = try? await transport.body(req),
              let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
        else { return false }
        return obj["ddmail"] as? Bool == true
    }
}
