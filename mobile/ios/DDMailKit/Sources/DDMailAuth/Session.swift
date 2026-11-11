import DDMailAPI
import Foundation
import Security

/// Сессия учётки ddmail. Пароль не хранится нигде — только токен (контракт §2).
public struct Session: Codable, Equatable, Sendable {
    public var serverURL: String
    public var username: String
    public var userID: Int64
    public var token: String

    public init(serverURL: String, username: String, userID: Int64, token: String) {
        self.serverURL = serverURL
        self.username = username
        self.userID = userID
        self.token = token
    }

    /// Ключ учётки для общих с десктопом документов: `логин@хост`
    /// (как `account_key` десктопа и `accountKey` Android).
    public var accountKey: String {
        let host = URL(string: serverURL)?.host() ?? serverURL
        return "\(username)@\(host)"
    }
}

/// Где лежит сессия. В приложении — Keychain; в тестах — память.
public protocol SessionStore: Sendable {
    func load() -> Session?
    func save(_ session: Session) throws
    func clear()
}

/// Сессия целиком — одна запись Keychain (контракт §2):
/// `AfterFirstUnlockThisDeviceOnly` — фон (push, BGTask) может обменять токен
/// при заблокированном экране, а в резервную копию на другое устройство
/// запись не попадает. На Маке — keychain с защитой данных, как на iOS.
public struct KeychainSessionStore: SessionStore {
    let service: String
    let account = "session"

    public init(service: String = "ru.letotam.ddmail.session") {
        self.service = service
    }

    private var query: [String: Any] {
        var q: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
        #if os(macOS)
        q[kSecUseDataProtectionKeychain as String] = true
        #endif
        return q
    }

    public func load() -> Session? {
        var q = query
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        guard SecItemCopyMatching(q as CFDictionary, &out) == errSecSuccess,
              let data = out as? Data
        else { return nil }
        return try? JSONDecoder().decode(Session.self, from: data)
    }

    public func save(_ session: Session) throws {
        let data = try JSONEncoder().encode(session)
        let attrs: [String: Any] = [
            kSecValueData as String: data,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
        ]
        var status = SecItemUpdate(query as CFDictionary, attrs as CFDictionary)
        if status == errSecItemNotFound {
            status = SecItemAdd(query.merging(attrs) { $1 } as CFDictionary, nil)
        }
        guard status == errSecSuccess else { throw KeychainError(status: status) }
    }

    public func clear() {
        SecItemDelete(query as CFDictionary)
    }
}

public struct KeychainError: Error, LocalizedError, Equatable {
    public let status: OSStatus
    public var errorDescription: String? {
        let msg = SecCopyErrorMessageString(status, nil) as String? ?? "код \(status)"
        return "Не удалось сохранить сессию в связке ключей: \(msg)"
    }
}

/// Токен для `SessionAPI` из хранилища сессии: обновлённый токен сразу
/// записывается, чтобы перезапуск приложения не вернул просроченный.
public actor StoredTokens: TokenStore {
    private var session: Session
    private let store: SessionStore

    public init(session: Session, store: SessionStore) {
        self.session = session
        self.store = store
    }

    public func current() -> String { session.token }

    public func update(_ token: String) {
        session.token = token
        try? store.save(session)
    }
}
