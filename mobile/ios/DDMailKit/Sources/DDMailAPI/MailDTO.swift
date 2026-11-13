import Foundation

// Почта: зеркало Dto.kt Android и Desktop*-структур сервера.

public struct ContactInfo: Codable, Equatable, Hashable, Sendable {
    public var name: String
    public var addr: String

    public init(name: String = "", addr: String = "") {
        self.name = name
        self.addr = addr
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.value(.name, "")
        addr = try c.value(.addr, "")
    }
}

/// Ссылка на письмо беседы. `uid` у учётки ddmail — серверный id письма.
public struct MessageRef: Codable, Equatable, Hashable, Sendable {
    public var folder: String
    public var uid: Int64
    /// Стабильный ключ письма (RFC Message-ID); uid — волатильный.
    public var messageID: String
    public var seen: Bool
    /// Статус доставки своего исходящего (`delivery_status`, контракт §5):
    /// пусто — неизвестно (входящие, старые письма) = доставлено.
    public var deliveryStatus: String

    enum CodingKeys: String, CodingKey {
        case folder, uid, seen
        case messageID = "message_id"
        case deliveryStatus = "delivery_status"
    }

    public init(folder: String = "", uid: Int64, messageID: String = "", seen: Bool = false, deliveryStatus: String = "") {
        self.folder = folder
        self.uid = uid
        self.messageID = messageID
        self.seen = seen
        self.deliveryStatus = deliveryStatus
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        folder = try c.value(.folder, "")
        uid = try c.value(.uid, 0)
        messageID = try c.value(.messageID, "")
        seen = try c.value(.seen, false)
        deliveryStatus = try c.value(.deliveryStatus, "")
    }

    /// Ссылка уходит и в запросы (`/conversations/messages`, флаги): статус —
    /// только если он есть, форма запроса прежняя.
    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(folder, forKey: .folder)
        try c.encode(uid, forKey: .uid)
        try c.encode(messageID, forKey: .messageID)
        try c.encode(seen, forKey: .seen)
        if !deliveryStatus.isEmpty { try c.encode(deliveryStatus, forKey: .deliveryStatus) }
    }
}

/// Статус доставки исходящего письма (`delivery_status` сервера, десктоп §1).
/// Пустое и незнакомое значение — `.unknown`: считать доставленным, ничего
/// не рисовать.
public enum DeliveryState: String, Equatable, Sendable {
    case unknown = ""
    case pending
    case delivered
    case failed

    public init(_ raw: String) {
        self = DeliveryState(rawValue: raw.lowercased()) ?? .unknown
    }
}

/// Message-ID без угловых скобок и регистра: сервер отдаёт его в событиях
/// без скобок, а в ссылках писем — как в заголовке.
public func normalizedMessageID(_ s: String) -> String {
    s.trimmingCharacters(in: CharacterSet(charactersIn: "<> \t")).lowercased()
}

public struct ConversationDTO: Decodable, Equatable, Sendable {
    /// Задаёт сервер (набор адресов); клиент его не разбирает.
    public var id: String
    public var label: String
    public var avatarHash: String
    public var receivedBy: String
    public var counterparts: [ContactInfo]
    public var isGroup: Bool
    /// Секунды.
    public var lastDateTs: Int64
    public var lastSubject: String
    public var unreadCount: Int
    public var totalCount: Int
    public var messages: [MessageRef]
    /// Миллисекунды, часы сервера.
    public var updatedAt: Int64
    /// Беседа из спама (`/conversations?spam=1`).
    public var spam: Bool
    /// Статус доставки последнего письма беседы, если оно наше исходящее
    /// (`last_delivery_status`); пусто — показывать нечего.
    public var lastDeliveryStatus: String

    public init(
        id: String, label: String = "", avatarHash: String = "", receivedBy: String = "", counterparts: [ContactInfo] = [],
        isGroup: Bool = false, lastDateTs: Int64 = 0, lastSubject: String = "", unreadCount: Int = 0, totalCount: Int = 0,
        messages: [MessageRef] = [], updatedAt: Int64 = 0, spam: Bool = false, lastDeliveryStatus: String = ""
    ) {
        self.id = id; self.label = label; self.avatarHash = avatarHash; self.receivedBy = receivedBy
        self.counterparts = counterparts; self.isGroup = isGroup; self.lastDateTs = lastDateTs
        self.lastSubject = lastSubject; self.unreadCount = unreadCount; self.totalCount = totalCount
        self.messages = messages; self.updatedAt = updatedAt; self.spam = spam
        self.lastDeliveryStatus = lastDeliveryStatus
    }

    enum CodingKeys: String, CodingKey {
        case id, label, counterparts, messages, spam
        case avatarHash = "avatar_hash"
        case receivedBy = "received_by"
        case isGroup = "is_group"
        case lastDateTs = "last_date_ts"
        case lastSubject = "last_subject"
        case unreadCount = "unread_count"
        case totalCount = "total_count"
        case updatedAt = "updated_at"
        case lastDeliveryStatus = "last_delivery_status"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        label = try c.value(.label, "")
        avatarHash = try c.value(.avatarHash, "")
        receivedBy = try c.value(.receivedBy, "")
        counterparts = try c.value(.counterparts, [])
        isGroup = try c.value(.isGroup, false)
        lastDateTs = try c.value(.lastDateTs, 0)
        lastSubject = try c.value(.lastSubject, "")
        unreadCount = try c.value(.unreadCount, 0)
        totalCount = try c.value(.totalCount, 0)
        messages = try c.value(.messages, [])
        updatedAt = try c.value(.updatedAt, 0)
        spam = try c.value(.spam, false)
        lastDeliveryStatus = try c.value(.lastDeliveryStatus, "")
    }
}

/// `/conversations?since=` — изменившиеся беседы и водяной знак часов сервера.
public struct ConversationsDelta: Decodable, Sendable {
    public var serverNowMs: Int64
    public var conversations: [ConversationDTO]

    enum CodingKeys: String, CodingKey {
        case conversations
        case serverNowMs = "server_now_ms"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        serverNowMs = try c.value(.serverNowMs, 0)
        conversations = try c.value(.conversations, [])
    }
}

public struct ChangeEntry: Decodable, Equatable, Sendable {
    public static let upsert = 1
    public static let delete = 2

    public var seq: Int64
    public var messageID: String
    public var kind: Int

    enum CodingKeys: String, CodingKey {
        case seq, kind
        case messageID = "message_id"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        seq = try c.value(.seq, 0)
        messageID = try c.value(.messageID, "")
        kind = try c.value(.kind, 0)
    }
}

/// Хвост журнала изменений; `reset` — курсор негоден, нужна полная синхронизация.
public struct ChangesResponse: Decodable, Sendable {
    public var entries: [ChangeEntry]
    public var latestSeq: Int64
    public var reset: Bool

    enum CodingKeys: String, CodingKey {
        case entries, reset
        case latestSeq = "latest_seq"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        entries = try c.value(.entries, [])
        latestSeq = try c.value(.latestSeq, 0)
        reset = try c.value(.reset, false)
    }
}

/// Документ, который сервер хранит с версией и не толкует (`/dialog-folders`,
/// `/identity-groups`).
public struct VersionedDoc: Decodable, Sendable {
    public var version: Int64
    public var doc: JSONValue?

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        version = try c.value(.version, 0)
        doc = try c.decodeIfPresent(JSONValue.self, forKey: .doc)
    }

    enum CodingKeys: String, CodingKey { case version, doc }
}

public struct AttachmentDTO: Codable, Equatable, Hashable, Sendable {
    public var filename: String
    public var mimeType: String
    public var size: Int64
    public var index: Int

    enum CodingKeys: String, CodingKey {
        case filename, size, index
        case mimeType = "mime_type"
    }

    public init(filename: String = "", mimeType: String = "", size: Int64 = 0, index: Int = 0) {
        self.filename = filename
        self.mimeType = mimeType
        self.size = size
        self.index = index
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        filename = try c.value(.filename, "")
        mimeType = try c.value(.mimeType, "")
        size = try c.value(.size, 0)
        index = try c.value(.index, 0)
    }
}

/// Тело письма (`/conversations/messages`).
public struct MessageBody: Codable, Equatable, Sendable {
    public var uid: Int64
    public var folder: String
    public var subject: String
    public var from: String
    public var fromAddr: String
    public var to: [String]
    public var cc: [String]
    /// Секунды.
    public var dateTs: Int64
    public var html: String?
    public var text: String?
    public var attachments: [AttachmentDTO]
    public var isOutgoing: Bool
    public var messageID: String
    public var inReplyTo: String
    public var references: [String]
    /// Доставка своего исходящего (десктоп §1): `delivery_status` — `""`
    /// (неизвестно = доставлено), `pending`, `delivered`, `failed`;
    /// `delivery_reason` — код причины, `delivery_error` — она же словами на
    /// языке пользователя, `delivery_failed` — кому не доставлено.
    public var deliveryStatus: String
    public var deliveryReason: String
    public var deliveryError: String
    public var deliveryFailed: [String]

    enum CodingKeys: String, CodingKey {
        case uid, folder, subject, from, to, cc, html, text, attachments, references
        case fromAddr = "from_addr"
        case dateTs = "date_ts"
        case isOutgoing = "is_outgoing"
        case messageID = "message_id"
        case inReplyTo = "in_reply_to"
        case deliveryStatus = "delivery_status"
        case deliveryReason = "delivery_reason"
        case deliveryError = "delivery_error"
        case deliveryFailed = "delivery_failed"
    }

    public init(
        uid: Int64, folder: String = "", subject: String = "", from: String = "", fromAddr: String = "",
        to: [String] = [], cc: [String] = [], dateTs: Int64 = 0, html: String? = nil, text: String? = nil,
        attachments: [AttachmentDTO] = [], isOutgoing: Bool = false, messageID: String = "",
        inReplyTo: String = "", references: [String] = [],
        deliveryStatus: String = "", deliveryReason: String = "", deliveryError: String = "", deliveryFailed: [String] = []
    ) {
        self.uid = uid; self.folder = folder; self.subject = subject; self.from = from
        self.fromAddr = fromAddr; self.to = to; self.cc = cc; self.dateTs = dateTs; self.html = html
        self.text = text; self.attachments = attachments; self.isOutgoing = isOutgoing
        self.messageID = messageID; self.inReplyTo = inReplyTo; self.references = references
        self.deliveryStatus = deliveryStatus; self.deliveryReason = deliveryReason
        self.deliveryError = deliveryError; self.deliveryFailed = deliveryFailed
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        uid = try c.decode(Int64.self, forKey: .uid)
        folder = try c.value(.folder, "")
        subject = try c.value(.subject, "")
        from = try c.value(.from, "")
        fromAddr = try c.value(.fromAddr, "")
        to = try c.value(.to, [])
        cc = try c.value(.cc, [])
        dateTs = try c.value(.dateTs, 0)
        html = try c.decodeIfPresent(String.self, forKey: .html)
        text = try c.decodeIfPresent(String.self, forKey: .text)
        attachments = try c.value(.attachments, [])
        isOutgoing = try c.value(.isOutgoing, false)
        messageID = try c.value(.messageID, "")
        inReplyTo = try c.value(.inReplyTo, "")
        references = try c.value(.references, [])
        deliveryStatus = try c.value(.deliveryStatus, "")
        deliveryReason = try c.value(.deliveryReason, "")
        deliveryError = try c.value(.deliveryError, "")
        deliveryFailed = try c.value(.deliveryFailed, [])
    }

    public var delivery: DeliveryState { DeliveryState(deliveryStatus) }

    /// Статус доставки из события сервера — без перезапроса тела.
    public mutating func apply(_ d: DeliveryUpdate) {
        deliveryStatus = d.status
        deliveryReason = d.reason
        deliveryError = d.error
        deliveryFailed = d.failed
    }

    /// Ни HTML, ни текста, ни вложений — сбойный фетч, перезапросить (§4а десктопа).
    public var isBlank: Bool {
        (html ?? "").trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
            && (text ?? "").trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
            && attachments.isEmpty
    }
}

/// Итог «В спам»: сколько писем удалено и какое правило заведено первым.
public struct PurgeOutcome: Decodable, Equatable, Sendable {
    public var deleted: Int64
    public var ruleType: String
    public var ruleValue: String
    public var ruleCount: Int

    enum CodingKeys: String, CodingKey {
        case deleted
        case ruleType = "rule_type"
        case ruleValue = "rule_value"
        case ruleCount = "rule_count"
    }

    public init(deleted: Int64 = 0, ruleType: String = "", ruleValue: String = "", ruleCount: Int = 0) {
        self.deleted = deleted; self.ruleType = ruleType; self.ruleValue = ruleValue; self.ruleCount = ruleCount
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        deleted = try c.value(.deleted, 0)
        ruleType = try c.value(.ruleType, "")
        ruleValue = try c.value(.ruleValue, "")
        ruleCount = try c.value(.ruleCount, 0)
    }
}

/// Двоичный ответ: аватар, часть письма, вложение.
public struct Binary: Sendable {
    public let data: Data
    public let contentType: String

    public init(data: Data, contentType: String) {
        self.data = data
        self.contentType = contentType
    }
}

/// Вложение исходящего письма; байты в JSON — base64 (как `[]byte` у Go).
public struct OutgoingAttachment: Encodable, Equatable, Sendable {
    public var filename: String
    public var mimeType: String
    public var content: Data
    /// Картинка в теле (`cid:`) — сервер кладёт её inline в `multipart/related`.
    public var contentID: String?

    enum CodingKeys: String, CodingKey {
        case filename, content
        case mimeType = "mime_type"
        case contentID = "content_id"
    }

    public init(filename: String, mimeType: String, content: Data, contentID: String? = nil) {
        self.filename = filename
        self.mimeType = mimeType
        self.content = content
        self.contentID = contentID
    }
}

/// Итог отправки: ответ `/send` или SMTP IMAP-учётки. `rfcMessageID` —
/// Message-ID копии в «Отправленных» (по нему узнаётся беседа нового
/// письма), `deliveryStatus` — у ddmail `pending` (принято в очередь), у
/// IMAP-учётки `delivered` (SMTP провайдера принял). Старый сервер полей
/// не отдаёт — пусто.
public struct SendReceipt: Decodable, Equatable, Sendable {
    public var rfcMessageID: String
    public var deliveryStatus: String

    enum CodingKeys: String, CodingKey {
        case rfcMessageID = "rfc_message_id"
        case deliveryStatus = "delivery_status"
    }

    public init(rfcMessageID: String = "", deliveryStatus: String = "") {
        self.rfcMessageID = rfcMessageID
        self.deliveryStatus = deliveryStatus
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        rfcMessageID = try c.value(.rfcMessageID, "")
        deliveryStatus = try c.value(.deliveryStatus, "")
    }
}

/// Тело `/send`. Отправка асинхронная: сервер ставит письмо в очередь.
public struct SendRequest: Encodable, Equatable, Sendable {
    public var from: String
    public var to: [String]
    public var cc: [String]
    public var subject: String
    public var html: String
    public var text: String
    public var inReplyTo: String
    public var references: String
    public var attachments: [OutgoingAttachment]

    enum CodingKeys: String, CodingKey {
        case from, to, cc, subject, html, text, references, attachments
        case inReplyTo = "in_reply_to"
    }

    public init(
        from: String, to: [String], cc: [String] = [], subject: String, html: String, text: String,
        inReplyTo: String = "", references: String = "", attachments: [OutgoingAttachment] = []
    ) {
        self.from = from; self.to = to; self.cc = cc; self.subject = subject; self.html = html
        self.text = text; self.inReplyTo = inReplyTo; self.references = references; self.attachments = attachments
    }
}

/// Письмо из серверного поиска (`GET /search`, `search.IndexDocument`):
/// секция «Сообщения» выпадашки поиска, как `search_messages` десктопа.
public struct MessageSearchHit: Decodable, Equatable, Identifiable, Sendable {
    /// Серверный id письма — он же `uid` в беседах учётки ddmail.
    public var id: Int64
    public var subject: String
    public var from: String
    /// Секунды.
    public var date: Int64
    public var messageID: String
    /// `email` или `calendar`.
    public var type: String
    public var softDeleted: Bool
    public var spamStatus: String

    enum CodingKeys: String, CodingKey {
        case id, subject, from, date, type
        case messageID = "message_id"
        case softDeleted = "soft_deleted"
        case spamStatus = "spam_status"
    }

    public init(id: Int64, subject: String = "", from: String = "", date: Int64 = 0, messageID: String = "",
                type: String = "email", softDeleted: Bool = false, spamStatus: String = "") {
        self.id = id; self.subject = subject; self.from = from; self.date = date; self.messageID = messageID
        self.type = type; self.softDeleted = softDeleted; self.spamStatus = spamStatus
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.value(.id, 0)
        subject = try c.value(.subject, "")
        from = try c.value(.from, "")
        date = try c.value(.date, 0)
        messageID = try c.value(.messageID, "")
        type = try c.value(.type, "email")
        softDeleted = try c.value(.softDeleted, false)
        spamStatus = try c.value(.spamStatus, "")
    }
}
