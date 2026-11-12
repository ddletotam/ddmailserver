import DDMailAPI
import DDMailIMAP
import DDMailMIME
import DDMailNet
import DDMailStore
import Foundation

/// Роль папки IMAP-учётки (контракт внешних учёток §3). Синкаются INBOX,
/// «Отправленные», «Черновики» и Junk — как папка «Спам».
public enum ImapRole: String, CaseIterable, Sendable {
    case inbox, sent, drafts, junk

    /// Окно — последние N писем (лимиты десктопа).
    public var limit: Int {
        switch self {
        case .inbox: 5000
        case .sent: 2500
        case .drafts: 1250
        case .junk: 500
        }
    }

    /// Старшие биты id письма. INBOX — самая старшая: рубеж уведомлений
    /// учётки — максимум id, и входящие должны быть «новее» отправленных.
    var slot: Int64 {
        switch self {
        case .sent: 1
        case .drafts: 2
        case .junk: 3
        case .inbox: 4
        }
    }

    init?(slot: Int64) {
        guard let r = ImapRole.allCases.first(where: { $0.slot == slot }) else { return nil }
        self = r
    }
}

/// id письма IMAP-учётки для ленты и кэша тел: в учётке письма разных папок
/// с одним UID — разные письма, а лента знает только число. Число — в
/// пределах 2^53 (JavaScript ленты): роль (3 бита) · поколение UIDVALIDITY
/// (18 бит) · UID (32 бита). Поколение растёт при смене UIDVALIDITY — новые
/// письма после перенумерации «новее» старых.
public enum ImapUID {
    public static func make(_ role: ImapRole, epoch: Int64, uid: Int64) -> Int64 {
        role.slot << 50 | (epoch & 0x3FFFF) << 32 | (uid & 0xFFFF_FFFF)
    }

    public static func split(_ id: Int64) -> (role: ImapRole, uid: UInt32)? {
        guard let r = ImapRole(slot: id >> 50) else { return nil }
        return (r, UInt32(truncatingIfNeeded: id & 0xFFFF_FFFF))
    }

    /// Все id папки (для чистки тел при смене UIDVALIDITY).
    public static func range(_ role: ImapRole) -> ClosedRange<Int64> {
        (role.slot << 50)...(role.slot << 50 | ((1 << 50) - 1))
    }
}

/// Папки ящика по ролям: SPECIAL-USE (`\Sent`, `\Drafts`, `\Junk`; Mail.ru
/// пишет `\Spam`), иначе по именам (`find_sent_folder` десктопа и такие же
/// списки для остальных).
public enum ImapFolders {
    static let names: [ImapRole: [String]] = [
        .sent: ["Sent", "INBOX.Sent", "Sent Messages", "Sent Items", "Отправленные"],
        .drafts: ["Drafts", "INBOX.Drafts", "Черновики"],
        .junk: ["Junk", "Spam", "INBOX.Junk", "INBOX.Spam", "Спам"],
    ]
    static let attrs: [ImapRole: [String]] = [
        .sent: ["\\Sent"], .drafts: ["\\Drafts"], .junk: ["\\Junk", "\\Spam"],
    ]

    /// Роль → имя на сервере (modified UTF-7).
    public static func resolve(_ list: [ListEntry]) -> [ImapRole: String] {
        let usable = list.filter { !$0.has("\\Noselect") && !$0.has("\\NonExistent") }
        var out: [ImapRole: String] = [:]
        if let inbox = usable.first(where: { $0.rawName.caseInsensitiveCompare("INBOX") == .orderedSame }) {
            out[.inbox] = inbox.rawName
        }
        for role in [ImapRole.sent, .drafts, .junk] {
            if let e = usable.first(where: { e in attrs[role]!.contains { e.has($0) } }) {
                out[role] = e.rawName
                continue
            }
            for n in names[role]! {
                if let e = usable.first(where: { $0.name.caseInsensitiveCompare(n) == .orderedSame }) {
                    out[role] = e.rawName
                    break
                }
            }
            if out[role] == nil, role == .sent, let e = usable.first(where: { $0.name.lowercased().contains("sent") }) {
                out[role] = e.rawName
            }
        }
        return out
    }
}

/// Заголовки FETCH → строка кэша. `From` — из сырого заголовка (регистр
/// имени), RFC 2047 раскодирован; дата — `Date`, не разобралась — `INTERNALDATE`.
enum ImapHeaders {
    static let fetchItems = "UID FLAGS ENVELOPE INTERNALDATE BODY.PEEK[HEADER.FIELDS (FROM TO CC DATE SUBJECT MESSAGE-ID IN-REPLY-TO REFERENCES CONTENT-TYPE)]"

    static func record(_ f: FetchData, account: String, role: ImapRole) -> ImapHeaderRecord? {
        guard let uid = f.uid else { return nil }
        let h = HeaderBlock(f.header ?? [])
        let env = f.envelope
        let from = h["From"].flatMap { MailAddress.parseList($0).first }
            ?? env?.from.first.map { MailAddress(name: $0.name, email: $0.email) }
        func list(_ name: String, _ fallback: [EnvelopeAddress]?) -> [StoredAddress] {
            if let raw = h[name] {
                return MailAddress.parseList(raw).map { StoredAddress(name: $0.name, email: $0.email) }
            }
            return (fallback ?? []).map { StoredAddress(name: $0.name, email: $0.email) }
        }
        let subject = h.text("Subject") ?? env.map { EncodedWord.decode($0.subject) } ?? ""
        let dateRaw = h["Date"] ?? env?.date ?? ""
        let ts = MailDate.parse(dateRaw) ?? f.internalDate.flatMap(MailDate.parse) ?? 0
        let mid = normalizeMessageID(h["Message-ID"] ?? env?.messageID ?? "")
        let irt = extractMessageIDs(h["In-Reply-To"] ?? env?.inReplyTo ?? "").first ?? ""
        return ImapHeaderRecord(
            accountKey: account, role: role.rawValue, uid: Int64(uid), messageID: mid,
            fromName: from?.name ?? "", fromAddr: from?.email ?? "",
            to: list("To", env?.to), cc: list("Cc", env?.cc), subject: subject, dateTs: ts, dateRaw: dateRaw,
            seen: f.has(flag: "\\Seen"), draft: f.has(flag: "\\Draft") || role == .drafts, flagged: f.has(flag: "\\Flagged"),
            inReplyTo: irt, references: extractMessageIDs(h["References"] ?? ""),
            hasAttachments: (h["Content-Type"] ?? "").lowercased().contains("multipart/mixed")
        )
    }
}

/// Последовательный канал к серверу: одно соединение, команды по одной;
/// оборвалось переиспользуемое соединение — один повтор на свежем.
actor ImapChannel {
    private let open: @Sendable () async throws -> IMAPClient
    private var client: IMAPClient?
    private var lastUsed = Date.distantPast
    private var tail: Task<Void, Never>?
    /// Простоявшее дольше соединение не переиспользуем: серверы режут тихие.
    static let maxIdle: TimeInterval = 240

    init(open: @escaping @Sendable () async throws -> IMAPClient) {
        self.open = open
    }

    func run<T: Sendable>(_ body: @escaping @Sendable (IMAPClient) async throws -> T) async throws -> T {
        let previous = tail
        let task = Task { () throws -> T in
            await previous?.value
            return try await self.attempt(body)
        }
        tail = Task { _ = try? await task.value }
        return try await task.value
    }

    private func attempt<T: Sendable>(_ body: @escaping @Sendable (IMAPClient) async throws -> T) async throws -> T {
        var reused = false
        let c: IMAPClient
        if let cur = client, Date().timeIntervalSince(lastUsed) < Self.maxIdle {
            c = cur
            reused = true
        } else {
            await client?.close()
            client = nil
            c = try await open()
            client = c
        }
        do {
            let r = try await body(c)
            lastUsed = Date()
            return r
        } catch {
            await c.close()
            if client === c { client = nil }
            if reused, let m = error as? MailNetError, [.closed, .timeout].contains(m) || { if case .connect = m { true } else { false } }() {
                let fresh = try await open()
                client = fresh
                let r = try await body(fresh)
                lastUsed = Date()
                return r
            }
            throw error
        }
    }

    func close() async {
        await client?.logout()
        client = nil
    }
}

/// Последние сырые письма в памяти: части `cid:` и вложения открытой беседы
/// не перекачиваются (контракт §3: «из кэша сырого письма или повторным BODY.PEEK[]»).
struct RawCache {
    private var items: [(id: Int64, data: [UInt8])] = []
    private var bytes = 0
    let limit: Int

    init(limit: Int = 32 << 20) { self.limit = limit }

    mutating func get(_ id: Int64) -> [UInt8]? {
        guard let i = items.firstIndex(where: { $0.id == id }) else { return nil }
        let it = items.remove(at: i)
        items.append(it)
        return it.data
    }

    mutating func put(_ id: Int64, _ data: [UInt8]) {
        if let i = items.firstIndex(where: { $0.id == id }) {
            bytes -= items[i].data.count
            items.remove(at: i)
        }
        items.append((id, data))
        bytes += data.count
        while bytes > limit, items.count > 1 {
            bytes -= items.removeFirst().data.count
        }
    }
}

/// Первый массив JSON верхнего уровня в тексте (`extract_top_level_array`
/// десктопа): скобки внутри строк не считаются.
func extractTopLevelArray(_ s: String) -> String? {
    let b = Array(s.utf8)
    guard let start = b.firstIndex(of: 91) else { return nil }
    var depth = 0
    var inStr = false
    var esc = false
    for i in start..<b.count {
        let c = b[i]
        if inStr {
            if esc { esc = false } else if c == 92 { esc = true } else if c == 34 { inStr = false }
            continue
        }
        switch c {
        case 34: inStr = true
        case 91: depth += 1
        case 93:
            depth -= 1
            if depth == 0 { return String(decoding: b[start...i], as: UTF8.self) }
        default: break
        }
    }
    return nil
}
