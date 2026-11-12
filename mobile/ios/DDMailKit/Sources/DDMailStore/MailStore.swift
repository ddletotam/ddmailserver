import DDMailAPI
import Foundation
import GRDB

/// Беседы пространства кэша вместе со ссылками на их письма.
public struct ConversationSnapshot: Equatable, Sendable {
    public var conversations: [ConversationRecord]
    public var refs: [String: [MessageRefRecord]]
}

/// Кэш почты на SQLite (GRDB) — контракт §1: явные миграции, один писатель.
/// `DatabasePool` (WAL): чтения не ждут записи, а Notification Service
/// Extension (§7) сможет открыть тот же файл из App Group.
public final class MailStore: Sendable {
    let db: any DatabaseWriter

    /// Кэш в файле `url` (каталог создаётся).
    public convenience init(url: URL) throws {
        try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        var config = Configuration()
        config.foreignKeysEnabled = false
        try self.init(writer: DatabasePool(path: url.path, configuration: config))
    }

    /// Кэш в памяти — для тестов.
    public static func inMemory() throws -> MailStore {
        try MailStore(writer: DatabaseQueue())
    }

    init(writer: any DatabaseWriter) throws {
        db = writer
        try Self.migrator.migrate(db)
    }

    static var migrator: DatabaseMigrator {
        var m = DatabaseMigrator()
        m.registerMigration("v1") { db in
            try db.create(table: "conversations") { t in
                t.column("accountKey", .text).notNull()
                t.column("id", .text).notNull()
                t.column("label", .text).notNull()
                t.column("avatarHash", .text).notNull()
                t.column("receivedBy", .text).notNull()
                t.column("counterpartsJSON", .text).notNull()
                t.column("isGroup", .boolean).notNull()
                t.column("lastDateTs", .integer).notNull()
                t.column("lastSubject", .text).notNull()
                t.column("unreadCount", .integer).notNull()
                t.column("totalCount", .integer).notNull()
                t.column("updatedAt", .integer).notNull()
                t.primaryKey(["accountKey", "id"])
            }
            try db.create(table: "conversation_messages") { t in
                t.column("accountKey", .text).notNull()
                t.column("conversationID", .text).notNull()
                t.column("folder", .text).notNull()
                t.column("uid", .integer).notNull()
                t.column("messageID", .text).notNull()
                t.column("seen", .boolean).notNull()
                t.primaryKey(["accountKey", "conversationID", "uid"])
            }
            try db.create(index: "conversation_messages_by_message", on: "conversation_messages", columns: ["accountKey", "messageID"])
            try db.create(table: "message_bodies") { t in
                t.column("accountKey", .text).notNull()
                t.column("uid", .integer).notNull()
                t.column("messageID", .text).notNull()
                t.column("usable", .boolean).notNull()
                t.column("json", .blob).notNull()
                t.primaryKey(["accountKey", "uid"])
            }
            try db.create(index: "message_bodies_by_message", on: "message_bodies", columns: ["accountKey", "messageID"])
            try db.create(table: "meta") { t in
                t.primaryKey("key", .text)
                t.column("value", .text).notNull()
            }
        }
        registerImapTables(&m)
        return m
    }

    // MARK: Беседы

    /// Снимок пространств `accountKeys` (обычно учётка и её спам).
    public func snapshot(_ accountKeys: [String]) throws -> [String: ConversationSnapshot] {
        try db.read { try Self.read($0, accountKeys) }
    }

    /// Снимки при каждом изменении кэша — для списка. Первый — сразу.
    public func observe(_ accountKeys: [String]) -> AsyncStream<[String: ConversationSnapshot]> {
        stream(ValueObservation.tracking { try Self.read($0, accountKeys) }.removeDuplicates())
    }

    /// Наблюдение GRDB → `AsyncStream`: типы GRDB не выходят за модуль.
    /// Ошибка чтения обрывает поток — экран остаётся с последним снимком.
    private func stream<T: Sendable>(_ observation: ValueObservation<ValueReducers.RemoveDuplicates<ValueReducers.Fetch<T>>>) -> AsyncStream<T> where T: Equatable {
        let values = observation.values(in: db)
        return AsyncStream { cont in
            let task = Task {
                do {
                    for try await v in values { cont.yield(v) }
                } catch {}
                cont.finish()
            }
            cont.onTermination = { _ in task.cancel() }
        }
    }

    private static func read(_ db: Database, _ keys: [String]) throws -> [String: ConversationSnapshot] {
        var out: [String: ConversationSnapshot] = [:]
        for k in keys {
            let convs = try ConversationRecord
                .filter(Column("accountKey") == k)
                .order(Column("lastDateTs").desc)
                .fetchAll(db)
            let refs = try MessageRefRecord.filter(Column("accountKey") == k).fetchAll(db)
            out[k] = ConversationSnapshot(conversations: convs, refs: Dictionary(grouping: refs, by: \.conversationID))
        }
        return out
    }

    /// Полная выдача заменяет набор целиком.
    public func replaceAll(_ account: String, _ convs: [ConversationDTO]) throws {
        try db.write { db in
            try MessageRefRecord.filter(Column("accountKey") == account).deleteAll(db)
            try ConversationRecord.filter(Column("accountKey") == account).deleteAll(db)
            try Self.insert(db, account, convs)
        }
    }

    /// Дельта: изменившиеся беседы целиком заменяют свои прежние версии.
    public func upsert(_ account: String, _ convs: [ConversationDTO]) throws {
        try db.write { db in
            let ids = convs.map(\.id)
            try MessageRefRecord
                .filter(Column("accountKey") == account && ids.contains(Column("conversationID")))
                .deleteAll(db)
            try Self.insert(db, account, convs)
        }
    }

    private static func insert(_ db: Database, _ account: String, _ convs: [ConversationDTO]) throws {
        for c in convs {
            try ConversationRecord(accountKey: account, dto: c).insert(db, onConflict: .replace)
            for r in c.messages {
                try MessageRefRecord(accountKey: account, conversationID: c.id, ref: r).insert(db, onConflict: .replace)
            }
        }
    }

    /// Надгробия журнала: письма уходят из бесед, беседы без писем — целиком
    /// (как `apply_deletions` десктопа). Ответ — сколько ссылок удалено.
    @discardableResult
    public func applyDeletions(_ account: String, messageIDs: [String]) throws -> Int {
        let ids = messageIDs.filter { !$0.isEmpty }
        guard !ids.isEmpty else { return 0 }
        return try db.write { db in
            let n = try MessageRefRecord
                .filter(Column("accountKey") == account && ids.contains(Column("messageID")))
                .deleteAll(db)
            try BodyRecord
                .filter(Column("accountKey") == account && ids.contains(Column("messageID")))
                .deleteAll(db)
            try db.execute(sql: """
                DELETE FROM conversations WHERE accountKey = ? AND id NOT IN
                (SELECT DISTINCT conversationID FROM conversation_messages WHERE accountKey = ?)
                """, arguments: [account, account])
            return n
        }
    }

    /// Беседы удалены целиком (оптимистично, до ответа журнала).
    public func deleteConversations(_ account: String, ids: [String]) throws {
        try db.write { db in
            try MessageRefRecord.filter(Column("accountKey") == account && ids.contains(Column("conversationID"))).deleteAll(db)
            try ConversationRecord.filter(Column("accountKey") == account && ids.contains(Column("id"))).deleteAll(db)
        }
    }

    /// Беседу открыли — прочитана локально сразу, сервер — следом.
    public func markRead(_ account: String, conversation: String) throws {
        try db.write { db in
            try db.execute(sql: "UPDATE conversation_messages SET seen = 1 WHERE accountKey = ? AND conversationID = ?", arguments: [account, conversation])
            try db.execute(sql: "UPDATE conversations SET unreadCount = 0 WHERE accountKey = ? AND id = ?", arguments: [account, conversation])
        }
    }

    /// «Непрочитано»: письмо `uid` снова непрочитано.
    public func markUnread(_ account: String, conversation: String, uid: Int64) throws {
        try db.write { db in
            try db.execute(sql: "UPDATE conversation_messages SET seen = 0 WHERE accountKey = ? AND conversationID = ? AND uid = ?", arguments: [account, conversation, uid])
            try db.execute(sql: "UPDATE conversations SET unreadCount = MAX(unreadCount, 1) WHERE accountKey = ? AND id = ?", arguments: [account, conversation])
        }
    }

    // MARK: Тела писем

    /// Тела по uid; пустые (непригодные) не отдаются — их надо перезапросить.
    public func bodies(_ account: String, uids: [Int64]) throws -> [Int64: MessageBody] {
        try db.read { db in
            let rows = try BodyRecord
                .filter(Column("accountKey") == account && uids.contains(Column("uid")) && Column("usable") == true)
                .fetchAll(db)
            var out: [Int64: MessageBody] = [:]
            for r in rows {
                if let b = try? JSONDecoder().decode(MessageBody.self, from: r.json) { out[r.uid] = b }
            }
            return out
        }
    }

    public func putBodies(_ account: String, _ bodies: [MessageBody]) throws {
        try db.write { db in
            for b in bodies {
                try BodyRecord(
                    accountKey: account, uid: b.uid, messageID: b.messageID,
                    usable: !b.isBlank, json: JSONEncoder().encode(b)
                ).insert(db, onConflict: .replace)
            }
        }
    }

    /// Какие тела уже в кэше (непустые) — для докачки; сами тела не читаем.
    public func cachedBodyUIDs(_ account: String) throws -> Set<Int64> {
        try db.read { db in
            Set(try Int64.fetchAll(db, sql: "SELECT uid FROM message_bodies WHERE accountKey = ? AND usable = 1", arguments: [account]))
        }
    }

    // MARK: Служебное

    public func meta(_ key: String) throws -> String? {
        try db.read { try MetaRecord.fetchOne($0, key: key)?.value }
    }

    public func setMeta(_ key: String, _ value: String) throws {
        try db.write { try MetaRecord(key: key, value: value).insert($0, onConflict: .replace) }
    }

    public func observeMeta(_ key: String) -> AsyncStream<String?> {
        stream(ValueObservation.tracking { try MetaRecord.fetchOne($0, key: key)?.value }.removeDuplicates())
    }

    /// Выход из учётки: кэш чужой учётки не остаётся на устройстве.
    public func clearAll() throws {
        try db.write { db in
            for t in ["conversations", "conversation_messages", "message_bodies", "meta", "imap_messages", "imap_folders"] {
                try db.execute(sql: "DELETE FROM \(t)")
            }
        }
    }
}
