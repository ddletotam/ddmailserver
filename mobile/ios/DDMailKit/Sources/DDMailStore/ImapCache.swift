import Foundation
import GRDB

/// Состояние папки IMAP-учётки (контракт внешних учёток §3): по нему проход
/// инкрементальный — новые письма от `UIDNEXT`, флаги — от `HIGHESTMODSEQ`.
public struct ImapFolderRecord: Codable, Equatable, Sendable, FetchableRecord, PersistableRecord {
    public static let databaseTableName = "imap_folders"

    public var accountKey: String
    /// Роль папки: `inbox`, `sent`, `drafts`, `junk`.
    public var role: String
    /// Имя на сервере (modified UTF-7) — для команд.
    public var name: String
    public var uidValidity: Int64
    public var uidNext: Int64
    public var highestModSeq: Int64
    /// Нижний UID окна (последние N писем папки).
    public var floorUID: Int64
    /// Сколько раз менялась UIDVALIDITY: входит в id письма, чтобы новые
    /// письма после сброса нумерации были «новее» старых (рубеж уведомлений).
    public var epoch: Int64

    public init(accountKey: String, role: String, name: String, uidValidity: Int64 = 0, uidNext: Int64 = 0,
                highestModSeq: Int64 = 0, floorUID: Int64 = 0, epoch: Int64 = 0) {
        self.accountKey = accountKey; self.role = role; self.name = name; self.uidValidity = uidValidity
        self.uidNext = uidNext; self.highestModSeq = highestModSeq; self.floorUID = floorUID; self.epoch = epoch
    }
}

/// Адрес в кэше заголовков.
public struct StoredAddress: Codable, Equatable, Hashable, Sendable {
    public var n: String
    public var e: String

    public init(name: String, email: String) {
        n = name
        e = email
    }
}

/// Заголовки письма IMAP-папки — из них на устройстве строятся беседы.
public struct ImapHeaderRecord: Codable, Equatable, Sendable, FetchableRecord, PersistableRecord {
    public static let databaseTableName = "imap_messages"

    public var accountKey: String
    public var role: String
    public var uid: Int64
    public var messageID: String
    public var fromName: String
    public var fromAddr: String
    public var toJSON: String
    public var ccJSON: String
    public var subject: String
    /// Секунды: `Date`, не разобралась — `INTERNALDATE`.
    public var dateTs: Int64
    public var dateRaw: String
    public var seen: Bool
    public var draft: Bool
    public var flagged: Bool
    public var inReplyTo: String
    public var referencesJSON: String
    public var hasAttachments: Bool

    public init(
        accountKey: String, role: String, uid: Int64, messageID: String = "", fromName: String = "", fromAddr: String = "",
        to: [StoredAddress] = [], cc: [StoredAddress] = [], subject: String = "", dateTs: Int64 = 0, dateRaw: String = "",
        seen: Bool = false, draft: Bool = false, flagged: Bool = false, inReplyTo: String = "", references: [String] = [],
        hasAttachments: Bool = false
    ) {
        self.accountKey = accountKey; self.role = role; self.uid = uid; self.messageID = messageID
        self.fromName = fromName; self.fromAddr = fromAddr
        toJSON = Self.encode(to); ccJSON = Self.encode(cc)
        self.subject = subject; self.dateTs = dateTs; self.dateRaw = dateRaw; self.seen = seen; self.draft = draft
        self.flagged = flagged; self.inReplyTo = inReplyTo
        referencesJSON = String(decoding: (try? JSONEncoder().encode(references)) ?? Data("[]".utf8), as: UTF8.self)
        self.hasAttachments = hasAttachments
    }

    static func encode(_ a: [StoredAddress]) -> String {
        String(decoding: (try? JSONEncoder().encode(a)) ?? Data("[]".utf8), as: UTF8.self)
    }

    public var to: [StoredAddress] { (try? JSONDecoder().decode([StoredAddress].self, from: Data(toJSON.utf8))) ?? [] }
    public var cc: [StoredAddress] { (try? JSONDecoder().decode([StoredAddress].self, from: Data(ccJSON.utf8))) ?? [] }
    public var references: [String] { (try? JSONDecoder().decode([String].self, from: Data(referencesJSON.utf8))) ?? [] }
}

extension MailStore {
    static func registerImapTables(_ m: inout DatabaseMigrator) {
        m.registerMigration("v2-accounts") { db in
            // Учёток стало несколько: служебные ключи ddmail-учётки — с её
            // ключом в имени (`<account>/<ключ>`), иначе две учётки делили бы курсор.
            if let acc = try String.fetchOne(db, sql: "SELECT value FROM meta WHERE key = 'account_key'") {
                for k in ["journal_seq", "conv_since", "conv_full_ts", "conv_id_epoch", "dialog_folders",
                          "dialog_folders_version", "identities", "identity_groups"] {
                    try db.execute(sql: "UPDATE meta SET key = ? WHERE key = ?", arguments: ["\(acc)/\(k)", k])
                }
                try db.execute(sql: "DELETE FROM meta WHERE key = 'account_key'")
            }
            try db.create(table: "imap_folders") { t in
                t.column("accountKey", .text).notNull()
                t.column("role", .text).notNull()
                t.column("name", .text).notNull()
                t.column("uidValidity", .integer).notNull()
                t.column("uidNext", .integer).notNull()
                t.column("highestModSeq", .integer).notNull()
                t.column("floorUID", .integer).notNull()
                t.column("epoch", .integer).notNull()
                t.primaryKey(["accountKey", "role"])
            }
            try db.create(table: "imap_messages") { t in
                t.column("accountKey", .text).notNull()
                t.column("role", .text).notNull()
                t.column("uid", .integer).notNull()
                t.column("messageID", .text).notNull()
                t.column("fromName", .text).notNull()
                t.column("fromAddr", .text).notNull()
                t.column("toJSON", .text).notNull()
                t.column("ccJSON", .text).notNull()
                t.column("subject", .text).notNull()
                t.column("dateTs", .integer).notNull()
                t.column("dateRaw", .text).notNull()
                t.column("seen", .boolean).notNull()
                t.column("draft", .boolean).notNull()
                t.column("flagged", .boolean).notNull()
                t.column("inReplyTo", .text).notNull()
                t.column("referencesJSON", .text).notNull()
                t.column("hasAttachments", .boolean).notNull()
                t.primaryKey(["accountKey", "role", "uid"])
            }
        }
    }

    // MARK: Папки

    public func imapFolders(_ account: String) throws -> [String: ImapFolderRecord] {
        try db.read { db in
            Dictionary(try ImapFolderRecord.filter(Column("accountKey") == account).fetchAll(db).map { ($0.role, $0) },
                       uniquingKeysWith: { a, _ in a })
        }
    }

    public func putImapFolder(_ f: ImapFolderRecord) throws {
        try db.write { try f.insert($0, onConflict: .replace) }
    }

    /// Папка пропала с сервера (её удалили, роль ушла другой) — забыть её состояние и письма.
    public func dropImapFolder(_ account: String, role: String) throws {
        try db.write { db in
            _ = try ImapFolderRecord.filter(Column("accountKey") == account && Column("role") == role).deleteAll(db)
            _ = try ImapHeaderRecord.filter(Column("accountKey") == account && Column("role") == role).deleteAll(db)
        }
    }

    // MARK: Заголовки

    public func imapUIDs(_ account: String, role: String) throws -> [Int64] {
        try db.read { db in
            try Int64.fetchAll(db, sql: "SELECT uid FROM imap_messages WHERE accountKey = ? AND role = ? ORDER BY uid",
                               arguments: [account, role])
        }
    }

    public func imapHeaders(_ account: String, roles: [String]) throws -> [ImapHeaderRecord] {
        try db.read { db in
            try ImapHeaderRecord.filter(Column("accountKey") == account && roles.contains(Column("role"))).fetchAll(db)
        }
    }

    public func imapHeaders(_ account: String, role: String, uids: [Int64]) throws -> [ImapHeaderRecord] {
        try db.read { db in
            try ImapHeaderRecord.filter(Column("accountKey") == account && Column("role") == role && uids.contains(Column("uid"))).fetchAll(db)
        }
    }

    public func putImapHeaders(_ rows: [ImapHeaderRecord]) throws {
        guard !rows.isEmpty else { return }
        try db.write { db in for r in rows { try r.insert(db, onConflict: .replace) } }
    }

    /// Флаги изменились: `(uid, seen, draft, flagged)`.
    public func updateImapFlags(_ account: String, role: String, _ flags: [(uid: Int64, seen: Bool, draft: Bool, flagged: Bool)]) throws {
        guard !flags.isEmpty else { return }
        try db.write { db in
            for f in flags {
                try db.execute(sql: "UPDATE imap_messages SET seen = ?, draft = ?, flagged = ? WHERE accountKey = ? AND role = ? AND uid = ?",
                               arguments: [f.seen, f.draft, f.flagged, account, role, f.uid])
            }
        }
    }

    /// Прочитано/непрочитано по набору писем (оптимистично, до ответа сервера).
    public func setImapSeen(_ account: String, role: String, uids: [Int64], seen: Bool) throws {
        guard !uids.isEmpty else { return }
        try db.write { db in
            try db.execute(sql: "UPDATE imap_messages SET seen = ? WHERE accountKey = ? AND role = ? AND uid IN (\(uids.map { String($0) }.joined(separator: ",")))",
                           arguments: [seen, account, role])
        }
    }

    @discardableResult
    public func deleteImapHeaders(_ account: String, role: String, uids: [Int64]) throws -> Int {
        guard !uids.isEmpty else { return 0 }
        return try db.write { db in
            try ImapHeaderRecord.filter(Column("accountKey") == account && Column("role") == role && uids.contains(Column("uid"))).deleteAll(db)
        }
    }

    /// Сменилась UIDVALIDITY: заголовки папки и её тела (диапазон id) — прочь.
    public func clearImapFolder(_ account: String, role: String, bodyUIDs: ClosedRange<Int64>) throws {
        try db.write { db in
            _ = try ImapHeaderRecord.filter(Column("accountKey") == account && Column("role") == role).deleteAll(db)
            try db.execute(sql: "DELETE FROM message_bodies WHERE accountKey = ? AND uid BETWEEN ? AND ?",
                           arguments: [account, bodyUIDs.lowerBound, bodyUIDs.upperBound])
        }
    }

    /// Тела по id (удалены письма).
    public func deleteBodies(_ account: String, uids: [Int64]) throws {
        guard !uids.isEmpty else { return }
        try db.write { db in
            _ = try BodyRecord.filter(Column("accountKey") == account && uids.contains(Column("uid"))).deleteAll(db)
        }
    }

    // MARK: Учётка целиком

    /// Удаление учётки: её беседы (и спам), тела, заголовки, служебные ключи.
    public func clearAccount(_ account: String) throws {
        try db.write { db in
            for k in [account, "\(account)#spam"] {
                for t in ["conversations", "conversation_messages", "message_bodies", "imap_messages", "imap_folders"] {
                    try db.execute(sql: "DELETE FROM \(t) WHERE accountKey = ?", arguments: [k])
                }
            }
            try db.execute(sql: "DELETE FROM meta WHERE key LIKE ? ESCAPE '\\'", arguments: [Self.likePrefix(account + "/")])
        }
    }

    static func likePrefix(_ s: String) -> String {
        s.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "%", with: "\\%").replacingOccurrences(of: "_", with: "\\_") + "%"
    }
}
