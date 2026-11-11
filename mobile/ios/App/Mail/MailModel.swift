import DDMailAPI
import DDMailStore
import DDMailSync
import Foundation
import Observation
import os

let mailLog = Logger(subsystem: "ru.letotam.ddmail", category: "mail")

/// Почта на экране (порт `MailViewModel` Android): список, табы папок,
/// поиск, выбор, действия. Данные — из кэша (`SyncEngine`), сеть — синком.
@MainActor @Observable
final class MailModel {
    let engine: SyncEngine
    private let connection: Connection
    /// Открытая беседа — одна лента на всё время работы почты (§8).
    let conversation: ConversationModel
    /// Ответ, новое письмо, пересылка (§5).
    @ObservationIgnored private(set) var compose: ComposeModel!

    private(set) var all: [Conversation] = []
    private(set) var doc = FoldersDoc().withSpam()
    private(set) var colors = IdentityColors()
    private(set) var list = MailListState()
    private(set) var loaded = false
    private(set) var syncing = false

    /// Выбранная папка; `nil` — «Входящие». Помнится между запусками (§5).
    var activeFolderID: String? {
        didSet {
            guard oldValue != activeFolderID else { return }
            // Другой таб — другой набор строк: выбор снимается.
            picked.removeAll()
            UserDefaults.standard.set(activeFolderID, forKey: Self.activeKey)
            rebuild()
        }
    }

    var query = "" { didSet { rebuild() } }

    /// Открытая беседа (по id: дельта не сбивает выбор).
    var selectedID: String? {
        didSet {
            guard oldValue != selectedID else { return }
            if let id = selectedID { UserDefaults.standard.set(id, forKey: Self.lastKey) }
            openSelected()
        }
    }

    /// Множественный выбор (§5): id бесед.
    var picked: Set<String> = []

    /// Итог действия — короткая плашка внизу.
    var notice: String?

    private var tasks: [Task<Void, Never>] = []
    private static let activeKey = "mail.activeFolder"
    private static let lastKey = "mail.lastConversation"

    init(connection: Connection, engine: SyncEngine) {
        self.connection = connection
        self.engine = engine
        conversation = ConversationModel(engine: engine) { [weak connection] in connection?.report(.dead) }
        compose = ComposeModel(
            engine: engine, conversation: conversation,
            syncSoon: { [weak self] in Task { await self?.sync() } },
            onAuthDead: { [weak connection] in connection?.report(.dead) }
        )
        activeFolderID = UserDefaults.standard.string(forKey: Self.activeKey)
        start()
    }

    private func start() {
        tasks.append(Task { [weak self, engine] in
            for await convs in engine.conversations() {
                mailLog.debug("conversations: \(convs.count)")
                guard let self else { return }
                all = convs
                loaded = true
                rebuild()
                // Новые письма открытой беседы — дописать снизу.
                if let c = selected { conversation.updated(c) }
                // Отправили новое письмо — открыть его беседу, как только появится.
                if let target = compose.sentTarget, let c = convs.first(where: { matchesTarget($0, target) }) {
                    compose.consumeTarget()
                    open(c.id)
                }
            }
        })
        tasks.append(Task { [weak self, engine] in
            mailLog.debug("folders: subscribe")
            for await d in engine.folders() {
                mailLog.debug("folders: \(d.folders.count)")
                guard let self else { return }
                doc = d
                rebuild()
            }
        })
        tasks.append(Task { [weak self, engine] in
            for await c in engine.identityColors() {
                mailLog.debug("colors updated")
                self?.colors = c
            }
        })
    }

    func stop() {
        tasks.forEach { $0.cancel() }
        tasks.removeAll()
    }

    private func rebuild() {
        list = buildMailList(all, doc: doc, activeID: activeFolderID, query: query)
    }

    var selected: Conversation? { selectedID.flatMap { id in all.first { $0.id == id } } }

    /// Беседа, на которой закончили, — для раскладок, где беседа видна сразу.
    var lastConversationID: String? { UserDefaults.standard.string(forKey: Self.lastKey) }

    // MARK: Синхронизация

    /// Один проход; итог — в состояние сессии (§2).
    func sync() async {
        guard !syncing else { return }
        syncing = true
        defer { syncing = false }
        do {
            try await engine.sync()
            mailLog.debug("sync ok")
            connection.report(.ok)
        } catch is CancellationError {
        } catch APIError.unauthorized {
            connection.report(.dead)
        } catch {
            mailLog.error("sync failed: \(String(describing: error), privacy: .public)")
            connection.report(.offline(error.localizedDescription))
        }
    }

    // MARK: Открытие

    /// Беседа в другой папке — переключаемся на её папку (как
    /// `folders_ui::reveal` десктопа): иначе открытие из уведомления молча
    /// ничего бы не показало.
    func open(_ id: String?) {
        if let id, let c = all.first(where: { $0.id == id }) {
            let fid = doc.folderOf(c).map { doc.folders[$0].id }
            if fid != activeFolderID { activeFolderID = fid }
        }
        selectedID = id
    }

    private func openSelected() {
        compose.reset()
        conversation.open(selected)
        guard let c = selected, c.unreadCount > 0 else { return }
        Task { [engine] in
            do { try await engine.markRead(c) } catch { self.notice = "Не удалось отметить прочитанным: \(error.localizedDescription)" }
        }
    }

    /// Папка «Спам» скрыта всегда; попасть в неё — из меню.
    func openSpam() {
        if let i = doc.spamIndex { activeFolderID = doc.folders[i].id }
    }

    // MARK: Действия

    private func targets(_ ids: Set<String>) -> [Conversation] { all.filter { ids.contains($0.id) } }

    /// Свайп и меню: прочитано ⇄ непрочитано.
    func toggleRead(_ c: Conversation) {
        Task { [engine] in
            do {
                if c.unreadCount > 0 { try await engine.markRead(c) } else { try await engine.markUnread(c) }
            } catch { notice = "Не удалось: \(error.localizedDescription)" }
        }
    }

    func markRead(_ ids: Set<String>, read: Bool) {
        let convs = targets(ids)
        picked.removeAll()
        Task { [engine] in
            do {
                for c in convs { if read { try await engine.markRead(c) } else { try await engine.markUnread(c) } }
                notice = read ? "Прочитано: \(convs.count)" : "Непрочитано: \(convs.count)"
            } catch { notice = "Не удалось: \(error.localizedDescription)" }
        }
    }

    /// Папки для «В папку…» — все, включая скрытые (как `folder-targets`
    /// десктопа), кроме системных: в «Спам» руками не кладут.
    var folderTargets: [DialogFolder] { doc.folders.filter { !$0.isSystem } }

    /// В папку (`nil` — во «Входящие») — одной правкой документа на весь выбор.
    func move(_ ids: Set<String>, to folderID: String?) {
        let convs = targets(ids)
        picked.removeAll()
        let name = folderID.flatMap { id in doc.folders.first { $0.id == id }?.name } ?? "Входящие"
        Task { [engine] in
            do {
                try await engine.assignFolder(convs, folderID: folderID)
                notice = "Перенесено в «\(name)»: \(convs.count)"
            } catch { notice = "Не удалось перенести: \(error.localizedDescription)" }
        }
    }

    /// Сколько бесед и писем — для подтверждений удаления и «В спам».
    func summary(_ ids: Set<String>) -> (conversations: Int, messages: Int) {
        let c = targets(ids)
        return (c.count, c.reduce(0) { $0 + $1.messages.count })
    }

    func delete(_ ids: Set<String>) {
        let convs = targets(ids)
        picked.removeAll()
        if let s = selectedID, ids.contains(s) { selectedID = nil }
        Task { [engine] in
            do {
                try await engine.deleteConversations(convs)
                notice = "Удалено бесед: \(convs.count)"
            } catch { notice = "Не удалось удалить: \(error.localizedDescription)" }
            await sync()
        }
    }

    /// «В спам» (Android §5): письма удаляются, отправители блокируются —
    /// по адресу или по домену.
    func reportSpam(_ ids: Set<String>, scope: String) {
        let convs = targets(ids).filter { !$0.spam }
        picked.removeAll()
        guard !convs.isEmpty else { return }
        if let s = selectedID, ids.contains(s) { selectedID = nil }
        Task { [engine] in
            do {
                let o = try await engine.reportSpam(convs, scope: scope)
                let what = (o.ruleType == "domain" ? "домен " : "") + o.ruleValue
                let more = o.ruleCount > 1 ? " и ещё \(o.ruleCount - 1)" : ""
                notice = "✓ В спам: \(what)\(more), удалено писем: \(o.deleted)"
            } catch { notice = "Не удалось отправить в спам: \(error.localizedDescription)" }
            await sync()
        }
    }

    /// «Не спам»; `trust` — ещё и доверять отправителю.
    func notSpam(_ ids: Set<String>, trust: Bool) {
        let convs = targets(ids).filter(\.spam)
        picked.removeAll()
        guard !convs.isEmpty else { return }
        if let s = selectedID, ids.contains(s) { selectedID = nil }
        Task { [engine] in
            do {
                try await engine.restoreSpam(convs, trust: trust)
                notice = trust
                    ? "Во «Входящие», отправителям доверяем: бесед \(convs.count)"
                    : "Во «Входящие»: бесед \(convs.count)"
            } catch { notice = "Не удалось вернуть из спама: \(error.localizedDescription)" }
            await sync()
        }
    }
}
