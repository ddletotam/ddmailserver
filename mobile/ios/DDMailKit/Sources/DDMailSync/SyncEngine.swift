import DDMailAPI
import DDMailStore
import Foundation

/// Синхронизация учётки ddmail (контракт §4) — порт `SyncEngine` Android,
/// модель десктопного native-провайдера:
///
/// 1. журнал `/changes?since=<seq>` — надгробия удалений применяются к кэшу
///    ДО дельты: дельта бесед сообщает только изменившиеся, удалённые — никогда;
///    `reset` (курсор негоден) — следующий шаг полный;
/// 2. `/conversations?since=<watermark>` — дельта; полная выдача (`since=0`)
///    заменяет набор целиком: на первом запуске, по `reset`, раз в сутки и при
///    смене `convIDEpoch`;
/// 3. спам `/conversations?spam=1` — всегда полный список, в своё пространство
///    кэша: журнал считает спам удалением, и в общем пространстве надгробия
///    стирали бы эти беседы;
/// 4. документы папок и групп адресов — в кэш (табы и цвета видны на старте).
///
/// Проходы одной учётки не идут параллельно: следующий ждёт предыдущего.
public actor SyncEngine: MailProvider, DialogFolderSource {
    /// Версия формата id бесед (сервер). Сменилась — следующий синк полный,
    /// иначе старые ключи висели бы рядом с новыми (§4 десктопа).
    public static let convIDEpoch = "2"
    public static let fullEvery: Int64 = 24 * 3600
    public static let seenFlag = "\\Seen"

    public nonisolated let accountKey: String
    nonisolated let spamKey: String
    let api: SessionAPI
    /// Докачка «Только по Wi-Fi»: тот же токен, сессия без дорогих сетей.
    let prefetchAPI: SessionAPI?
    nonisolated let store: MailStore
    private let now: @Sendable () -> Int64
    private var tail: Task<Void, Never>?
    nonisolated let keys: Keys

    public init(accountKey: String, api: SessionAPI, store: MailStore, prefetchAPI: SessionAPI? = nil,
                now: @escaping @Sendable () -> Int64 = { Int64(Date().timeIntervalSince1970) }) {
        self.accountKey = accountKey
        spamKey = "\(accountKey)#spam"
        self.api = api
        self.prefetchAPI = prefetchAPI
        self.store = store
        self.now = now
        keys = Keys(accountKey)
    }

    // MARK: Наблюдение

    /// Все беседы учётки, со спамом (`spam = true`); порядок — по дате.
    public nonisolated func conversations() -> AsyncStream<[Conversation]> {
        let account = accountKey, spam = spamKey
        let source = store.observe([account, spam])
        return AsyncStream { cont in
            let t = Task {
                for await snap in source { cont.yield(Self.domain(snap, account: account, spam: spam)) }
                cont.finish()
            }
            cont.onTermination = { _ in t.cancel() }
        }
    }

    /// Снимок без наблюдения (фон, уведомления).
    public nonisolated func currentConversations() throws -> [Conversation] {
        Self.domain(try store.snapshot([accountKey, spamKey]), account: accountKey, spam: spamKey)
    }

    static func domain(_ snap: [String: ConversationSnapshot], account: String, spam: String) -> [Conversation] {
        func map(_ s: ConversationSnapshot?, isSpam: Bool) -> [Conversation] {
            guard let s else { return [] }
            return s.conversations.map { r in
                Conversation(
                    accountKey: account, id: r.id, label: r.label, receivedBy: r.receivedBy,
                    counterparts: r.counterparts, isGroup: r.isGroup, lastDateTs: r.lastDateTs,
                    lastSubject: r.lastSubject, unreadCount: r.unreadCount, totalCount: r.totalCount,
                    messages: (s.refs[r.id] ?? []).map(\.ref), spam: isSpam,
                    lastDelivery: DeliveryState(r.lastDeliveryStatus)
                )
            }
        }
        let ham = map(snap[account], isSpam: false)
        let spamConvs = map(snap[spam], isSpam: true)
        if spamConvs.isEmpty { return ham }
        return (ham + spamConvs).sorted { $0.lastDateTs > $1.lastDateTs }
    }

    public nonisolated func folders() -> AsyncStream<FoldersDoc> {
        let source = store.observeMeta(keys.folders)
        return AsyncStream { cont in
            let t = Task {
                for await v in source { cont.yield(Self.parseFolders(v)) }
                cont.finish()
            }
            cont.onTermination = { _ in t.cancel() }
        }
    }

    public nonisolated func currentFolders() -> FoldersDoc {
        Self.parseFolders((try? store.meta(keys.folders)) ?? nil)
    }

    static func parseFolders(_ s: String?) -> FoldersDoc {
        let raw = s.flatMap { JSONValue.parse(Data($0.utf8)) }
        return FoldersDoc.parse(raw).withSpam()
    }

    /// Цвета строк по группам адресов: из кэша, раз в синк — с сервера.
    public nonisolated func identityColors() -> AsyncStream<IdentityColors> {
        let source = store.observeMeta(keys.identityGroups)
        return AsyncStream { cont in
            let t = Task {
                for await s in source { cont.yield(IdentityColors.parse(s.flatMap { JSONValue.parse(Data($0.utf8)) })) }
                cont.finish()
            }
            cont.onTermination = { _ in t.cancel() }
        }
    }

    /// «В спам» у ddmail — правилом сервера (адрес или домен).
    public nonisolated func spamSupport() -> SpamSupport { .rules }

    /// Свои адреса: из кэша, раз в синк — с сервера.
    public nonisolated func identities() -> [Identity] {
        guard let s = try? store.meta(keys.identities) else { return [] }
        return (try? JSONDecoder().decode([Identity].self, from: Data(s.utf8))) ?? []
    }

    // MARK: Проход

    /// Один проход. Идущий проход сначала закончится; ошибки API — наверх.
    /// Отмена вызывающего отменяет и проход (расширение уведомлений и фоновая
    /// задача ограничены по времени).
    public func sync() async throws {
        let previous = tail
        let run = Task { [weak self] in
            await previous?.value
            try Task.checkCancellation()
            try await self?.syncOnce()
        }
        tail = Task { _ = try? await run.value }
        try await withTaskCancellationHandler {
            try await run.value
        } onCancel: {
            run.cancel()
        }
    }

    private func syncOnce() async throws {
        var forceFull = false

        let seq = Int64(try store.meta(keys.seq) ?? "") ?? 0
        let ch = try await api.changes(since: seq)
        if ch.reset && seq > 0 { forceFull = true }
        let deletes = ch.entries.filter { $0.kind == ChangeEntry.delete }.map(\.messageID)
        if !deletes.isEmpty { try store.applyDeletions(accountKey, messageIDs: deletes) }

        let lastFull = Int64(try store.meta(keys.fullTs) ?? "") ?? 0
        let epochStale = try store.meta(keys.epoch) != Self.convIDEpoch
        let since: Int64 = forceFull || epochStale || now() - lastFull > Self.fullEvery
            ? 0
            : Int64(try store.meta(keys.since) ?? "") ?? 0
        let delta = try await api.conversationsDelta(sinceMs: since)
        if since > 0 {
            try store.upsert(accountKey, delta.conversations)
        } else {
            try store.replaceAll(accountKey, delta.conversations)
            try store.setMeta(keys.fullTs, String(now()))
            try store.setMeta(keys.epoch, Self.convIDEpoch)
        }
        if delta.serverNowMs > 0 { try store.setMeta(keys.since, String(delta.serverNowMs)) }
        // Курсор журнала — только после того, как дельта легла: упади она,
        // надгробия этого прохода не потеряются, их отдадут ещё раз.
        if ch.latestSeq > 0 { try store.setMeta(keys.seq, String(ch.latestSeq)) }

        // Спам не повод валить проход: папка «Спам» покажет прошлый список.
        if let list = try? await api.spamConversations() {
            try store.replaceAll(spamKey, list)
        }
        if let ids = try? await api.identities(), let data = try? JSONEncoder().encode(ids) {
            try store.setMeta(keys.identities, String(decoding: data, as: UTF8.self))
        }
        // Цвета строк — не повод валить проход: старые цвета остаются.
        if let g = try? await api.identityGroups(), let doc = g.doc {
            try store.setMeta(keys.identityGroups, String(decoding: doc.data(), as: UTF8.self))
        }
        let f = try await api.dialogFolders()
        if let doc = f.doc {
            try store.setMeta(keys.folders, String(decoding: doc.data(), as: UTF8.self))
            try store.setMeta(keys.foldersVersion, String(f.version))
        }
    }

    // MARK: Тела

    /// Тела писем: из кэша, недостающие и пустые — с сервера (и в кэш).
    /// Порядок — как у `refs`; чего нет нигде — пропущено. Нет связи —
    /// показываем то, что есть; мёртвая сессия — наверх.
    public func bodies(_ refs: [MessageRef]) async throws -> [MessageBody] {
        guard !refs.isEmpty else { return [] }
        let inCache = try store.bodies(accountKey, uids: refs.map(\.uid))
        var cached = inCache
        // Статус доставки сменился (дельта принесла его в ссылку), а тело в
        // кэше — со старым: перезапросить.
        for r in refs where Self.deliveryStale(r, cached[r.uid]) { cached[r.uid] = nil }
        let missing = refs.filter { cached[$0.uid] == nil }
        var fetched: [Int64: MessageBody] = [:]
        if !missing.isEmpty {
            do {
                let got = try await api.messages(missing)
                try store.putBodies(accountKey, got)
                fetched = Dictionary(got.map { ($0.uid, $0) }, uniquingKeysWith: { a, _ in a })
            } catch let e as APIError {
                if case .unauthorized = e { throw e }
            }
        }
        // Нет связи — пусть лучше тело со старым статусом, чем пустое место.
        return refs.compactMap { cached[$0.uid] ?? fetched[$0.uid] ?? inCache[$0.uid] }
    }

    /// Тело закэшировано со статусом доставки, которого у письма уже нет.
    /// Пустой статус ссылки — «неизвестно» (кэш до миграции, старый
    /// сервер): тело не трогаем.
    static func deliveryStale(_ ref: MessageRef, _ body: MessageBody?) -> Bool {
        guard let body, !ref.deliveryStatus.isEmpty else { return false }
        return ref.deliveryStatus != body.deliveryStatus
    }

    /// Событие `delivery_status`: статус — в кэш сразу (пузырь и строка
    /// обновятся без перезапроса); остальное принесёт синк.
    @discardableResult
    public func applyDelivery(_ d: DeliveryUpdate) throws -> [String] {
        try store.applyDelivery(accountKey, d)
    }

    /// Докачка тел заранее (контракт §4): новые письма всех бесед — на диск до
    /// того, как беседу откроют. От свежей беседы к старой, порциями;
    /// `allowed` проверяется перед каждой порцией.
    @discardableResult
    public func prefetchBodies(constrained: Bool, allowed: @escaping @Sendable () async -> Bool) async throws -> Int {
        try await prefetchBodies(via: constrained ? prefetchAPI : nil, allowed: allowed)
    }

    @discardableResult
    public func prefetchBodies(via prefetchAPI: SessionAPI? = nil, batch: Int = 30, allowed: @Sendable () async -> Bool) async throws -> Int {
        let api = prefetchAPI ?? self.api
        let cached = try store.cachedBodyUIDs(accountKey)
        let snap = try store.snapshot([accountKey])[accountKey]
        let queue = (snap?.conversations ?? []).flatMap { c in
            (snap?.refs[c.id] ?? []).sorted { $0.uid > $1.uid }
        }.filter { !cached.contains($0.uid) }.map(\.ref)
        var n = 0
        var i = 0
        while i < queue.count {
            if Task.isCancelled { break }
            guard await allowed() else { break }
            let chunk = Array(queue[i..<min(i + batch, queue.count)])
            i += batch
            let got: [MessageBody]
            do {
                got = try await api.messages(chunk)
            } catch let e as APIError {
                if case .unauthorized = e { throw e }
                break
            }
            try store.putBodies(accountKey, got)
            n += got.count
        }
        return n
    }

    // MARK: Части и вложения

    /// Встроенная часть письма (`cid:`) — для документа ленты.
    public func part(uid: Int64, cid: String) async throws -> Binary {
        try await api.part(uid: uid, cid: cid)
    }

    public func attachment(uid: Int64, index: Int) async throws -> Binary {
        try await api.attachment(uid: uid, index: index)
    }

    /// Поставить письмо в отправку: сервер ставит в очередь и сразу кладёт
    /// копию в «Отправленные» со статусом `pending` (десктоп §1).
    @discardableResult
    public func send(_ request: SendRequest) async throws -> SendReceipt {
        try await api.send(request)
    }

    // MARK: Мутации

    private func cacheKey(_ c: Conversation) -> String { c.spam ? spamKey : accountKey }

    /// Открыли беседу: прочитана локально сразу, на сервере — следом.
    public func markRead(_ c: Conversation) async throws {
        let unseen = c.messages.filter { !$0.seen }
        try store.markRead(cacheKey(c), conversation: c.id)
        if !unseen.isEmpty { try await api.setFlag(unseen, flag: Self.seenFlag, add: true) }
    }

    /// «Непрочитано» — на последнем письме беседы.
    public func markUnread(_ c: Conversation) async throws {
        guard let last = c.messages.max(by: { $0.uid < $1.uid }) else { return }
        try store.markUnread(cacheKey(c), conversation: c.id, uid: last.uid)
        try await api.setFlag([last], flag: Self.seenFlag, add: false)
    }

    /// Удалить беседы целиком: из кэша — сразу, письма — на сервер.
    /// Надгробия журнала потом подтвердят то же самое.
    public func deleteConversations(_ convs: [Conversation]) async throws {
        try store.deleteConversations(accountKey, ids: convs.map(\.id))
        let refs = convs.flatMap(\.messages)
        for chunk in stride(from: 0, to: refs.count, by: 200).map({ Array(refs[$0..<min($0 + 200, refs.count)]) }) {
            try await api.delete(chunk)
        }
    }

    /// «Не спам»: письма бесед — во «Входящие» на сервере; из «Спам» в кэше
    /// уходят сразу, во «Входящих» появятся со следующим (полным) синком.
    @discardableResult
    public func restoreSpam(_ convs: [Conversation], trust: Bool = false) async throws -> Int {
        let spam = convs.filter(\.spam)
        guard !spam.isEmpty else { return 0 }
        let refs = spam.flatMap(\.messages)
        var n = 0
        for chunk in stride(from: 0, to: refs.count, by: 200).map({ Array(refs[$0..<min($0 + 200, refs.count)]) }) {
            n += try await api.restoreSpam(chunk, trust: trust)
        }
        try store.deleteConversations(spamKey, ids: spam.map(\.id))
        try store.setMeta(keys.fullTs, "0")
        return n
    }

    /// «В спам»: письма бесед удаляются на сервере, отправители (или домены,
    /// `scope` = `domain`) блокируются. Из кэша — сразу; следующий синк полный:
    /// при отказе он вернёт беседы, при успехе уберёт и зацепленные правилом.
    public func reportSpam(_ convs: [Conversation], scope: String) async throws -> PurgeOutcome {
        let ham = convs.filter { !$0.spam }
        let ids = ham.flatMap(\.messages).map(\.uid)
        // Запасной адрес — только когда беседа одна: у нескольких он был бы
        // адресом одной из них, и по ошибке сервера заблокировался бы не тот.
        let fallback = ham.count == 1 ? (ham[0].counterparts.first?.addr.lowercased() ?? "") : ""
        try store.deleteConversations(accountKey, ids: ham.map(\.id))
        defer { try? store.setMeta(keys.fullTs, "0") }
        return try await api.blacklistAndPurge(scope: scope, messageIDs: ids, fallbackAddress: fallback)
    }

    /// Правка документа папок («В папку», раздел «Папки» настроек, меню
    /// табов): сразу в кэш, затем `PUT` с `base_version`. Чужая правка между
    /// чтением и записью (`409`) — правка повторяется поверх свежего
    /// документа: она описана замыканием, а не готовым документом, так что
    /// чужое не затирается.
    public func editFolders(_ edit: @escaping @Sendable (FoldersDoc) -> FoldersDoc?) async throws {
        var version = Int64(try store.meta(keys.foldersVersion) ?? "") ?? 0
        var doc = currentFolders()
        for _ in 0..<3 {
            guard let edited = edit(doc)?.withSpam() else { return }
            // Сразу в кэш: список перестроится, не дожидаясь сервера.
            try store.setMeta(keys.folders, String(decoding: edited.toJSON().data(), as: UTF8.self))
            do {
                let r = try await api.saveDialogFolders(baseVersion: version, doc: edited.toJSON())
                try store.setMeta(keys.foldersVersion, String(r.version))
                return
            } catch let APIError.server(code, message) where code == 409 {
                guard let fresh = try? JSONDecoder().decode(VersionedDoc.self, from: Data(message.utf8)) else {
                    throw APIError.server(code: code, message: message)
                }
                version = fresh.version
                doc = FoldersDoc.parse(fresh.doc).withSpam()
                try store.setMeta(keys.foldersVersion, String(version))
            }
        }
        // Три раза подряд документ менялся под нами — не делаем вид, что записали.
        throw APIError.server(code: 409, message: "Папки сейчас меняет другое устройство. Попробуйте ещё раз.")
    }

    /// Служебные ключи кэша — с ключом учётки (учёток несколько, кэш один).
    struct Keys: Sendable {
        let seq, since, fullTs, epoch, folders, foldersVersion, identities, identityGroups: String

        init(_ account: String) {
            func k(_ s: String) -> String { "\(account)/\(s)" }
            seq = k("journal_seq"); since = k("conv_since"); fullTs = k("conv_full_ts"); epoch = k("conv_id_epoch")
            folders = k("dialog_folders"); foldersVersion = k("dialog_folders_version")
            identities = k("identities"); identityGroups = k("identity_groups")
        }
    }
}
