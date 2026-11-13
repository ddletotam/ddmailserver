import DDMailAPI
import DDMailIMAP
import DDMailMIME
import DDMailNet
import DDMailSMTP
import DDMailStore
import Foundation
import os

private let imapLog = Logger(subsystem: "ru.letotam.ddmail", category: "imap")

/// Настройки IMAP-учётки для движка (без секрета).
public struct ImapAccountConfig: Sendable {
    public var key: String
    public var email: String
    public var displayName: String
    public var login: String
    public var imap: MailEndpoint
    public var smtp: MailEndpoint
    public var smtpLogin: String
    public var smtpAuth: Bool
    /// Свои адреса учётки: email, логин-адрес, «другие мои адреса».
    public var ownAddresses: [String]

    public init(key: String, email: String, displayName: String, login: String, imap: MailEndpoint, smtp: MailEndpoint,
                smtpLogin: String, smtpAuth: Bool, ownAddresses: [String]) {
        self.key = key; self.email = email; self.displayName = displayName; self.login = login
        self.imap = imap; self.smtp = smtp; self.smtpLogin = smtpLogin; self.smtpAuth = smtpAuth
        self.ownAddresses = ownAddresses
    }

    /// Gmail сам кладёт копию отправленного в «Отправленные» (контракт §5).
    var isGmail: Bool { imap.host.hasSuffix("gmail.com") || smtp.host.hasSuffix("gmail.com") || imap.host.hasSuffix("googlemail.com") }
}

/// Почта IMAP-учётки (контракт внешних учёток §3–§5): инкрементальный синк по
/// состоянию папки (UIDVALIDITY, UIDNEXT, HIGHESTMODSEQ; CONDSTORE/QRESYNC с
/// запасными путями), беседы — на устройстве из кэша заголовков (порт
/// десктопа и сервера), тела — `BODY.PEEK[]` и разбор MIME в ту же модель,
/// что у ddmail, отправка — SMTP с копией в «Отправленные».
///
/// Два соединения: синк и действия (тела, флаги, перенос) — открытие беседы
/// не ждёт долгого первого синка. Ошибки — `APIError` (`unauthorized` — нужен вход).
public actor ImapEngine: MailProvider {
    public nonisolated let accountKey: String
    nonisolated let spamKey: String
    nonisolated let config: ImapAccountConfig
    nonisolated let store: MailStore
    private let password: @Sendable () -> String?
    private let connector: MailConnector
    private let constrainedConnector: MailConnector
    private let syncChannel: ImapChannel
    private let opsChannel: ImapChannel
    private var raw = RawCache()
    private var syncTail: Task<Void, Error>?
    /// Ключ кэша: айдентики, которые объявил сам сервер (ddmail по IMAP — METADATA).
    nonisolated let identitiesKey: String

    public init(
        config: ImapAccountConfig, store: MailStore, password: @escaping @Sendable () -> String?,
        connector: MailConnector = SystemMailConnector(), constrainedConnector: MailConnector = SystemMailConnector(constrained: true)
    ) {
        accountKey = config.key
        spamKey = "\(config.key)#spam"
        self.config = config
        self.store = store
        self.password = password
        self.connector = connector
        self.constrainedConnector = constrainedConnector
        identitiesKey = "\(config.key)/imap_identities"
        let open = Self.opener(config, password, connector)
        syncChannel = ImapChannel(open: open)
        opsChannel = ImapChannel(open: open)
    }

    static func opener(_ config: ImapAccountConfig, _ password: @escaping @Sendable () -> String?, _ connector: MailConnector) -> @Sendable () async throws -> IMAPClient {
        {
            guard let pw = password(), !pw.isEmpty else { throw MailNetError.auth("нет пароля") }
            let c = try await IMAPClient.connect(config.imap, connector: connector, timeout: .seconds(25))
            do {
                try await c.login(user: config.login, password: pw)
            } catch {
                await c.close()
                throw error
            }
            return c
        }
    }

    /// Ошибка наружу — на языке экрана.
    private func mapped<T>(_ body: () async throws -> T) async throws -> T {
        do { return try await body() } catch { throw apiError(error) }
    }

    // MARK: Наблюдение

    public nonisolated func conversations() -> AsyncStream<[Conversation]> {
        let account = accountKey, spam = spamKey
        let source = store.observe([account, spam])
        return AsyncStream { cont in
            let t = Task {
                for await snap in source { cont.yield(SyncEngine.domain(snap, account: account, spam: spam)) }
                cont.finish()
            }
            cont.onTermination = { _ in t.cancel() }
        }
    }

    public nonisolated func currentConversations() throws -> [Conversation] {
        SyncEngine.domain(try store.snapshot([accountKey, spamKey]), account: accountKey, spam: spamKey)
    }

    /// Свои адреса: адрес учётки (по умолчанию), «другие мои» и объявленные
    /// сервером. Сервер ddmail объявил адрес по умолчанию — он и есть
    /// «От кого» по умолчанию: адрес, введённый при подключении, сервер может
    /// и не разрешать для отправки.
    public nonisolated func identities() -> [Identity] {
        let server = serverIdentities()
        let serverDefault = server.first(where: \.isDefault)?.email.lowercased()
        var out = [Identity(email: config.email, name: config.displayName, isDefault: serverDefault == nil || serverDefault == config.email.lowercased())]
        var seen: Set<String> = [config.email.lowercased()]
        for a in config.ownAddresses where seen.insert(a.lowercased()).inserted {
            out.append(Identity(email: a, name: config.displayName))
        }
        for i in server where seen.insert(i.email.lowercased()).inserted {
            out.append(Identity(email: i.email.lowercased(), name: i.name.isEmpty ? config.displayName : i.name, signature: i.signature,
                                isDefault: i.email.lowercased() == serverDefault))
        }
        return out
    }

    nonisolated func serverIdentities() -> [Identity] {
        guard let s = try? store.meta(identitiesKey) else { return [] }
        return (try? JSONDecoder().decode([Identity].self, from: Data(s.utf8))) ?? []
    }

    nonisolated var ours: Set<String> {
        Set(identities().map { $0.email.lowercased() })
    }

    public nonisolated func spamSupport() -> SpamSupport {
        ((try? store.imapFolders(accountKey)[ImapRole.junk.rawValue]) ?? nil) != nil ? .folder : .none
    }

    // MARK: Синк

    /// Один проход; следующий ждёт предыдущего.
    public func sync() async throws {
        let previous = syncTail
        let run = Task {
            _ = try? await previous?.value
            try await self.syncOnce()
        }
        syncTail = run
        try await mapped { try await run.value }
    }

    private func syncOnce() async throws {
        let key = accountKey
        let store = self.store
        // Айдентики сервера — каждый проход: команда дешёвая, а набор меняется.
        let wantIdentities = true
        let result: (roles: [ImapRole: String], ids: [Identity]?) = try await syncChannel.run { c in
            let list = try await c.list()
            let roles = ImapFolders.resolve(list)
            var ids: [Identity]?
            if wantIdentities, await c.has("METADATA") { ids = try? await Self.fetchIdentities(c) }
            let known = try store.imapFolders(key)
            for role in ImapRole.allCases where roles[role] == nil && known[role.rawValue] != nil {
                try store.dropImapFolder(key, role: role.rawValue)
            }
            guard roles[.inbox] != nil else { throw MailNetError.server("На сервере нет папки «Входящие».") }
            for role in [ImapRole.inbox, .sent, .drafts, .junk] {
                guard let name = roles[role] else { continue }
                do {
                    try await Self.syncFolder(c, store: store, account: key, role: role, name: name)
                } catch let e as MailNetError where role != .inbox {
                    // Папка, кроме «Входящих», не открылась — остальным это не мешает.
                    if case .server = e { imapLog.notice("sync \(role.rawValue): \(e.localizedDescription, privacy: .public)"); continue }
                    throw e
                }
            }
            return (roles, ids)
        }
        if let ids = result.ids, let data = try? JSONEncoder().encode(ids) {
            try store.setMeta(identitiesKey, String(decoding: data, as: UTF8.self))
        }
        try rebuild()
    }

    /// Айдентики сервера ddmail по IMAP (`GETMETADATA`, как десктоп).
    static func fetchIdentities(_ c: IMAPClient) async throws -> [Identity] {
        let r = try await c.run("GETMETADATA \"\" /shared/vendor/ddmail/identities")
        for u in r.untagged {
            guard case let .other(text) = u, text.uppercased().hasPrefix("METADATA"), let json = extractTopLevelArray(text) else { continue }
            return (try? JSONDecoder().decode([Identity].self, from: Data(json.utf8))) ?? []
        }
        return []
    }

    /// Инкрементальный проход по папке (контракт §3):
    /// - сменилась UIDVALIDITY — кэш папки (заголовки и тела) прочь, папка заново;
    /// - новые — `UID FETCH <последний+1>:*`;
    /// - флаги — CONDSTORE (`CHANGEDSINCE`), без него — FLAGS всего окна;
    /// - удалённые — QRESYNC (`VANISHED`), иначе `UID SEARCH` и разница.
    static func syncFolder(_ c: IMAPClient, store: MailStore, account: String, role: ImapRole, name: String) async throws {
        var st = try store.imapFolders(account)[role.rawValue] ?? ImapFolderRecord(accountKey: account, role: role.rawValue, name: name)
        if st.name != name {
            // Роль досталась другой папке — прежние письма не из неё.
            try store.clearImapFolder(account, role: role.rawValue, bodyUIDs: ImapUID.range(role))
            st = ImapFolderRecord(accountKey: account, role: role.rawValue, name: name, epoch: st.epoch + 1)
        }
        let canQresync = await c.qresyncEnabled && st.uidValidity > 0 && st.highestModSeq > 0
        let sel = try await c.select(name, readOnly: true, qresync: canQresync ? (UInt32(st.uidValidity), st.highestModSeq) : nil)
        if st.uidValidity != 0 && Int64(sel.uidValidity) != st.uidValidity {
            try store.clearImapFolder(account, role: role.rawValue, bodyUIDs: ImapUID.range(role))
            st = ImapFolderRecord(accountKey: account, role: role.rawValue, name: name, epoch: st.epoch + 1)
        }
        let hasModSeq = sel.highestModSeq > 0
        let fetchItems = ImapHeaders.fetchItems + (hasModSeq ? " MODSEQ" : "")

        guard sel.exists > 0 else {
            try store.deleteImapHeaders(account, role: role.rawValue, uids: try store.imapUIDs(account, role: role.rawValue))
            st.uidValidity = Int64(sel.uidValidity)
            st.uidNext = Int64(sel.uidNext)
            st.highestModSeq = sel.highestModSeq
            st.floorUID = 0
            try store.putImapFolder(st)
            return
        }

        // Окно — последние N писем; нижний UID — по номеру (одно письмо).
        let floorSeq = max(1, sel.exists - role.limit + 1)
        let floor = Int64(try await c.fetchUID(seq: floorSeq) ?? 1)
        var known = Set(try store.imapUIDs(account, role: role.rawValue))
        // Ушедшие ниже окна — из кэша.
        let below = known.filter { $0 < floor }
        if !below.isEmpty {
            try store.deleteImapHeaders(account, role: role.rawValue, uids: Array(below))
            try store.deleteBodies(account, uids: below.map { ImapUID.make(role, epoch: st.epoch, uid: $0) })
            known.subtract(below)
        }

        // Новые письма.
        var fresh: [ImapHeaderRecord] = []
        let top = known.max() ?? (floor - 1)
        let nothingNew = st.uidNext > 0 && Int64(sel.uidNext) == st.uidNext && !known.isEmpty
        if !nothingNew {
            let from = max(floor, top + 1)
            let r = try await c.uidFetch("\(from):*", fetchItems)
            // `n:*` при n больше последнего UID возвращает последнее письмо — отсекаем.
            fresh = r.fetches.filter { ($0.uid.map(Int64.init) ?? 0) >= from }
                .compactMap { ImapHeaders.record($0, account: account, role: role) }
        }
        // Окно стало шире (удалили старые): дозабрать письма ниже известных.
        if let minKnown = known.min(), floor < minKnown {
            let r = try await c.uidFetch("\(floor):\(minKnown - 1)", fetchItems)
            fresh += r.fetches.compactMap { ImapHeaders.record($0, account: account, role: role) }
        }

        // Флаги и удалённые.
        var flagRows: [(uid: Int64, seen: Bool, draft: Bool, flagged: Bool)] = []
        var gone = Set<Int64>()
        func flags(_ f: FetchData) -> (Int64, Bool, Bool, Bool)? {
            guard let u = f.uid, f.flags != nil else { return nil }
            return (Int64(u), f.has(flag: "\\Seen"), f.has(flag: "\\Draft") || role == .drafts, f.has(flag: "\\Flagged"))
        }
        if !known.isEmpty {
            if canQresync && hasModSeq {
                // SELECT (QRESYNC ...) уже прислал изменения и удалённое.
                flagRows = sel.changed.compactMap(flags)
                for v in sel.vanished { for u in v.all where known.contains(Int64(u)) { gone.insert(Int64(u)) } }
            } else if hasModSeq && st.highestModSeq > 0 && Int64(sel.uidValidity) == st.uidValidity {
                let r = try await c.uidFetch("\(floor):*", "UID FLAGS", changedSince: st.highestModSeq)
                flagRows = r.fetches.compactMap(flags)
                for v in r.vanished { for u in v.all where known.contains(Int64(u)) { gone.insert(Int64(u)) } }
                if !(await c.qresyncEnabled) {
                    let alive = Set(try await c.uidSearch("UID \(floor):*").map(Int64.init))
                    gone.formUnion(known.subtracting(alive))
                }
            } else {
                // Без CONDSTORE: флаги всего окна; кого нет в ответе — удалены.
                let r = try await c.uidFetch("\(floor):*", "UID FLAGS")
                flagRows = r.fetches.compactMap(flags)
                let alive = Set(flagRows.map(\.uid))
                gone.formUnion(known.subtracting(alive))
            }
        }
        flagRows.removeAll { gone.contains($0.uid) || !known.contains($0.uid) }

        try store.putImapHeaders(fresh)
        try store.updateImapFlags(account, role: role.rawValue, flagRows)
        if !gone.isEmpty {
            try store.deleteImapHeaders(account, role: role.rawValue, uids: Array(gone))
            try store.deleteBodies(account, uids: gone.map { ImapUID.make(role, epoch: st.epoch, uid: $0) })
        }
        st.uidValidity = Int64(sel.uidValidity)
        st.uidNext = Int64(sel.uidNext)
        st.highestModSeq = sel.highestModSeq
        st.floorUID = floor
        try store.putImapFolder(st)
        imapLog.debug("sync \(role.rawValue, privacy: .public): +\(fresh.count) flags \(flagRows.count) -\(gone.count)")
    }

    /// Беседы из кэша заголовков: обычные (INBOX, «Отправленные»,
    /// «Черновики») и спам (Junk) — в своё пространство.
    func rebuild() throws {
        let folders = try store.imapFolders(accountKey)
        let rows = try store.imapHeaders(accountKey, roles: ImapRole.allCases.map(\.rawValue))
        func msg(_ r: ImapHeaderRecord) -> HeaderMessage? {
            guard let role = ImapRole(rawValue: r.role), let f = folders[r.role] else { return nil }
            return HeaderMessage(
                folder: ModifiedUTF7.decode(f.name), isSent: role == .sent, uid: ImapUID.make(role, epoch: f.epoch, uid: r.uid),
                messageID: r.messageID, fromName: r.fromName, fromAddr: r.fromAddr,
                to: r.to.map { ($0.n, $0.e) }, cc: r.cc.map { ($0.n, $0.e) },
                subject: r.subject, dateTs: r.dateTs, seen: r.seen, draft: r.draft
            )
        }
        let ham = rows.filter { $0.role != ImapRole.junk.rawValue }.compactMap(msg)
        let spam = rows.filter { $0.role == ImapRole.junk.rawValue }.compactMap(msg)
        let ours = self.ours
        let own = config.email.lowercased()
        try store.replaceAll(accountKey, ConversationBuilder.build(ham, ours: ours, own: own))
        try store.replaceAll(spamKey, ConversationBuilder.build(spam, ours: ours, own: own, idPrefix: "spam:", spam: true))
    }

    // MARK: Тела

    /// Имя папки по id письма.
    private func folder(_ id: Int64) throws -> (role: ImapRole, name: String, uid: UInt32)? {
        guard let (role, uid) = ImapUID.split(id), let f = try store.imapFolders(accountKey)[role.rawValue] else { return nil }
        return (role, f.name, uid)
    }

    public func bodies(_ refs: [MessageRef]) async throws -> [MessageBody] {
        guard !refs.isEmpty else { return [] }
        let cached = try store.bodies(accountKey, uids: refs.map(\.uid))
        let missing = refs.filter { cached[$0.uid] == nil }.map(\.uid)
        var fetched: [Int64: MessageBody] = [:]
        if !missing.isEmpty {
            do {
                let got = try await fetchBodies(missing, channel: nil)
                try store.putBodies(accountKey, got)
                for b in got { fetched[b.uid] = b }
            } catch {
                // Нет связи — показываем, что есть; нужен вход — наверх.
                if isAuthFailure(error) { throw apiError(error) }
            }
        }
        return refs.compactMap { cached[$0.uid] ?? fetched[$0.uid] }
    }

    /// `BODY.PEEK[]` (флаг `\Seen` не ставится) по папкам, разбор MIME.
    private func fetchBodies(_ ids: [Int64], channel: ImapChannel?) async throws -> [MessageBody] {
        var byFolder: [String: (role: ImapRole, items: [(id: Int64, uid: UInt32)])] = [:]
        for id in ids {
            guard let f = try folder(id) else { continue }
            byFolder[f.name, default: (f.role, [])].items.append((id, f.uid))
        }
        let plan = byFolder
        let raws: [(id: Int64, folder: String, data: [UInt8])] = try await (channel ?? opsChannel).run { c in
            var out: [(Int64, String, [UInt8])] = []
            for (name, group) in plan {
                _ = try await c.select(name, readOnly: true)
                let set = UIDSet(group.items.map(\.uid))
                let byUID = Dictionary(group.items.map { ($0.uid, $0.id) }, uniquingKeysWith: { a, _ in a })
                for chunk in set.chunked(200) {
                    let r = try await c.uidFetch(chunk.string, "UID BODY.PEEK[]")
                    for f in r.fetches {
                        guard let u = f.uid, let id = byUID[u], let body = f.body else { continue }
                        out.append((id, ModifiedUTF7.decode(name), body))
                    }
                }
            }
            return out
        }
        let ours = self.ours
        return raws.map { r in
            raw.put(r.id, r.data)
            return Self.body(r.data, id: r.id, folder: r.folder, ours: ours)
        }
    }

    /// Сырое письмо → тело ленты (та же модель, что у сервера ddmail).
    static func body(_ data: [UInt8], id: Int64, folder: String, ours: Set<String>) -> MessageBody {
        let p = ParsedMessage(raw: data)
        let h = p.headers
        let from = h["From"].flatMap { MailAddress.parseList($0).first }
        let fromAddr = from?.email ?? ""
        let dateTs = h["Date"].flatMap(MailDate.parse) ?? 0
        return MessageBody(
            uid: id, folder: folder, subject: h.text("Subject") ?? "",
            from: from.map { $0.name.isEmpty ? $0.email : "\($0.name) <\($0.email)>" } ?? "",
            fromAddr: fromAddr,
            to: MailAddress.parseList(h["To"] ?? "").map(\.display),
            cc: MailAddress.parseList(h["Cc"] ?? "").map(\.display),
            dateTs: dateTs, html: p.html, text: p.text,
            attachments: p.attachments.map { AttachmentDTO(filename: $0.filename, mimeType: $0.mimeType, size: Int64($0.size), index: $0.index) },
            isOutgoing: ours.contains(fromAddr),
            messageID: normalizeMessageID(h["Message-ID"] ?? ""),
            inReplyTo: extractMessageIDs(h["In-Reply-To"] ?? "").first ?? "",
            references: extractMessageIDs(h["References"] ?? "")
        )
    }

    /// Сырое письмо: из памяти или повторным `BODY.PEEK[]`.
    private func rawMessage(_ id: Int64) async throws -> [UInt8] {
        if let d = raw.get(id) { return d }
        guard let f = try folder(id) else { throw APIError.server(code: 404, message: "Письмо не найдено.") }
        let data: [UInt8]? = try await mapped {
            try await opsChannel.run { c in
                _ = try await c.select(f.name, readOnly: true)
                return try await c.uidFetch(String(f.uid), "UID BODY.PEEK[]").fetches.first?.body
            }
        }
        guard let data else { throw APIError.server(code: 404, message: "Письмо удалено на сервере.") }
        raw.put(id, data)
        return data
    }

    public func part(uid: Int64, cid: String) async throws -> Binary {
        let p = ParsedMessage(raw: try await rawMessage(uid))
        guard let part = p.part(cid: cid) else { throw APIError.server(code: 404, message: "Нет части \(cid).") }
        return Binary(data: part.data, contentType: part.mimeType)
    }

    public func attachment(uid: Int64, index: Int) async throws -> Binary {
        let p = ParsedMessage(raw: try await rawMessage(uid))
        guard let a = p.attachment(index) else { throw APIError.server(code: 404, message: "Нет вложения.") }
        return Binary(data: a.data, contentType: a.mimeType)
    }

    /// Докачка тел заранее (§3, «как у ddmail»): от свежей беседы к старой,
    /// порциями; своё соединение — открытие беседы его не ждёт.
    @discardableResult
    public func prefetchBodies(constrained: Bool, allowed: @escaping @Sendable () async -> Bool) async throws -> Int {
        let cached = try store.cachedBodyUIDs(accountKey)
        let snap = try store.snapshot([accountKey])[accountKey]
        let queue = (snap?.conversations ?? []).flatMap { c in
            (snap?.refs[c.id] ?? []).sorted { $0.uid > $1.uid }
        }.map(\.uid).filter { !cached.contains($0) }
        guard !queue.isEmpty else { return 0 }
        let channel = ImapChannel(open: Self.opener(config, password, constrained ? constrainedConnector : connector))
        defer { Task { await channel.close() } }
        var n = 0
        var i = 0
        while i < queue.count {
            if Task.isCancelled { break }
            guard await allowed() else { break }
            let chunk = Array(queue[i..<min(i + 20, queue.count)])
            i += 20
            let got: [MessageBody]
            do {
                got = try await fetchBodies(chunk, channel: channel)
            } catch {
                if isAuthFailure(error) { throw apiError(error) }
                break
            }
            try store.putBodies(accountKey, got)
            n += got.count
        }
        return n
    }

    // MARK: Изменения (оптимистично, отказ — откат и сообщение)

    /// Письма бесед по папкам: роль → (имя, UID).
    private func grouped(_ refs: [MessageRef]) throws -> [ImapRole: (name: String, uids: [UInt32])] {
        var out: [ImapRole: (name: String, uids: [UInt32])] = [:]
        for r in refs {
            guard let f = try folder(r.uid) else { continue }
            out[f.role, default: (f.name, [])].uids.append(f.uid)
        }
        return out
    }

    private func setSeen(_ refs: [MessageRef], _ seen: Bool) async throws {
        let groups = try grouped(refs)
        guard !groups.isEmpty else { return }
        for (role, g) in groups {
            try store.setImapSeen(accountKey, role: role.rawValue, uids: g.uids.map(Int64.init), seen: seen)
        }
        try rebuild()
        do {
            try await mapped {
                try await opsChannel.run { c in
                    for (_, g) in groups {
                        _ = try await c.select(g.name)
                        try await c.uidStore(UIDSet(g.uids), seen ? "+FLAGS.SILENT" : "-FLAGS.SILENT", flags: ["\\Seen"])
                    }
                }
            }
        } catch {
            for (role, g) in groups {
                try? store.setImapSeen(accountKey, role: role.rawValue, uids: g.uids.map(Int64.init), seen: !seen)
            }
            try? rebuild()
            throw error
        }
    }

    public func markRead(_ c: Conversation) async throws {
        try await setSeen(c.messages.filter { !$0.seen }, true)
    }

    public func markUnread(_ c: Conversation) async throws {
        guard let last = c.messages.max(by: { $0.uid < $1.uid }) else { return }
        try await setSeen([last], false)
    }

    /// Убрать письма из кэша сразу; вернуть, если сервер откажет.
    private func optimisticRemove(_ refs: [MessageRef], _ op: @escaping @Sendable ([ImapRole: (name: String, uids: [UInt32])], IMAPClient) async throws -> Void) async throws {
        let groups = try grouped(refs)
        guard !groups.isEmpty else { return }
        var backup: [ImapHeaderRecord] = []
        for (role, g) in groups {
            let ids = g.uids.map(Int64.init)
            backup += try store.imapHeaders(accountKey, role: role.rawValue, uids: ids)
            try store.deleteImapHeaders(accountKey, role: role.rawValue, uids: ids)
        }
        try rebuild()
        do {
            try await mapped { try await opsChannel.run { c in try await op(groups, c) } }
        } catch {
            try? store.putImapHeaders(backup)
            try? rebuild()
            throw error
        }
    }

    /// Удаление: `\Deleted` и `UID EXPUNGE` (UIDPLUS), иначе `EXPUNGE`. Необратимо.
    public func deleteConversations(_ convs: [Conversation]) async throws {
        let refs = convs.flatMap(\.messages)
        try await optimisticRemove(refs) { groups, c in
            for (_, g) in groups {
                _ = try await c.select(g.name)
                try await c.delete(UIDSet(g.uids))
            }
        }
        try? store.deleteBodies(accountKey, uids: refs.map(\.uid))
    }

    /// «В спам» — перенос в Junk (`UID MOVE`, без него — COPY и удаление).
    /// Правил «адрес/домен» нет: их держит только сервер ddmail.
    public func reportSpam(_ convs: [Conversation], scope: String) async throws -> PurgeOutcome {
        guard let junk = try store.imapFolders(accountKey)[ImapRole.junk.rawValue]?.name else {
            throw APIError.server(code: 0, message: "В ящике нет папки «Спам».")
        }
        let refs = convs.filter { !$0.spam }.flatMap(\.messages)
        try await optimisticRemove(refs) { groups, c in
            for (role, g) in groups where role != .junk {
                _ = try await c.select(g.name)
                try await c.move(UIDSet(g.uids), to: junk)
            }
        }
        try? await sync()
        return PurgeOutcome(deleted: Int64(refs.count))
    }

    /// «Не спам» — из Junk во «Входящие».
    @discardableResult
    public func restoreSpam(_ convs: [Conversation], trust: Bool) async throws -> Int {
        guard let inbox = try store.imapFolders(accountKey)[ImapRole.inbox.rawValue]?.name else { return 0 }
        let refs = convs.filter(\.spam).flatMap(\.messages)
        try await optimisticRemove(refs) { groups, c in
            for (role, g) in groups where role == .junk {
                _ = try await c.select(g.name)
                try await c.move(UIDSet(g.uids), to: inbox)
            }
        }
        try? await sync()
        return refs.count
    }

    // MARK: Отправка (контракт §5)

    /// SMTP провайдера принял письмо — для клиента оно доставлено
    /// (`delivered`): дальнейший отказ провайдер пришлёт письмом во
    /// «Входящие». Ошибка SMTP — исключение, письмо не ушло.
    @discardableResult
    public func send(_ request: SendRequest) async throws -> SendReceipt {
        guard let pw = password(), !pw.isEmpty else { throw APIError.unauthorized("нет пароля") }
        let from = MailAddress.parseList(request.from).first ?? MailAddress(email: config.email)
        let msg = OutgoingMessage(
            from: MailAddress(name: from.name.isEmpty ? config.displayName : from.name, email: from.email),
            to: request.to.flatMap { MailAddress.parseList($0) },
            cc: request.cc.flatMap { MailAddress.parseList($0) },
            subject: request.subject, text: request.text, html: request.html,
            inReplyTo: request.inReplyTo, references: request.references,
            attachments: request.attachments.map { .init(filename: $0.filename, mimeType: $0.mimeType, data: $0.content, contentID: $0.contentID) }
        )
        let (data, mid) = MIMEBuilder.build(msg)
        let recipients = (msg.to + msg.cc).map(\.email)
        let creds = config.smtpAuth ? SMTPCredentials(user: config.smtpLogin.isEmpty ? config.login : config.smtpLogin, password: pw) : nil
        try await mapped {
            let c = try await SMTPClient.connect(config.smtp, credentials: creds, connector: connector, timeout: .seconds(25))
            do {
                try await c.send(from: from.email, recipients: recipients, data: data)
            } catch {
                await c.close()
                throw error
            }
            await c.quit()
        }
        let receipt = SendReceipt(rfcMessageID: mid, deliveryStatus: DeliveryState.delivered.rawValue)
        guard !config.isGmail else { return receipt }
        Task { await self.ensureSentCopy(messageID: mid, data: data) }
        return receipt
    }

    /// Копия в «Отправленные» (у десктопа нет): через ~5 с ищем письмо по
    /// Message-ID (многие серверы кладут копию сами); не нашлось за ~15 с —
    /// `APPEND` с `\Seen`.
    func ensureSentCopy(messageID: String, data: Data, firstDelay: Duration = .seconds(5), window: Duration = .seconds(15)) async {
        guard let sent = try? store.imapFolders(accountKey)[ImapRole.sent.rawValue]?.name else {
            imapLog.notice("нет папки «Отправленные» — копию не кладём")
            return
        }
        try? await Task.sleep(for: firstDelay)
        let deadline = ContinuousClock.now + window
        while true {
            let found = (try? await opsChannel.run { c in
                _ = try await c.select(sent, readOnly: true)
                let ids = try await c.searchMessageID(messageID)
                return !ids.isEmpty
            }) ?? false
            if found { break }
            if ContinuousClock.now >= deadline {
                do {
                    try await opsChannel.run { c in try await c.append(sent, flags: ["\\Seen"], date: Date(), message: data) }
                } catch {
                    imapLog.error("APPEND в «Отправленные»: \(error.localizedDescription, privacy: .public)")
                }
                break
            }
            try? await Task.sleep(for: .seconds(3))
        }
        try? await sync()
    }

    // MARK: Новая почта на экране (контракт §6)

    /// IDLE на INBOX своим соединением, пока задачу не отменят: событие —
    /// `onEvent` (инкрементальный синк); перевыдача IDLE каждые 25 мин;
    /// обрыв — переподключение не чаще раза в 30 с; без IDLE у сервера —
    /// опрос раз в 2 мин. Неверный пароль — `onAuthFailure` и конец: ретраи
    /// его не лечат.
    public nonisolated func watch(
        idleFor: Duration = .seconds(25 * 60), pollEvery: Duration = .seconds(120), backoff: Duration = .seconds(30),
        onEvent: @escaping @Sendable () async -> Void, onAuthFailure: @escaping @Sendable () async -> Void
    ) async {
        let open = Self.opener(config, password, connector)
        let inbox = { [store, accountKey] in (try? store.imapFolders(accountKey)[ImapRole.inbox.rawValue]?.name) ?? "INBOX" }
        while !Task.isCancelled {
            let started = ContinuousClock.now
            do {
                let c = try await open()
                defer { Task { await c.close() } }
                if await c.has("IDLE") {
                    _ = try await c.select(inbox(), readOnly: true)
                    while !Task.isCancelled {
                        if try await c.idle(timeout: idleFor) { await onEvent() }
                    }
                } else {
                    await c.logout()
                    while !Task.isCancelled {
                        try await Task.sleep(for: pollEvery)
                        await onEvent()
                    }
                }
            } catch is CancellationError {
                return
            } catch {
                if isAuthFailure(error) {
                    await onAuthFailure()
                    return
                }
                imapLog.notice("IDLE \(self.accountKey, privacy: .private): \(error.localizedDescription, privacy: .public)")
            }
            // Не чаще раза в 30 с: сервер, рвущий соединение сразу, не превратит это в шторм.
            let elapsed = ContinuousClock.now - started
            if elapsed < backoff { try? await Task.sleep(for: backoff - elapsed) }
        }
    }

    /// Закрыть соединения (учётку удалили или пространство пересобирается).
    public func close() async {
        await syncChannel.close()
        await opsChannel.close()
    }
}
