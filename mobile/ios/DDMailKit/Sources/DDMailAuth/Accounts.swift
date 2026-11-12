import DDMailAPI
import DDMailNet
import Foundation
import Security

/// Учётка почты (контракт внешних учёток §1). Учёток несколько, любых видов
/// вперемешку; беседа идентифицируется парой (`key`, id беседы).
public struct MailAccount: Codable, Equatable, Hashable, Identifiable, Sendable {
    public enum Kind: String, Codable, Sendable {
        /// Сервер ddmail: токен сессии.
        case ddmail
        /// IMAP + SMTP паролем (в т.ч. паролем приложения).
        case imap
    }

    public var kind: Kind
    /// `account_key`: `логин@хост` (IMAP — хост IMAP, ddmail — хост сервера).
    public var key: String
    /// Адрес почты учётки.
    public var email: String
    /// Имя для «От кого».
    public var displayName: String

    // ddmail
    public var serverURL: String
    public var username: String
    public var userID: Int64

    // IMAP / SMTP
    public var imap: MailEndpoint?
    public var imapLogin: String
    public var smtp: MailEndpoint?
    /// Логин SMTP; пусто — как у IMAP.
    public var smtpLogin: String
    /// `false` — SMTP без авторизации (внутренний релей).
    public var smtpAuth: Bool
    /// «Другие мои адреса» — айдентики учётки, которых без сервера ddmail
    /// больше взять неоткуда (контракт §3).
    public var extraAddresses: [String]

    // CalDAV / CardDAV (контракт §8); у ddmail — сервер.
    public var caldavURL: String
    public var carddavURL: String

    public var id: String { key }

    public init(
        kind: Kind, key: String, email: String, displayName: String = "",
        serverURL: String = "", username: String = "", userID: Int64 = 0,
        imap: MailEndpoint? = nil, imapLogin: String = "", smtp: MailEndpoint? = nil, smtpLogin: String = "",
        smtpAuth: Bool = true, extraAddresses: [String] = [], caldavURL: String = "", carddavURL: String = ""
    ) {
        self.kind = kind; self.key = key; self.email = email; self.displayName = displayName
        self.serverURL = serverURL; self.username = username; self.userID = userID
        self.imap = imap; self.imapLogin = imapLogin; self.smtp = smtp; self.smtpLogin = smtpLogin
        self.smtpAuth = smtpAuth; self.extraAddresses = extraAddresses
        self.caldavURL = caldavURL; self.carddavURL = carddavURL
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        kind = try c.decode(Kind.self, forKey: .kind)
        key = try c.decode(String.self, forKey: .key)
        email = try c.decodeIfPresent(String.self, forKey: .email) ?? ""
        displayName = try c.decodeIfPresent(String.self, forKey: .displayName) ?? ""
        serverURL = try c.decodeIfPresent(String.self, forKey: .serverURL) ?? ""
        username = try c.decodeIfPresent(String.self, forKey: .username) ?? ""
        userID = try c.decodeIfPresent(Int64.self, forKey: .userID) ?? 0
        imap = try c.decodeIfPresent(MailEndpoint.self, forKey: .imap)
        imapLogin = try c.decodeIfPresent(String.self, forKey: .imapLogin) ?? ""
        smtp = try c.decodeIfPresent(MailEndpoint.self, forKey: .smtp)
        smtpLogin = try c.decodeIfPresent(String.self, forKey: .smtpLogin) ?? ""
        smtpAuth = try c.decodeIfPresent(Bool.self, forKey: .smtpAuth) ?? true
        extraAddresses = try c.decodeIfPresent([String].self, forKey: .extraAddresses) ?? []
        caldavURL = try c.decodeIfPresent(String.self, forKey: .caldavURL) ?? ""
        carddavURL = try c.decodeIfPresent(String.self, forKey: .carddavURL) ?? ""
    }

    /// Учётка ddmail из сессии входа.
    public static func ddmail(_ s: Session, email: String = "") -> MailAccount {
        MailAccount(kind: .ddmail, key: s.accountKey, email: email, serverURL: s.serverURL, username: s.username, userID: s.userID)
    }

    /// Учётка IMAP: ключ — `логин@хост IMAP` (`imap::account_key` десктопа).
    public static func imap(
        email: String, login: String, imap: MailEndpoint, smtp: MailEndpoint, smtpLogin: String = "", smtpAuth: Bool = true, displayName: String = ""
    ) -> MailAccount {
        MailAccount(
            kind: .imap, key: imapAccountKey(host: imap.host, username: login), email: email.lowercased(), displayName: displayName,
            imap: imap, imapLogin: login, smtp: smtp, smtpLogin: smtpLogin, smtpAuth: smtpAuth
        )
    }

    /// Свои адреса учётки (контракт §3): email, логин, если он адрес, и
    /// «другие мои адреса». Нижний регистр, без повторов, по порядку.
    public var ownAddresses: [String] {
        var seen = Set<String>()
        return ([email, kind == .imap ? imapLogin : ""] + extraAddresses)
            .map { $0.trimmingCharacters(in: .whitespaces).lowercased() }
            .filter { $0.contains("@") && seen.insert($0).inserted }
    }

    /// Подпись в списках учёток.
    public var title: String {
        if !email.isEmpty { return email }
        return key
    }
}

/// `account_key` IMAP-учётки — `username@host` (как `imap::account_key`).
public func imapAccountKey(host: String, username: String) -> String {
    "\(username)@\(host)"
}

/// Секрет учётки — только в Keychain (контракт §1, iOS §2).
public enum AccountSecret: Codable, Equatable, Sendable {
    case session(Session)
    case password(String)

    public var password: String? { if case let .password(p) = self { p } else { nil } }
    public var session: Session? { if case let .session(s) = self { s } else { nil } }
}

/// Где лежат учётки и их секреты. В приложении — файл и Keychain, в тестах — память.
public protocol AccountStore: Sendable {
    func accounts() -> [MailAccount]
    func saveAccounts(_ list: [MailAccount]) throws
    func secret(_ key: String) -> AccountSecret?
    func setSecret(_ secret: AccountSecret, for key: String) throws
    func removeSecret(_ key: String)
}

extension AccountStore {
    /// Добавить или заменить учётку (по ключу) вместе с секретом.
    public func upsert(_ account: MailAccount, secret: AccountSecret?) throws {
        if let secret { try setSecret(secret, for: account.key) }
        var list = accounts()
        if let i = list.firstIndex(where: { $0.key == account.key }) { list[i] = account } else { list.append(account) }
        try saveAccounts(list)
    }

    /// Удалить учётку и её секрет (кэш стирает вызывающий).
    public func remove(_ key: String) throws {
        removeSecret(key)
        try saveAccounts(accounts().filter { $0.key != key })
    }

    /// Всё: выход из всех учёток (сброс UI-тестов).
    public func removeAll() {
        for a in accounts() { removeSecret(a.key) }
        try? saveAccounts([])
    }
}

/// Сессия ddmail-учётки поверх `AccountStore` — для `StoredTokens`:
/// обменянный токен сразу записывается в Keychain учётки.
public struct AccountSessionStore: SessionStore {
    let key: String
    let store: AccountStore

    public init(key: String, store: AccountStore) {
        self.key = key
        self.store = store
    }

    public func load() -> Session? { store.secret(key)?.session }
    public func save(_ session: Session) throws { try store.setSecret(.session(session), for: key) }
    public func clear() { store.removeSecret(key) }
}

/// Учётки в приложении: список — JSON в Application Support (в нём нет
/// секретов: восстановленная из резервной копии учётка без секрета просто
/// попросит входа), секреты — Keychain `AfterFirstUnlockThisDeviceOnly`,
/// по записи на учётку.
public final class KeychainAccountStore: AccountStore, @unchecked Sendable {
    let file: URL
    let service: String
    private let lock = NSLock()

    public init(
        file: URL = AppGroup.accountsURL,
        service: String = "ru.letotam.ddmail.account"
    ) {
        self.file = file
        self.service = service
    }

    public func accounts() -> [MailAccount] {
        lock.withLock {
            guard let data = try? Data(contentsOf: file) else { return [] }
            return (try? JSONDecoder().decode([MailAccount].self, from: data)) ?? []
        }
    }

    public func saveAccounts(_ list: [MailAccount]) throws {
        try lock.withLock {
            try FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
            try JSONEncoder().encode(list).write(to: file, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
        }
    }

    private func query(_ key: String) -> [String: Any] {
        var q: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: key,
        ]
        #if os(macOS)
        q[kSecUseDataProtectionKeychain as String] = true
        #endif
        return q
    }

    public func secret(_ key: String) -> AccountSecret? {
        var q = query(key)
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        guard SecItemCopyMatching(q as CFDictionary, &out) == errSecSuccess, let data = out as? Data else { return nil }
        return try? JSONDecoder().decode(AccountSecret.self, from: data)
    }

    public func setSecret(_ secret: AccountSecret, for key: String) throws {
        let data = try JSONEncoder().encode(secret)
        let attrs: [String: Any] = [
            kSecValueData as String: data,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
        ]
        var status = SecItemUpdate(query(key) as CFDictionary, attrs as CFDictionary)
        if status == errSecItemNotFound {
            status = SecItemAdd(query(key).merging(attrs) { $1 } as CFDictionary, nil)
        }
        guard status == errSecSuccess else { throw KeychainError(status: status) }
    }

    public func removeSecret(_ key: String) {
        SecItemDelete(query(key) as CFDictionary)
    }

    /// Список учёток жил в Application Support приложения; с App Group
    /// (контракт §7) — в общем контейнере, чтобы его видело расширение
    /// уведомлений. Переносится один раз, старый файл удаляется после
    /// удачной записи нового.
    public func migrateFile(from old: URL = AppGroup.legacyDataDirectory.appending(path: "accounts.json")) {
        let fm = FileManager.default
        guard old.standardizedFileURL != file.standardizedFileURL, fm.fileExists(atPath: old.path) else { return }
        lock.withLock {
            if !fm.fileExists(atPath: file.path) {
                do {
                    try fm.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
                    let data = try Data(contentsOf: old)
                    try data.write(to: file, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
                } catch {
                    return // старый файл остаётся — попробуем при следующем запуске
                }
            }
            try? fm.removeItem(at: old)
        }
    }

    /// Переход со старой версии (одна ddmail-сессия в `KeychainSessionStore`):
    /// сессия становится первой учёткой, старая запись стирается.
    public func migrateLegacy(_ legacy: SessionStore = KeychainSessionStore()) {
        guard accounts().isEmpty, let s = legacy.load() else { return }
        do {
            try upsert(.ddmail(s), secret: .session(s))
            legacy.clear()
        } catch {}
    }
}

/// Учётки в памяти — для тестов.
public final class MemoryAccountStore: AccountStore, @unchecked Sendable {
    private let lock = NSLock()
    private var list: [MailAccount]
    private var secrets: [String: AccountSecret]

    public init(_ list: [MailAccount] = [], secrets: [String: AccountSecret] = [:]) {
        self.list = list
        self.secrets = secrets
    }

    public func accounts() -> [MailAccount] { lock.withLock { list } }
    public func saveAccounts(_ l: [MailAccount]) throws { lock.withLock { list = l } }
    public func secret(_ key: String) -> AccountSecret? { lock.withLock { secrets[key] } }
    public func setSecret(_ s: AccountSecret, for key: String) throws { lock.withLock { secrets[key] = s } }
    public func removeSecret(_ key: String) { lock.withLock { _ = secrets.removeValue(forKey: key) } }
}
