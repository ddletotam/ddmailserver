import Foundation

/// Кадр WebSocket `/ws` (`WSEvent` сервера, `handlers_desktop_ws.go`). Почти
/// всем событиям хватает типа — клиент по нему синхронизируется; поля нужны
/// тем, что несут данные: `message_sent` и `delivery_status` (десктоп §1).
public struct ServerEvent: Decodable, Equatable, Sendable {
    public static let messageSent = "message_sent"
    public static let deliveryStatus = "delivery_status"
    public static let calendarUpdated = "calendar_updated"
    /// Не кадр сервера: `SessionAPI.events` сообщает так, что сокет открыт
    /// (понг на пинг). Переподключение — повод к синку: пропущенное за обрыв
    /// иначе приедет только со следующим событием.
    public static let opened = "_opened"

    public var type: String
    /// Серверный id письма (у учётки ddmail — его `uid` в беседах).
    public var messageID: Int64
    /// Message-ID письма без угловых скобок.
    public var rfcMessageID: String
    public var deliveryStatus: String
    public var deliveryReason: String
    public var deliveryError: String
    public var deliveryFailed: [String]

    enum CodingKeys: String, CodingKey {
        case type
        case messageID = "message_id"
        case rfcMessageID = "rfc_message_id"
        case deliveryStatus = "delivery_status"
        case deliveryReason = "delivery_reason"
        case deliveryError = "delivery_error"
        case deliveryFailed = "delivery_failed"
    }

    public init(type: String, messageID: Int64 = 0, rfcMessageID: String = "", deliveryStatus: String = "",
                deliveryReason: String = "", deliveryError: String = "", deliveryFailed: [String] = []) {
        self.type = type; self.messageID = messageID; self.rfcMessageID = rfcMessageID
        self.deliveryStatus = deliveryStatus; self.deliveryReason = deliveryReason
        self.deliveryError = deliveryError; self.deliveryFailed = deliveryFailed
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        type = try c.value(.type, "")
        messageID = try c.value(.messageID, 0)
        rfcMessageID = try c.value(.rfcMessageID, "")
        deliveryStatus = try c.value(.deliveryStatus, "")
        deliveryReason = try c.value(.deliveryReason, "")
        deliveryError = try c.value(.deliveryError, "")
        deliveryFailed = try c.value(.deliveryFailed, [])
    }

    /// Кадр сокета; без типа или не JSON — `nil` (пропустить).
    public static func parse(_ data: Data) -> ServerEvent? {
        guard let e = try? JSONDecoder().decode(ServerEvent.self, from: data), !e.type.isEmpty else { return nil }
        return e
    }

    /// Смена статуса доставки — только у события `delivery_status` с адресом письма.
    public var delivery: DeliveryUpdate? {
        guard type == Self.deliveryStatus, messageID > 0 || !rfcMessageID.isEmpty else { return nil }
        return DeliveryUpdate(
            uid: messageID, messageID: rfcMessageID, status: deliveryStatus,
            reason: deliveryReason, error: deliveryError, failed: deliveryFailed
        )
    }
}

/// Новый статус доставки письма — те же поля, что в теле письма.
public struct DeliveryUpdate: Equatable, Sendable {
    /// Серверный id письма; 0 — неизвестен (тогда — по `messageID`).
    public var uid: Int64
    public var messageID: String
    public var status: String
    public var reason: String
    public var error: String
    public var failed: [String]

    public init(uid: Int64, messageID: String = "", status: String, reason: String = "", error: String = "", failed: [String] = []) {
        self.uid = uid; self.messageID = messageID; self.status = status
        self.reason = reason; self.error = error; self.failed = failed
    }
}
