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
    /// Новые рубежи по учёткам: самый большой uid из известных. У каждой
    /// учётки своя нумерация писем, поэтому и рубеж свой.
    public let watermarks: [String: Int64]
    public let items: [Item]

    /// `previous` — рубежи прошлого прохода; учётка без рубежа в первом проходе
    /// только запоминает его (иначе после входа посыпались бы уведомления обо
    /// всём непрочитанном). `openRef` — открытая беседа (`Conversation.ref`).
    public init(conversations all: [Conversation], folders: FoldersDoc, openRef: String?, previous: [String: Int64]) {
        let convs = all.filter { !$0.spam }
        func hidden(_ c: Conversation) -> Bool {
            folders.folderOf(c).map { folders.folders[$0].hidden } ?? false
        }
        badge = convs.filter { $0.unreadCount > 0 && !hidden($0) }.count
        var marks = previous
        var out: [Item] = []
        for (account, list) in Dictionary(grouping: convs, by: \.accountKey) {
            let top = list.flatMap { $0.messages.map(\.uid) }.max()
            if let top { marks[account] = max(top, previous[account] ?? 0) }
            guard let last = previous[account], top != nil else { continue }
            for c in list {
                guard c.ref != openRef, !hidden(c) else { continue }
                let n = c.messages.filter { !$0.seen && $0.uid > last }.count
                if n > 0 { out.append(Item(conversation: c, count: n)) }
            }
        }
        watermarks = marks
        items = out.sorted { $0.conversation.lastDateTs > $1.conversation.lastDateTs }
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
