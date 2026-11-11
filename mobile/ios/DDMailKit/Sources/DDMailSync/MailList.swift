import Foundation

/// Таб папки над списком (Android §5): у «Спама» счётчика нет.
public struct FolderTab: Equatable, Identifiable, Sendable {
    public let index: Int
    public let id: String
    public let name: String
    public let icon: String
    public let unread: Int
}

/// Что видно в списке: табы, выбранная папка, строки.
public struct MailListState: Equatable, Sendable {
    public var tabs: [FolderTab] = []
    /// Индекс выбранной папки; `nil` — «Входящие» (таба «Все» нет).
    public var active: Int?
    public var inboxUnread = 0
    public var conversations: [Conversation] = []
    public var inSpam = false
    public var searching = false

    public init() {}
}

/// Состояние списка (порт `MailViewModel.build` Android).
///
/// - Поиск — по всем папкам, спам в поиск не попадает.
/// - Удалённая папка — снова инбокс. Скрытая видна табом, только пока в неё
///   привели (открытие беседы из неё, переход в «Спам» из меню).
public func buildMailList(_ convs: [Conversation], doc: FoldersDoc, activeID: String?, query: String?) -> MailListState {
    var s = MailListState()
    if let q = query, !q.trimmingCharacters(in: .whitespaces).isEmpty {
        s.searching = true
        s.conversations = searchConversations(convs.filter { !$0.spam }, q)
        return s
    }
    let active = activeID.flatMap { id in doc.folders.firstIndex { $0.id == id } }
    let counts = doc.unreadCounts(convs)
    s.tabs = doc.folders.enumerated().compactMap { i, f in
        if f.hidden && i != active { return nil }
        // Непрочитанный спам — не повод на него смотреть.
        return FolderTab(index: i, id: f.id, name: f.name, icon: f.icon, unread: f.system == systemSpam ? 0 : counts.folders[i])
    }
    s.active = active
    s.inboxUnread = counts.inbox
    s.conversations = doc.filter(convs, active: active)
    s.inSpam = active != nil && active == doc.spamIndex
    return s
}

/// Локальный поиск по диалогам (Android §5): имя, адрес любого участника,
/// тема последнего письма; порядок — сначала совпавшие по имени, затем по
/// адресу, затем по теме, внутри — как в списке. Без учёта регистра и
/// диакритики (кириллица складывается правильно, «ё» находит «е»).
public func searchConversations(_ convs: [Conversation], _ q: String) -> [Conversation] {
    let needle = fold(q.trimmingCharacters(in: .whitespacesAndNewlines))
    guard !needle.isEmpty else { return convs }
    func has(_ s: String) -> Bool { fold(s).contains(needle) }
    var taken = Set<String>()
    let byName = convs.filter { has($0.label) }
    byName.forEach { taken.insert($0.id) }
    let byAddr = convs.filter { c in
        !taken.contains(c.id)
            && (c.counterparts.contains { has($0.addr) || has($0.name) } || has(c.receivedBy))
    }
    byAddr.forEach { taken.insert($0.id) }
    let bySubject = convs.filter { !taken.contains($0.id) && has($0.lastSubject) }
    return byName + byAddr + bySubject
}

private func fold(_ s: String) -> String {
    s.folding(options: [.caseInsensitive, .diacriticInsensitive, .widthInsensitive], locale: nil)
}
