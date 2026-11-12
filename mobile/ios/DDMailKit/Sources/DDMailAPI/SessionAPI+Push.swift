import Foundation

/// Служба доставки push — столбец `push_tokens.kind` сервера: регистрация
/// одна на все платформы (контракт §7, Android §7).
public enum PushKind: String, Codable, Sendable {
    case apns, hms, fcm
}

/// Окружение APNs: токен отладочной сборки живёт только в sandbox,
/// релизной — только в production.
public enum APNsEnvironment: String, Codable, Sendable {
    case sandbox, production

    /// По значению права `aps-environment` (`development` / `production`).
    public init(entitlement: String) {
        self = entitlement == "production" ? .production : .sandbox
    }
}

/// Ответ регистрации: `delivery` — настроена ли у сервера доставка этой
/// службой (без ключа токен хранится, но push не идут).
public struct PushRegistrationResult: Decodable, Equatable, Sendable {
    public let kind: String
    public let delivery: Bool
}

/// Регистрация устройства для push (`/push/token`).
extension SessionAPI {
    /// Зарегистрировать (повтор безвреден: сервер обновит строку).
    public func registerPush(kind: PushKind, token: String, environment: APNsEnvironment?, topic: String) async throws -> PushRegistrationResult {
        struct Body: Encodable {
            let kind: String
            let token: String
            let environment: String
            let topic: String
        }
        let data = try await send("POST", "/push/token", body: Body(
            kind: kind.rawValue, token: token, environment: environment?.rawValue ?? "", topic: topic
        ))
        return try decode(PushRegistrationResult.self, from: data)
    }

    /// Снять регистрацию (выход из учётки, уведомления выключены). Токена на
    /// сервере уже нет — тоже успех.
    public func unregisterPush(kind: PushKind, token: String) async throws {
        struct Body: Encodable {
            let kind: String
            let token: String
        }
        try await send("DELETE", "/push/token", body: Body(kind: kind.rawValue, token: token))
    }
}

/// Токен без хранилища: обменянный живёт только в памяти. Для запросов
/// после выхода из учётки — обмен не должен записать удалённую сессию обратно.
public actor EphemeralTokens: TokenStore {
    private var token: String
    public init(_ token: String) { self.token = token }
    public func current() -> String { token }
    public func update(_ t: String) { token = t }
}
