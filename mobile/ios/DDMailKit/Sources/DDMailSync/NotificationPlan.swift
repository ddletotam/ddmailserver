/// Что показать после синка (Android §5, §7; порт `MailNotifier.check`).
///
/// «Новое» — непрочитанные письма с uid больше последнего, о котором уже
/// уведомили; первый проход лишь запоминает рубеж (иначе после входа
/// посыпались бы уведомления обо всём непрочитанном). Не уведомляем о беседе,
/// открытой на экране, и о скрытых папках; спам молчит всегда — ни
/// уведомления, ни бейджа.
public struct NotificationPlan: Equatable, Sendable {
    public struct Item: Equatable, Sendable {
        public let conversation: Conversation
        /// Сколько новых писем в беседе.
        public let count: Int
    }

    /// Бейдж на иконке — беседы с непрочитанным вне скрытых папок (как точка
    /// трея десктопа: беседы, а не письма).
    public let badge: Int
    /// Новый рубеж: самый большой uid из известных.
    public let watermark: Int64?
    public let items: [Item]

    public init(conversations all: [Conversation], folders: FoldersDoc, openID: String?, previousWatermark: Int64?) {
        let convs = all.filter { !$0.spam }
        func hidden(_ c: Conversation) -> Bool {
            folders.folderOf(c).map { folders.folders[$0].hidden } ?? false
        }
        badge = convs.filter { $0.unreadCount > 0 && !hidden($0) }.count
        let top = convs.flatMap { $0.messages.map(\.uid) }.max()
        watermark = top.map { max($0, previousWatermark ?? 0) } ?? previousWatermark
        guard let last = previousWatermark, top != nil else {
            items = []
            return
        }
        items = convs.compactMap { c in
            guard c.id != openID, !hidden(c) else { return nil }
            let n = c.messages.filter { !$0.seen && $0.uid > last }.count
            return n > 0 ? Item(conversation: c, count: n) : nil
        }
    }

    /// Текст уведомления — собирается на устройстве из своего кэша (152-ФЗ:
    /// через push ни отправитель, ни тема не идут).
    public static func text(_ item: Item) -> (title: String, body: String) {
        let c = item.conversation
        let title = c.label.isEmpty ? (c.counterparts.first?.addr ?? "") : c.label
        let body = item.count > 1 ? "\(c.lastSubject) · новых: \(item.count)" : c.lastSubject
        return (title, body)
    }
}
