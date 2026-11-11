import DDMailAPI

/// Беседа для UI. Семантика — сервера и десктопного контракта (§4).
public struct Conversation: Equatable, Hashable, Identifiable, Sendable {
    public var accountKey: String
    public var id: String
    public var label: String
    public var receivedBy: String
    public var counterparts: [ContactInfo]
    public var isGroup: Bool
    /// Секунды.
    public var lastDateTs: Int64
    public var lastSubject: String
    public var unreadCount: Int
    public var totalCount: Int
    public var messages: [MessageRef]
    /// Из спама: место — только системная папка «Спам» (её состав задаёт сервер).
    public var spam: Bool

    public init(
        accountKey: String, id: String, label: String = "", receivedBy: String = "",
        counterparts: [ContactInfo] = [], isGroup: Bool = false, lastDateTs: Int64 = 0,
        lastSubject: String = "", unreadCount: Int = 0, totalCount: Int = 0,
        messages: [MessageRef] = [], spam: Bool = false
    ) {
        self.accountKey = accountKey; self.id = id; self.label = label; self.receivedBy = receivedBy
        self.counterparts = counterparts; self.isGroup = isGroup; self.lastDateTs = lastDateTs
        self.lastSubject = lastSubject; self.unreadCount = unreadCount; self.totalCount = totalCount
        self.messages = messages; self.spam = spam
    }

    public var key: DialogKey { DialogKey(account: accountKey, id: id) }
}

/// Ключ ручного назначения в папку — как `MergeKey` десктопа: `{account, id}`,
/// где `account` = `логин@хост`.
public struct DialogKey: Equatable, Hashable, Sendable {
    public var account: String
    public var id: String

    public init(account: String, id: String) {
        self.account = account
        self.id = id
    }
}
