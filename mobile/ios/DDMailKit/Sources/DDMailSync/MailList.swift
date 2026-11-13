import Foundation

/// Таб папки над списком (Android §5): у «Спама» счётчика нет.
public struct FolderTab: Equatable, Identifiable, Sendable {
    public let index: Int
    public let id: String
    public let name: String
    public let icon: String
    public let unread: Int
    /// «Не показывать в списке», а таб виден («Показать скрытые» или в
    /// папку привели) — рисуется бледнее.
    public var hidden = false
    /// Системная («Спам»): ни «Настроить…», ни «Удалить».
    public var system = false
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
///   привели (открытие беседы из неё) или включено «Показать скрытые»
///   (`showHidden`, на сессию — десктоп §4).
/// - Системная «Спам» — всегда таб полосы, последним (`docs/mobile-layout.md`):
///   у мобильных клиентов нет меню, откуда в неё прийти иначе.
public func buildMailList(_ convs: [Conversation], doc: FoldersDoc, activeID: String?, query: String?, showHidden: Bool = false) -> MailListState {
    var s = MailListState()
    if let q = query, !q.trimmingCharacters(in: .whitespaces).isEmpty {
        s.searching = true
        s.conversations = searchConversations(convs.filter { !$0.spam }, q)
        return s
    }
    let active = activeID.flatMap { id in doc.folders.firstIndex { $0.id == id } }
    let counts = doc.unreadCounts(convs)
    s.tabs = doc.folders.enumerated().compactMap { i, f in
        let spam = f.system == systemSpam
        if f.hidden && i != active && !spam && !showHidden { return nil }
        // Непрочитанный спам — не повод на него смотреть.
        return FolderTab(index: i, id: f.id, name: f.name, icon: f.icon, unread: spam ? 0 : counts.folders[i],
                         hidden: f.hidden && !spam, system: f.isSystem)
    }
    s.active = active
    s.inboxUnread = counts.inbox
    s.conversations = doc.filter(convs, active: active)
    s.inSpam = active != nil && active == doc.spamIndex
    return s
}

/// Найденный диалог выпадашки поиска и что в нём совпало — вторая строка
/// (десктоп §4 «Поиск»).
public struct ConversationHit: Equatable, Sendable {
    public let conversation: Conversation
    /// Совпавшее: адрес участника или тема; пусто — совпало имя.
    public let matched: String
}

/// Локальный поиск по диалогам с пояснением, что совпало: имя (вторая строка —
/// адрес собеседника), адрес любого участника, тема. Порядок — как у
/// `searchConversations`.
public func searchConversationHits(_ convs: [Conversation], _ q: String) -> [ConversationHit] {
    let needle = fold(q.trimmingCharacters(in: .whitespacesAndNewlines))
    guard !needle.isEmpty else { return [] }
    func has(_ s: String) -> Bool { fold(s).contains(needle) }
    return searchConversations(convs, q).map { c in
        if has(c.label) {
            // Имя — сам адрес: второй строкой его не повторяем.
            let addr = c.counterparts.first?.addr ?? ""
            return ConversationHit(conversation: c, matched: addr.caseInsensitiveCompare(c.label) == .orderedSame ? "" : addr)
        }
        if let p = c.counterparts.first(where: { has($0.addr) || has($0.name) }) {
            return ConversationHit(conversation: c, matched: p.addr)
        }
        if has(c.receivedBy) { return ConversationHit(conversation: c, matched: c.receivedBy) }
        return ConversationHit(conversation: c, matched: c.lastSubject)
    }
}

/// Строка поиска похожа на адрес — первая строка выпадашки «Написать по
/// адресу…» (`parse_email_like` десктопа): без пробелов, кавычек, угловых
/// скобок и запятых; непустая часть до `@`; в домене точка не первой и не
/// последней. Возвращает адрес в нижнем регистре.
public func parseEmailLike(_ q: String) -> String? {
    let s = q.trimmingCharacters(in: .whitespacesAndNewlines)
    if s.contains(where: { $0.isWhitespace || "<>\",".contains($0) }) { return nil }
    guard let at = s.firstIndex(of: "@") else { return nil }
    let local = s[..<at]
    let domain = s[s.index(after: at)...]
    guard !local.isEmpty, !domain.isEmpty, let dot = domain.firstIndex(of: ".") else { return nil }
    if dot == domain.startIndex || domain.index(after: dot) == domain.endIndex { return nil }
    return s.lowercased()
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
    byName.forEach { taken.insert($0.ref) }
    let byAddr = convs.filter { c in
        !taken.contains(c.ref)
            && (c.counterparts.contains { has($0.addr) || has($0.name) } || has(c.receivedBy))
    }
    byAddr.forEach { taken.insert($0.ref) }
    let bySubject = convs.filter { !taken.contains($0.ref) && has($0.lastSubject) }
    return byName + byAddr + bySubject
}

private func fold(_ s: String) -> String {
    s.folding(options: [.caseInsensitive, .diacriticInsensitive, .widthInsensitive], locale: nil)
}
