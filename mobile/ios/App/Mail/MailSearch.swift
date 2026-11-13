import DDMailAPI
import DDMailSync
import Foundation
import Observation

/// Поиск почты — выпадашка под полем в первой строке списка (десктоп §4
/// «Поиск», `docs/mobile-layout.md`). Секции по порядку: «Написать по
/// адресу…», «Диалоги», «Контакты», «Сообщения». Диалоги и контакты ищутся
/// на устройстве на каждое нажатие, сообщения — сервером ddmail или IMAP
/// учётки с паузой.
/// Это же единственный путь к новому письму.
@MainActor @Observable
final class MailSearch {
    /// Контакт выпадашки: одна строка на адрес.
    struct ContactHit: Equatable, Identifiable {
        let name: String
        let email: String
        var id: String { email }
    }

    /// Письмо серверного поиска и его учётка.
    struct MessageHit: Equatable, Identifiable {
        let accountKey: String
        let hit: MessageSearchHit
        var id: String { "\(accountKey)/\(hit.id)" }
    }

    @ObservationIgnored private weak var mail: MailModel?

    var query = "" {
        didSet {
            guard oldValue != query else { return }
            refresh()
            searchMessages()
        }
    }

    private(set) var composeEmail: String?
    private(set) var conversations: [ConversationHit] = []
    private(set) var contacts: [ContactHit] = []
    private(set) var messages: [MessageHit] = []
    private(set) var loadingMessages = false
    /// Просьба поставить фокус в поле (⌘N): экран следит за сменой числа.
    private(set) var focusRequest = 0

    @ObservationIgnored private var messageTask: Task<Void, Never>?
    @ObservationIgnored private var contactsRequested = false

    private static let convLimit = 30
    private static let contactLimit = 12
    private static let messageLimit = 12

    init(mail: MailModel) {
        self.mail = mail
    }

    var trimmed: String { query.trimmingCharacters(in: .whitespacesAndNewlines) }
    /// Выпадашка открыта, пока в поле что-то есть (как `search-open` десктопа).
    var isOpen: Bool { !trimmed.isEmpty }
    var isEmpty: Bool { composeEmail == nil && conversations.isEmpty && contacts.isEmpty && messages.isEmpty }

    func requestFocus() { focusRequest += 1 }

    /// Закрыть выпадашку и стереть строку (выбрали результат).
    func clear() { query = "" }

    /// Локальная часть: адрес, диалоги, контакты. Зовётся и при смене списка
    /// бесед (дельта синка), чтобы выпадашка не показывала ушедшее.
    func refresh() {
        let q = trimmed
        guard !q.isEmpty, let mail else {
            if composeEmail != nil { composeEmail = nil }
            if !conversations.isEmpty { conversations = [] }
            if !contacts.isEmpty { contacts = [] }
            return
        }
        composeEmail = parseEmailLike(q)
        // По всем диалогам, а не по выбранной папке; спама в поиске нет.
        let hits = Array(searchConversationHits(mail.all.filter { !$0.spam }, q).prefix(Self.convLimit))
        if hits != conversations { conversations = hits }
        let found = Self.contacts(in: mail.workspace.contacts?.contacts ?? [], q,
                                  skip: Set(hits.flatMap { $0.conversation.counterparts.map { $0.addr.lowercased() } }))
        if found != contacts { contacts = found }
        loadContactsOnce()
    }

    /// Адресная книга нужна поиску и до первого захода в «Контакты».
    private func loadContactsOnce() {
        guard !contactsRequested, let book = mail?.workspace.contacts, !book.loaded else { return }
        contactsRequested = true
        Task { [weak self] in
            await book.load()
            self?.refresh()
        }
    }

    /// Имя или адрес содержит строку; адреса собеседников найденных диалогов
    /// не повторяются (`conv_hit_addrs` десктопа).
    static func contacts(in book: [ContactDTO], _ q: String, skip: Set<String>) -> [ContactHit] {
        var out: [ContactHit] = []
        var seen = skip
        for c in book {
            let byName = c.fullName.localizedStandardContains(q)
            for e in c.emails where !e.isEmpty && (byName || e.localizedStandardContains(q)) {
                guard seen.insert(e.lowercased()).inserted else { continue }
                out.append(ContactHit(name: c.fullName, email: e))
                if out.count == contactLimit { return out }
            }
        }
        return out
    }

    /// Поиск писем одной учётки: запрос и предел — совпадения.
    typealias MessageSearcher = @Sendable (String, Int) async throws -> [MessageSearchHit]

    /// «Сообщения» — после паузы в наборе: у ddmail-учёток — поиск сервера
    /// (`GET /search`), у IMAP-учёток — `UID SEARCH` во «Входящих» и
    /// «Отправленных» (`ImapEngine.searchMessages`, Android §5, десктоп
    /// `search_impl`). Учётка, у которой поиск не удался, просто ничего не даёт.
    private func searchMessages() {
        messageTask?.cancel()
        let q = trimmed
        let sources: [(String, MessageSearcher)] = mail?.workspace.links.compactMap { l in
            guard l.status != .dead else { return nil }
            if let api = l.api { return (l.account.key, { try await api.searchMessages($0, limit: $1) }) }
            if let imap = l.imap { return (l.account.key, { try await imap.searchMessages($0, limit: $1) }) }
            return nil
        } ?? []
        guard !q.isEmpty, !sources.isEmpty else {
            messages = []
            loadingMessages = false
            return
        }
        loadingMessages = true
        let limit = Self.messageLimit
        messageTask = Task { [weak self] in
            try? await Task.sleep(for: .milliseconds(300))
            guard !Task.isCancelled else { return }
            var found: [MessageHit] = []
            await withTaskGroup(of: [MessageHit].self) { group in
                for (key, search) in sources {
                    group.addTask {
                        do {
                            return try await search(q, limit).map { MessageHit(accountKey: key, hit: $0) }
                        } catch {
                            mailLog.error("search messages: \(String(describing: error), privacy: .public)")
                            return []
                        }
                    }
                }
                for await part in group { found += part }
            }
            guard !Task.isCancelled, let self, self.trimmed == q else { return }
            messages = Array(found.sorted { $0.hit.date > $1.hit.date }.prefix(limit))
            loadingMessages = false
        }
    }

    // MARK: Выбор строки

    /// Диалог выпадашки — открыть (переходом в его папку).
    func open(_ hit: ConversationHit) {
        guard let mail else { return }
        clear()
        mail.open(hit.conversation.ref)
    }

    func write(_ contact: ContactHit) { mail?.write(to: contact.email) }

    func compose() {
        guard let e = composeEmail else { return }
        mail?.startNew(to: e)
    }

    /// Письмо — открыть беседу, в которой оно лежит (по id письма или Message-ID).
    func open(_ m: MessageHit) {
        guard let mail else { return }
        let mid = m.hit.messageID
        let owner = mail.all.first { c in
            c.accountKey == m.accountKey
                && c.messages.contains { $0.uid == m.hit.id || (!mid.isEmpty && $0.messageID == mid) }
        }
        clear()
        if let owner {
            mail.open(owner.ref)
        } else {
            mail.notice = "Этого письма нет среди бесед на устройстве"
        }
    }

    /// Return в поле: «Написать по адресу…», если он есть, иначе первая строка.
    func submit() {
        if composeEmail != nil { compose() } else if let first = conversations.first { open(first) } else if let c = contacts.first { write(c) } else if let m = messages.first { open(m) }
    }
}
