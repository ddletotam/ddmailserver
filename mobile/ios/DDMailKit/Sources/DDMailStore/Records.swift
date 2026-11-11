import DDMailAPI
import Foundation
import GRDB

// Записи кэша — аналог сущностей Room Android (`CacheDb.kt`). Пространство
// учётки — `accountKey`; спам живёт в своём (`<account>#spam`), как у
// десктопа и Android: журнал считает спам удалением.

public struct ConversationRecord: Codable, Equatable, Sendable, FetchableRecord, PersistableRecord {
    public static let databaseTableName = "conversations"

    public var accountKey: String
    public var id: String
    public var label: String
    public var avatarHash: String
    public var receivedBy: String
    public var counterpartsJSON: String
    public var isGroup: Bool
    public var lastDateTs: Int64
    public var lastSubject: String
    public var unreadCount: Int
    public var totalCount: Int
    public var updatedAt: Int64

    public init(accountKey: String, dto: ConversationDTO) {
        self.accountKey = accountKey
        id = dto.id
        label = dto.label
        avatarHash = dto.avatarHash
        receivedBy = dto.receivedBy
        counterpartsJSON = String(decoding: (try? JSONEncoder().encode(dto.counterparts)) ?? Data("[]".utf8), as: UTF8.self)
        isGroup = dto.isGroup
        lastDateTs = dto.lastDateTs
        lastSubject = dto.lastSubject
        unreadCount = dto.unreadCount
        totalCount = dto.totalCount
        updatedAt = dto.updatedAt
    }

    public var counterparts: [ContactInfo] {
        (try? JSONDecoder().decode([ContactInfo].self, from: Data(counterpartsJSON.utf8))) ?? []
    }
}

public struct MessageRefRecord: Codable, Equatable, Sendable, FetchableRecord, PersistableRecord {
    public static let databaseTableName = "conversation_messages"

    public var accountKey: String
    public var conversationID: String
    public var folder: String
    public var uid: Int64
    public var messageID: String
    public var seen: Bool

    public init(accountKey: String, conversationID: String, ref: MessageRef) {
        self.accountKey = accountKey
        self.conversationID = conversationID
        folder = ref.folder
        uid = ref.uid
        messageID = ref.messageID
        seen = ref.seen
    }

    public var ref: MessageRef { MessageRef(folder: folder, uid: uid, messageID: messageID, seen: seen) }
}

/// Тело письма — целиком JSON (его форма — ответ сервера), плюс столбцы для
/// поиска по ключу и докачки.
struct BodyRecord: Codable, FetchableRecord, PersistableRecord {
    static let databaseTableName = "message_bodies"

    var accountKey: String
    var uid: Int64
    var messageID: String
    /// Непустое тело (§4а десктопа: пустое перезапрашивается).
    var usable: Bool
    var json: Data
}

struct MetaRecord: Codable, FetchableRecord, PersistableRecord {
    static let databaseTableName = "meta"
    var key: String
    var value: String
}
