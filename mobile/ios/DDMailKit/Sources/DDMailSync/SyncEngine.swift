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
public actor SyncEngine {
    /// Версия формата id бесед (сервер). Сменилась — следующий синк полный,
    /// иначе старые ключи висели бы рядом с новыми (§4 десктопа).
    public static let convIDEpoch = "2"
    public static let fullEvery: Int64 = 24 * 3600
    public static let seenFlag = "\\Seen"

    public nonisolated let accountKey: String
    nonisolated let spamKey: String
    let api: SessionAPI
    nonisolated let store: MailStore
    private let now: @Sendable () -> Int64
    private var tail: Task<Void, Never>?

    public init(accountKey: String, api: SessionAPI, store: MailStore, now: @escaping @Sendable () -> Int64 = { Int64(Date().timeIntervalSince1970) }) {
        self.accountKey = accountKey
        spamKey = "\(accountKey)#spam"
        self.api = api
        self.store = store
        self.now = now
    }

    // MARK: Наблюдение

    /// Все беседы учётки, со спамом (`spam = true`); порядок — по дате.
    public nonisolated func conversations() -> some AsyncSequence<[Conversation], Never> {
        let account = accountKey, spam = spamKey
        return store.observe([account, spam]).map { Self.domain($0, account: account, spam: spam) }
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
                    messages: (s.refs[r.id] ?? []).map(\.ref), spam: isSpam
                )
            }
        }
        let ham = map(snap[account], isSpam: false)
        let spamConvs = map(snap[spam], isSpam: true)
        if spamConvs.isEmpty { return ham }
        return (ham + spamConvs).sorted { $0.lastDateTs > $1.lastDateTs }
    }

    public nonisolated func folders() -> some AsyncSequence<FoldersDoc, Never> {
        store.observeMeta(Keys.folders).map { Self.parseFolders($0) }
    }

    public nonisolated func currentFolders() -> FoldersDoc {
        Self.parseFolders((try? store.meta(Keys.folders)) ?? nil)
    }

    static func parseFolders(_ s: String?) -> FoldersDoc {
        let raw = s.flatMap { JSONValue.parse(Data($0.utf8)) }
        return FoldersDoc.parse(raw).withSpam()
    }

    /// Цвета строк по группам адресов: из кэша, раз в синк — с сервера.
    public nonisolated func identityColors() -> some AsyncSequence<IdentityColors, Never> {
        store.observeMeta(Keys.identityGroups).map { s in
            IdentityColors.parse(s.flatMap { JSONValue.parse(Data($0.utf8)) })
        }
    }

    /// Свои адреса: из кэша, раз в синк — с сервера.
    public nonisolated func identities() -> [Identity] {
        guard let s = try? store.meta(Keys.identities) else { return [] }
        return (try? JSONDecoder().decode([Identity].self, from: Data(s.utf8))) ?? []
    }

    // MARK: Проход

    /// Один проход. Идущий проход сначала закончится; ошибки API — наверх.
    public func sync() async throws {
        let previous = tail
        let run = Task { [weak self] in
            await previous?.value
            try await self?.syncOnce()
        }
        tail = Task { _ = try? await run.value }
        try await run.value
    }

    private func syncOnce() async throws {
        var forceFull = false

        let seq = Int64(try store.meta(Keys.seq) ?? "") ?? 0
        let ch = try await api.changes(since: seq)
        if ch.reset && seq > 0 { forceFull = true }
        let deletes = ch.entries.filter { $0.kind == ChangeEntry.delete }.map(\.messageID)
        if !deletes.isEmpty { try store.applyDeletions(accountKey, messageIDs: deletes) }

        let lastFull = Int64(try store.meta(Keys.fullTs) ?? "") ?? 0
        let epochStale = try store.meta(Keys.epoch) != Self.convIDEpoch
        let since: Int64 = forceFull || epochStale || now() - lastFull > Self.fullEvery
            ? 0
            : Int64(try store.meta(Keys.since) ?? "") ?? 0
        let delta = try await api.conversationsDelta(sinceMs: since)
        if since > 0 {
            try store.upsert(accountKey, delta.conversations)
        } else {
            try store.replaceAll(accountKey, delta.conversations)
            try store.setMeta(Keys.fullTs, String(now()))
            try store.setMeta(Keys.epoch, Self.convIDEpoch)
        }
        if delta.serverNowMs > 0 { try store.setMeta(Keys.since, String(delta.serverNowMs)) }
        // Курсор журнала — только после того, как дельта легла: упади она,
        // надгробия этого прохода не потеряются, их отдадут ещё раз.
        if ch.latestSeq > 0 { try store.setMeta(Keys.seq, String(ch.latestSeq)) }

        // Спам не повод валить проход: папка «Спам» покажет прошлый список.
        if let list = try? await api.spamConversations() {
            try store.replaceAll(spamKey, list)
        }
        if let ids = try? await api.identities(), let data = try? JSONEncoder().encode(ids) {
            try store.setMeta(Keys.identities, String(decoding: data, as: UTF8.self))
        }
        // Цвета строк — не повод валить проход: старые цвета остаются.
        if let g = try? await api.identityGroups(), let doc = g.doc {
            try store.setMeta(Keys.identityGroups, String(decoding: doc.data(), as: UTF8.self))
        }
        let f = try await api.dialogFolders()
        if let doc = f.doc {
            try store.setMeta(Keys.folders, String(decoding: doc.data(), as: UTF8.self))
            try store.setMeta(Keys.foldersVersion, String(f.version))
        }
    }

    // MARK: Тела

    /// Тела писем: из кэша, недостающие и пустые — с сервера (и в кэш).
    /// Порядок — как у `refs`; чего нет нигде — пропущено. Нет связи —
    /// показываем то, что есть; мёртвая сессия — наверх.
    public func bodies(_ refs: [MessageRef]) async throws -> [MessageBody] {
        guard !refs.isEmpty else { return [] }
        let cached = try store.bodies(accountKey, uids: refs.map(\.uid))
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
        return refs.compactMap { cached[$0.uid] ?? fetched[$0.uid] }
    }

    /// Докачка тел заранее (контракт §4): новые письма всех бесед — на диск до
    /// того, как беседу откроют. От свежей беседы к старой, порциями;
    /// `allowed` проверяется перед каждой порцией.
    @discardableResult
    public func prefetchBodies(batch: Int = 30, allowed: @Sendable () async -> Bool) async throws -> Int {
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
    public nonisolated func part(uid: Int64, cid: String) async throws -> Binary {
        try await api.part(uid: uid, cid: cid)
    }

    public nonisolated func attachment(uid: Int64, index: Int) async throws -> Binary {
        try await api.attachment(uid: uid, index: index)
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
        try store.setMeta(Keys.fullTs, "0")
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
        defer { try? store.setMeta(Keys.fullTs, "0") }
        return try await api.blacklistAndPurge(scope: scope, messageIDs: ids, fallbackAddress: fallback)
    }

    /// Положить беседы в папку руками (`nil` — во «Входящие») одной правкой
    /// документа. Чужая правка между чтением и записью (`409`) — правка
    /// повторяется поверх свежего документа.
    public func assignFolder(_ convs: [Conversation], folderID: String?) async throws {
        var version = Int64(try store.meta(Keys.foldersVersion) ?? "") ?? 0
        var doc = currentFolders()
        for _ in 0..<3 {
            let target = folderID.flatMap { id in doc.folders.firstIndex { $0.id == id } }
            if folderID != nil && target == nil { return }
            let edited = doc.assign(convs, to: target)
            // Сразу в кэш: список перестроится, не дожидаясь сервера.
            try store.setMeta(Keys.folders, String(decoding: edited.toJSON().data(), as: UTF8.self))
            do {
                let r = try await api.saveDialogFolders(baseVersion: version, doc: edited.toJSON())
                try store.setMeta(Keys.foldersVersion, String(r.version))
                return
            } catch let APIError.server(code, message) where code == 409 {
                guard let fresh = try? JSONDecoder().decode(VersionedDoc.self, from: Data(message.utf8)) else {
                    throw APIError.server(code: code, message: message)
                }
                version = fresh.version
                doc = FoldersDoc.parse(fresh.doc).withSpam()
                try store.setMeta(Keys.foldersVersion, String(version))
            }
        }
        // Три раза подряд документ менялся под нами — не делаем вид, что записали.
        throw APIError.server(code: 409, message: "Папки сейчас меняет другое устройство. Попробуйте ещё раз.")
    }

    enum Keys {
        static let seq = "journal_seq"
        static let since = "conv_since"
        static let fullTs = "conv_full_ts"
        static let epoch = "conv_id_epoch"
        static let folders = "dialog_folders"
        static let foldersVersion = "dialog_folders_version"
        static let identities = "identities"
        static let identityGroups = "identity_groups"
    }
}
