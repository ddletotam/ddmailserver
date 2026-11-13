import DDMailAPI
import DDMailStore
import DDMailSync
import Foundation
import Observation
import os

let mailLog = Logger(subsystem: "ru.letotam.ddmail", category: "mail")

/// Почта на экране (порт `MailViewModel` Android): список, табы папок,
/// поиск, выбор, действия. Учёток несколько — беседы всех учёток в одном
/// списке; беседу определяет `ref` (учётка и id), действие уходит своей
/// учётке. Данные — из кэша, сеть — синком.
@MainActor @Observable
final class MailModel {
    @ObservationIgnored unowned let workspace: Workspace
    /// Открытая беседа — одна лента на всё время работы почты (§8).
    let conversation: ConversationModel
    /// Ответ, новое письмо, пересылка (§5).
    @ObservationIgnored private(set) var compose: ComposeModel!
    /// Аватарки собеседников (§5) — с сервера ddmail, если он есть.
    let avatars: AvatarStore
    /// Живые каналы, пока на экране: WebSocket ddmail, IDLE у IMAP (§7).
    @ObservationIgnored private(set) var live: [LiveChannel] = []
    /// Папки диалогов: документ ddmail-учётки или локальный (внешние учётки §4).
    let folderSource: any DialogFolderSource

    private(set) var all: [Conversation] = []
    private var byAccount: [String: [Conversation]] = [:]
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

    /// «Показать скрытые» (меню полосы табов) — на сессию, как у десктопа:
    /// таб папки «Не показывать в списке» виден, бледнее.
    var showHidden = false {
        didSet { if oldValue != showHidden { rebuild() } }
    }

    /// «Настройки → Папки → Показывать папки»: выключатель устройства (у
    /// десктопа — `folders_enabled` локальных настроек), сами папки общие.
    /// Выключено — список как без папок.
    var foldersOn: Bool = UserDefaults.standard.object(forKey: MailModel.foldersOnKey) as? Bool ?? true {
        didSet {
            guard oldValue != foldersOn else { return }
            UserDefaults.standard.set(foldersOn, forKey: Self.foldersOnKey)
            rebuild()
        }
    }

    /// Папки хранит сервер ddmail (общие для всех устройств) или устройство.
    var foldersShared: Bool { folderSource is SyncEngine }

    /// Поиск — выпадашка над списком, сам список им не фильтруется (десктоп §4 «Поиск»).
    @ObservationIgnored private(set) var search: MailSearch!

    /// Открытая беседа (`Conversation.ref`: дельта не сбивает выбор) или
    /// новая беседа с адресом из поиска (`newPrefix` + адрес).
    var selectedID: String? {
        didSet {
            guard oldValue != selectedID else { return }
            if let id = selectedID, !id.hasPrefix(Self.newPrefix) { UserDefaults.standard.set(id, forKey: Self.lastKey) }
            openSelected()
        }
    }

    /// Новая беседа (десктоп `enter_compose_mode`): строка над списком и
    /// пустая лента с композером, пока письмо не ушло и беседа не пришла.
    static let newPrefix = "new:"

    /// Адрес новой беседы, если открыта она.
    var composingTo: String? {
        guard let id = selectedID, id.hasPrefix(Self.newPrefix) else { return nil }
        return String(id.dropFirst(Self.newPrefix.count))
    }

    /// Множественный выбор (§5): `ref` бесед.
    var picked: Set<String> = []

    /// Итог действия — короткая плашка внизу.
    var notice: String?
    /// Беседы, ждущие подтверждения удаления (из меню, контекстного меню, выбора):
    /// удаление — только после подтверждения, API восстановления нет (§5).
    var confirmDelete: Set<String>?

    private var tasks: [Task<Void, Never>] = []
    private static let activeKey = "mail.activeFolder"
    private static let lastKey = "mail.lastConversation"
    static let foldersOnKey = "settings.foldersOn"

    init(workspace: Workspace, cache: MailStore) {
        self.workspace = workspace
        folderSource = workspace.primary?.engine ?? LocalDialogFolders(store: cache)
        let resolve: @MainActor (String) -> (any MailProvider)? = { [weak workspace] key in workspace?.link(key)?.provider }
        let dead: @MainActor (String) -> Void = { [weak workspace] key in workspace?.link(key)?.report(.dead) }
        conversation = ConversationModel(provider: resolve, onAuthDead: dead)
        avatars = AvatarStore(api: workspace.primary?.api)
        search = MailSearch(mail: self)
        compose = ComposeModel(
            workspace: workspace, conversation: conversation,
            syncSoon: { [weak self] key in Task { await self?.sync(only: key) } },
            onAuthDead: dead
        )
        live = workspace.links.compactMap { link -> LiveChannel? in
            if let imap = link.imap {
                return ImapLive(
                    engine: imap,
                    sync: { [weak self] in await self?.sync(only: link.account.key) },
                    onDead: { [weak link] in link?.report(.dead) }
                )
            }
            guard let api = link.api else { return nil }
            return LiveUpdates(
                api: api,
                sync: { [weak self] in await self?.sync(only: link.account.key) },
                // `calendar_updated`: календарь перечитывается, почта — нет.
                onCalendar: { [weak workspace] in Task { await workspace?.calendar?.refresh() } },
                // `delivery_status`: статус — в кэш сразу, пузырь и строка
                // перерисуются от наблюдения кэша.
                onDelivery: { [weak link] d in
                    guard let engine = link?.engine else { return }
                    Task {
                        do { try await engine.applyDelivery(d) } catch {
                            mailLog.error("delivery: \(String(describing: error), privacy: .public)")
                        }
                    }
                }
            )
        }
        activeFolderID = UserDefaults.standard.string(forKey: Self.activeKey)
        start()
    }

    private func start() {
        for link in workspace.links {
            let key = link.account.key
            let provider = link.provider
            tasks.append(Task { [weak self] in
                for await convs in provider.conversations() {
                    guard let self else { return }
                    byAccount[key] = convs
                    merge()
                }
            })
        }
        if workspace.links.isEmpty { loaded = true }
        tasks.append(Task { [weak self, folderSource] in
            for await d in folderSource.folders() {
                guard let self else { return }
                doc = d
                rebuild()
            }
        })
        if let engine = workspace.primary?.engine {
            tasks.append(Task { [weak self] in
                for await c in engine.identityColors() { self?.colors = c }
            })
        }
    }

    /// Беседы всех учёток — одним списком по дате.
    private func merge() {
        all = byAccount.values.flatMap { $0 }.sorted { ($0.lastDateTs, $1.ref) > ($1.lastDateTs, $0.ref) }
        // Список готов, когда отозвались все учётки (кэш читается сразу).
        loaded = byAccount.count >= workspace.links.count
        rebuild()
        // Новые письма открытой беседы — дописать снизу.
        if let c = selected { conversation.updated(c) }
        // Отправили новое письмо — открыть его беседу, как только появится.
        openSentTarget()
    }

    /// Новое письмо ушло — открыть его беседу, как только синк принёс её
    /// строку (десктоп `compose_sent_target`): строка «Новая беседа»
    /// заменяется настоящей. Ждём, только пока открыта новая беседа (или
    /// ничего): ушёл в другую — переход не выдёргивает.
    private func openSentTarget() {
        guard let target = compose.sentTarget else { return }
        guard let c = all.first(where: target.matches) else { return }
        let stubs = compose.consumeTarget()
        guard selectedID == nil || composingTo != nil else { return }
        conversation.carry = stubs
        open(c.ref)
    }

    func stop() {
        live.forEach { $0.stop() }
        prefetchTask?.cancel()
        tasks.forEach { $0.cancel() }
        tasks.removeAll()
    }

    private func rebuild() {
        list = buildMailList(all, doc: visibleDoc, activeID: activeFolderID, query: nil, showHidden: showHidden)
        search?.refresh()
    }

    /// Документ папок для списка: при выключенном «Показывать папки» — только
    /// системные («Спам» остаётся табом).
    var visibleDoc: FoldersDoc { foldersOn ? doc : doc.onlySystem() }

    var selected: Conversation? { selectedID.flatMap { ref in all.first { $0.ref == ref } } }

    /// Беседа, на которой закончили, — для раскладок, где беседа видна сразу.
    var lastConversationID: String? { UserDefaults.standard.string(forKey: Self.lastKey) }

    func provider(_ c: Conversation) -> (any MailProvider)? { workspace.link(c.accountKey)?.provider }

    // MARK: Синхронизация

    /// Проход по всем учёткам разом; ошибка одной не мешает другим (§7).
    func sync() async {
        guard !syncing else { return }
        syncing = true
        defer { syncing = false }
        await syncAll(notify: true)
        startPrefetch()
    }

    /// Одна учётка (событие её канала, отправка с неё).
    func sync(only key: String) async {
        guard let link = workspace.link(key) else { return }
        await run(link)
        // Цель отправки могла узнать свой Message-ID уже после того, как
        // беседа пришла (кэш не менялся — списку перестраиваться не с чего).
        openSentTarget()
        await notify()
    }

    func syncAll(notify: Bool) async {
        await withTaskGroup(of: Void.self) { group in
            for link in workspace.links {
                group.addTask { await self.run(link) }
            }
        }
        if notify { await self.notify() }
    }

    private func run(_ link: AccountLink) async {
        do {
            try await link.provider.sync()
            link.report(.ok)
        } catch is CancellationError {
        } catch {
            mailLog.error("sync \(link.account.key, privacy: .private): \(String(describing: error), privacy: .public)")
            link.report(error: error)
        }
    }

    private func notify() async {
        await Notifier.check(conversations: all, folders: doc, openRef: selectedID)
    }

    private var prefetchTask: Task<Void, Never>?

    /// Докачка тел после синка — фоном, уступая всему остальному (§4).
    func startPrefetch() {
        guard prefetchTask == nil else { return }
        prefetchTask = Task(priority: .background) { [weak self, workspace] in
            await workspace.prefetch()
            self?.prefetchTask = nil
        }
    }

    // MARK: Открытие

    /// Беседа в другой папке — переключаемся на её папку (как
    /// `folders_ui::reveal` десктопа): иначе открытие из уведомления молча
    /// ничего бы не показало.
    func open(_ ref: String?) {
        if let ref, let c = all.first(where: { $0.ref == ref }) {
            let d = visibleDoc
            let fid = d.folderOf(c).map { d.folders[$0].id }
            if fid != activeFolderID { activeFolderID = fid }
        }
        selectedID = ref
    }

    private func openSelected() {
        compose.reset()
        conversation.open(selected)
        if let ref = selectedID, composingTo == nil { Notifier.clear(conversation: ref) }
        guard let c = selected, c.unreadCount > 0, let p = provider(c) else { return }
        Task {
            do { try await p.markRead(c) } catch { self.fail("Не удалось отметить прочитанным", error, c.accountKey) }
        }
    }

    /// Новая беседа с адресом — «Написать по адресу…» выпадашки поиска.
    func startNew(to address: String) {
        let addr = address.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        guard !addr.isEmpty else { return }
        search.clear()
        selectedID = Self.newPrefix + addr
    }

    /// «Написать» на адрес (контакт в выпадашке, карточка контакта): есть
    /// беседа с этим собеседником — открыть самую свежую, нет — новая (как
    /// `search-select-contact` десктопа).
    func write(to address: String) {
        let addr = address.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        if let c = all.first(where: { !$0.spam && $0.counterparts.first?.addr.lowercased() == addr }) {
            search.clear()
            open(c.ref)
        } else {
            startNew(to: addr)
        }
    }

    // MARK: Действия

    private func targets(_ refs: Set<String>) -> [Conversation] { all.filter { refs.contains($0.ref) } }

    /// Беседы по учёткам — каждой учётке своё.
    private func byProvider(_ convs: [Conversation]) -> [(any MailProvider, [Conversation])] {
        Dictionary(grouping: convs, by: \.accountKey).compactMap { key, list in
            workspace.link(key).map { ($0.provider, list) }
        }
    }

    private func fail(_ what: String, _ error: Error, _ key: String? = nil) {
        if isAuthFailure(error), let key { workspace.link(key)?.report(.dead) }
        notice = "\(what): \(error.localizedDescription)"
    }

    /// Свайп и меню: прочитано ⇄ непрочитано.
    func toggleRead(_ c: Conversation) {
        guard let p = provider(c) else { return }
        Task {
            do {
                if c.unreadCount > 0 { try await p.markRead(c) } else { try await p.markUnread(c) }
            } catch { fail("Не удалось", error, c.accountKey) }
        }
    }

    func markRead(_ refs: Set<String>, read: Bool) {
        let convs = targets(refs)
        picked.removeAll()
        Task {
            do {
                for c in convs {
                    guard let p = provider(c) else { continue }
                    if read { try await p.markRead(c) } else { try await p.markUnread(c) }
                }
                notice = read ? "Прочитано: \(convs.count)" : "Непрочитано: \(convs.count)"
            } catch { fail("Не удалось", error) }
        }
    }

    /// Папки для «В папку…» — все, включая скрытые (как `folder-targets`
    /// десктопа), кроме системных: в «Спам» руками не кладут.
    var folderTargets: [DialogFolder] { doc.folders.filter { !$0.isSystem } }

    /// В папку (`nil` — во «Входящие») — одной правкой документа на весь выбор.
    func move(_ refs: Set<String>, to folderID: String?) {
        let convs = targets(refs)
        picked.removeAll()
        let name = folderID.flatMap { id in doc.folders.first { $0.id == id }?.name } ?? "Входящие"
        Task { [folderSource] in
            do {
                try await folderSource.assignFolder(convs, folderID: folderID)
                notice = "Перенесено в «\(name)»: \(convs.count)"
            } catch { fail("Не удалось перенести", error, workspace.primary?.account.key) }
        }
    }

    // MARK: Папки (меню полосы табов, «Настройки → Папки»)

    /// Правка документа папок: локально сразу, на сервер — следом (§4).
    private func editFolders(_ what: String, _ edit: @escaping @Sendable (FoldersDoc) -> FoldersDoc?) async -> Bool {
        do {
            try await folderSource.editFolders(edit)
            return true
        } catch {
            fail(what, error, workspace.primary?.account.key)
            return false
        }
    }

    /// «Новая папка»: «Папка N» в конец, id — чтобы сразу открыть её настройку.
    func addFolder() async -> String? {
        let id = doc.newID()
        // id выбран заранее: после `409` правка повторится с тем же id.
        let ok = await editFolders("Не удалось создать папку") { d in d.index(of: id) == nil ? d.adding(id: id).doc : nil }
        return ok ? id : nil
    }

    /// «Удалить»: диалоги, положенные руками, возвращаются во «Входящие».
    func deleteFolder(_ id: String) {
        if activeFolderID == id { activeFolderID = nil }
        let name = doc.folders.first { $0.id == id }?.name ?? ""
        Task {
            if await editFolders("Не удалось удалить папку", { $0.index(of: id) == nil ? nil : $0.removing(id) }) {
                notice = "Папка «\(name)» удалена"
            }
        }
    }

    /// Порядок папок — он же приоритет правил.
    func moveFolders(from: IndexSet, to: Int) {
        let user = doc.userFolders.map(\.id)
        var moved = user
        moved.move(fromOffsets: from, toOffset: to)
        let order = moved
        Task {
            _ = await editFolders("Не удалось переставить папки") { d in
                var out = d
                for (n, id) in order.enumerated() { out = out.moving(id, to: n) }
                return out
            }
        }
    }

    /// Правка полей папки из её настройки (имя, значок, правила, скрытость).
    func updateFolder(_ id: String, _ edit: @escaping @Sendable (inout DialogFolder) -> Void) {
        Task {
            _ = await editFolders("Не удалось сохранить папку") { d in d.index(of: id) == nil ? nil : d.updating(id, edit) }
        }
    }

    /// «Все прочитаны»: всё непрочитанное в диалогах папки — локально сразу,
    /// на сервере следом (по учётке диалога).
    func markFolderRead(_ id: String) {
        let d = visibleDoc
        guard let i = d.index(of: id) else { return }
        let convs = all.filter { $0.unreadCount > 0 && d.folderOf($0) == i }
        Task {
            for c in convs {
                guard let p = provider(c) else { continue }
                do { try await p.markRead(c) } catch { fail("Не удалось отметить прочитанным", error, c.accountKey) }
            }
            if !convs.isEmpty { notice = "Прочитано бесед: \(convs.count)" }
        }
    }

    /// Сколько бесед и писем — для подтверждений удаления и «В спам».
    func summary(_ refs: Set<String>) -> (conversations: Int, messages: Int) {
        let c = targets(refs)
        return (c.count, c.reduce(0) { $0 + $1.messages.count })
    }

    func delete(_ refs: Set<String>) {
        let convs = targets(refs)
        picked.removeAll()
        if let s = selectedID, refs.contains(s) { selectedID = nil }
        Task {
            var failed = false
            for (p, list) in byProvider(convs) {
                do { try await p.deleteConversations(list) } catch {
                    failed = true
                    fail("Не удалось удалить", error, p.accountKey)
                }
            }
            if !failed { notice = "Удалено бесед: \(convs.count)" }
            await sync()
        }
    }

    /// Как выбранные беседы умеют «В спам»: правилом (ddmail), переносом в
    /// Junk (IMAP) или никак (нет папки Junk — пункта нет).
    func spamSupport(_ refs: Set<String>) -> SpamSupport {
        let kinds = Set(targets(refs).compactMap { provider($0)?.spamSupport() })
        if kinds.isEmpty || kinds.contains(.none) { return .none }
        return kinds.contains(.rules) && kinds.count == 1 ? .rules : .folder
    }

    /// «В спам» (Android §5): ddmail — письма удаляются, отправители
    /// блокируются по адресу или домену; IMAP — перенос в Junk.
    func reportSpam(_ refs: Set<String>, scope: String) {
        let convs = targets(refs).filter { !$0.spam }
        picked.removeAll()
        guard !convs.isEmpty else { return }
        if let s = selectedID, refs.contains(s) { selectedID = nil }
        Task {
            var notes: [String] = []
            for (p, list) in byProvider(convs) {
                do {
                    let o = try await p.reportSpam(list, scope: scope)
                    if o.ruleValue.isEmpty {
                        notes.append("✓ В спам: бесед \(list.count)")
                    } else {
                        let what = (o.ruleType == "domain" ? "домен " : "") + o.ruleValue
                        let more = o.ruleCount > 1 ? " и ещё \(o.ruleCount - 1)" : ""
                        notes.append("✓ В спам: \(what)\(more), удалено писем: \(o.deleted)")
                    }
                } catch { fail("Не удалось отправить в спам", error, p.accountKey) }
            }
            if !notes.isEmpty { notice = notes.joined(separator: "; ") }
            await sync()
        }
    }

    /// «Не спам»; `trust` — ещё и доверять отправителю (только ddmail).
    func notSpam(_ refs: Set<String>, trust: Bool) {
        let convs = targets(refs).filter(\.spam)
        picked.removeAll()
        guard !convs.isEmpty else { return }
        if let s = selectedID, refs.contains(s) { selectedID = nil }
        Task {
            var ok = true
            for (p, list) in byProvider(convs) {
                do { try await p.restoreSpam(list, trust: trust) } catch {
                    ok = false
                    fail("Не удалось вернуть из спама", error, p.accountKey)
                }
            }
            if ok {
                notice = trust
                    ? "Во «Входящие», отправителям доверяем: бесед \(convs.count)"
                    : "Во «Входящие»: бесед \(convs.count)"
            }
            await sync()
        }
    }

    /// «Доверять отправителю» есть только у ddmail (правило сервера).
    func canTrust(_ refs: Set<String>) -> Bool {
        targets(refs).allSatisfy { provider($0)?.spamSupport() == .rules }
    }
}

/// Живой канал учётки, пока приложение на экране.
@MainActor
protocol LiveChannel: AnyObject {
    func start()
    func stop()
}

extension LiveUpdates: LiveChannel {}
