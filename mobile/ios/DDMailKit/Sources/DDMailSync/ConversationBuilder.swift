import CryptoKit
import DDMailAPI
import Foundation

/// Письмо для построения бесед: заголовки из кэша IMAP-учётки.
public struct HeaderMessage: Equatable, Sendable {
    public var folder: String
    /// Папка «Отправленные»: при дублях по Message-ID побеждает её копия.
    public var isSent: Bool
    public var uid: Int64
    public var messageID: String
    public var fromName: String
    public var fromAddr: String
    /// `(имя, адрес)` получателей.
    public var to: [(name: String, addr: String)]
    public var cc: [(name: String, addr: String)]
    public var subject: String
    /// Секунды.
    public var dateTs: Int64
    public var seen: Bool
    public var draft: Bool

    public init(
        folder: String, isSent: Bool = false, uid: Int64, messageID: String = "", fromName: String = "", fromAddr: String,
        to: [(name: String, addr: String)] = [], cc: [(name: String, addr: String)] = [], subject: String = "",
        dateTs: Int64 = 0, seen: Bool = true, draft: Bool = false
    ) {
        self.folder = folder; self.isSent = isSent; self.uid = uid; self.messageID = messageID
        self.fromName = fromName; self.fromAddr = fromAddr; self.to = to; self.cc = cc
        self.subject = subject; self.dateTs = dateTs; self.seen = seen; self.draft = draft
    }

    public static func == (a: HeaderMessage, b: HeaderMessage) -> Bool {
        a.folder == b.folder && a.uid == b.uid && a.messageID == b.messageID && a.fromAddr == b.fromAddr
            && a.to.map(\.addr) == b.to.map(\.addr) && a.cc.map(\.addr) == b.cc.map(\.addr)
            && a.subject == b.subject && a.dateTs == b.dateTs && a.seen == b.seen && a.draft == b.draft
    }
}

/// Беседы на устройстве — порт `fetch_conversations_impl` /
/// `conversation_participants` / `conversation_id` десктопа и
/// `groupConversations` сервера (`internal/web/handlers_desktop.go`). Один и
/// тот же ящик приезжает и через ddmail, и по IMAP — id обязаны совпасть
/// байт в байт.
///
/// Там, где десктоп и сервер расходятся, правит сервер (он новее и чинил
/// ошибки десктопа): если нашего адреса в видимых участниках нет (bcc,
/// срезанные заголовки), в набор добавляется адрес учётки (`own`) — тогда
/// входящее по bcc и наш ответ складываются в одну беседу с одним id, а не
/// в две строки с одинаковым id (ошибка десктопа). Набор с `own` и id
/// беседы взаимно однозначны, так что группировка по набору — это
/// группировка по готовому id (контракт внешних учёток §3).
public enum ConversationBuilder {
    /// Набор адресов письма — ключ диалога: From+To+Cc в нижнем регистре,
    /// без Bcc, без повторов, по алфавиту (`conversation_participants`).
    public static func participants(from: String, to: [String], cc: [String]) -> [String] {
        var seen = Set<String>()
        var out: [String] = []
        for a in [from] + to + cc {
            let lc = a.trimmingCharacters(in: .whitespaces).lowercased()
            if !lc.isEmpty && seen.insert(lc).inserted { out.append(lc) }
        }
        return out.sorted()
    }

    /// id диалога — функция набора (`conversation_id`).
    public static func conversationID(mine: [String], others: [String], own: String) -> String {
        let mineKey = mine.isEmpty && !own.isEmpty ? own.lowercased() : mine.joined(separator: "+")
        if others.isEmpty { return "\(mineKey)|self" }
        if others.count > 1 { return "\(mineKey)|group:\(others.joined(separator: ","))" }
        return "\(mineKey)|\(others.joined(separator: ","))"
    }

    public static func gravatarHash(_ email: String) -> String {
        let d = Insecure.MD5.hash(data: Data(email.trimmingCharacters(in: .whitespaces).lowercased().utf8))
        return d.map { String(format: "%02x", $0) }.joined()
    }

    /// Беседы из писем. `ours` — свои адреса учётки (нижний регистр),
    /// `own` — адрес учётки. Порядок — по дате последнего письма, новые сверху.
    public static func build(_ messages: [HeaderMessage], ours: Set<String>, own: String, idPrefix: String = "", spam: Bool = false) -> [ConversationDTO] {
        let isOurs = { (a: String) in ours.contains(a.lowercased()) }
        let ownLc = own.lowercased()
        let sortedOurs = ours.sorted()

        var groups: [String: (participants: [String], msgs: [HeaderMessage])] = [:]
        var order: [String] = []
        for m in messages {
            var p = participants(from: m.fromAddr, to: m.to.map(\.addr), cc: m.cc.map(\.addr))
            if p.isEmpty { continue }
            if !p.contains(where: isOurs), !ownLc.isEmpty, ours.contains(ownLc) {
                p.append(ownLc)
                p.sort()
            }
            let key = p.joined(separator: ",")
            if groups[key] == nil {
                groups[key] = (p, [])
                order.append(key)
            }
            groups[key]!.msgs.append(m)
        }

        var out: [ConversationDTO] = []
        for key in order {
            let (participants, raw) = groups[key]!
            // Стабильная сортировка по дате (как сортировки десктопа и сервера).
            let entries = dedup(raw.enumerated().sorted { ($0.element.dateTs, $0.offset) < ($1.element.dateTs, $1.offset) }.map(\.element))
            let regular = entries.filter { !$0.draft }
            // Черновики сами беседы не начинают.
            guard let first = regular.first, let last = regular.last else { continue }

            let mine = participants.filter(isOurs)
            let others = participants.filter { !isOurs($0) }
            var cpAddrs: [String]
            var myID = ""
            if !others.isEmpty {
                cpAddrs = others
                // Айдентика — та, что участвовала в первом письме.
                let firstFrom = first.fromAddr.lowercased()
                if isOurs(firstFrom) {
                    myID = firstFrom
                } else if let r = (first.to + first.cc).map({ $0.addr.lowercased() }).first(where: isOurs) {
                    myID = r
                }
                if myID.isEmpty, let m = mine.first { myID = m }
                if myID.isEmpty, ours.contains(ownLc) { myID = ownLc }
                if myID.isEmpty, let s = sortedOurs.first { myID = s }
            } else {
                // Все адреса — мои: собеседник — автор первого письма.
                var cp = first.fromAddr.lowercased()
                if !participants.contains(cp) { cp = participants[0] }
                cpAddrs = [cp]
                myID = participants.first { $0 != cp } ?? cp
            }

            let counterparts = cpAddrs.map { ContactInfo(name: nameFor($0, entries), addr: $0) }
            let isGroup = others.count > 1
            var mineKey = mine.joined(separator: "+")
            if mineKey.isEmpty && !myID.isEmpty { mineKey = myID }
            let label: String
            let id: String
            if others.isEmpty {
                label = counterparts[0].name.isEmpty ? counterparts[0].addr : counterparts[0].name
                id = "\(mineKey)|self"
            } else if isGroup {
                let subject = first.subject.trimmingCharacters(in: .whitespaces)
                label = "\(subject.isEmpty ? "(без темы)" : subject) (\(participants.count) чел)"
                id = "\(mineKey)|group:\(others.joined(separator: ","))"
            } else {
                label = counterparts[0].name.isEmpty ? counterparts[0].addr : counterparts[0].name
                id = "\(mineKey)|\(others.joined(separator: ","))"
            }

            out.append(ConversationDTO(
                id: idPrefix + id, label: label, avatarHash: gravatarHash(cpAddrs[0]), receivedBy: myID,
                counterparts: counterparts, isGroup: isGroup, lastDateTs: last.dateTs, lastSubject: last.subject,
                unreadCount: regular.filter { !$0.seen }.count, totalCount: regular.count,
                messages: regular.map { MessageRef(folder: $0.folder, uid: $0.uid, messageID: $0.messageID, seen: $0.seen) },
                updatedAt: 0, spam: spam
            ))
        }
        // По дате последнего письма, новые сверху; равные — по id (порядок не случайный).
        return out.sorted { ($0.lastDateTs, $1.id) > ($1.lastDateTs, $0.id) }
    }

    /// Одно письмо в нескольких папках (письмо себе — и в «Отправленных», и
    /// во «Входящих»): оставить одну копию, «Отправленные» побеждают.
    static func dedup(_ msgs: [HeaderMessage]) -> [HeaderMessage] {
        var seen: [String: Int] = [:]
        var keep = Array(repeating: true, count: msgs.count)
        for (i, m) in msgs.enumerated() where !m.messageID.isEmpty {
            if let j = seen[m.messageID] {
                if m.isSent && !msgs[j].isSent {
                    keep[j] = false
                    seen[m.messageID] = i
                } else {
                    keep[i] = false
                }
            } else {
                seen[m.messageID] = i
            }
        }
        return msgs.enumerated().filter { keep[$0.offset] }.map(\.element)
    }

    /// Имя собеседника (`nameForAddr` сервера): первое по письмам — из From
    /// его писем или из To/Cc наших; похожее на адрес — не имя.
    static func nameFor(_ addr: String, _ entries: [HeaderMessage]) -> String {
        let lc = addr.lowercased()
        func good(_ n: String) -> String? {
            let c = cleanName(n)
            return c.isEmpty || c.contains("@") ? nil : c
        }
        for e in entries {
            if e.fromAddr.lowercased() == lc, let n = good(e.fromName) { return n }
            for r in e.to + e.cc where r.addr.lowercased() == lc {
                if let n = good(r.name) { return n }
            }
        }
        return ""
    }

    static func cleanName(_ s: String) -> String {
        var r = s
        if let i = r.firstIndex(of: "<") { r = String(r[..<i]) }
        return r.trimmingCharacters(in: .whitespaces).trimmingCharacters(in: CharacterSet(charactersIn: "\"'"))
    }
}
