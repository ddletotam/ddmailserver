import DDMailNet
import Foundation

/// IMAP-сервер в памяти для тестов синка: отвечает на команды клиента по
/// модели ящика (папки, UIDVALIDITY, письма с флагами и MODSEQ). Умеет
/// ровно то, что использует `ImapEngine`, с выключаемыми CONDSTORE/QRESYNC/MOVE.
final class FakeIMAPServer: @unchecked Sendable {
    struct Message {
        var uid: UInt32
        var flags: Set<String>
        var modseq: Int64
        var raw: String
    }

    struct Folder {
        var name: String
        var attrs: [String]
        var uidValidity: UInt32 = 1
        var uidNext: UInt32 = 1
        var messages: [Message] = []
        /// Удалённые и их MODSEQ — для VANISHED.
        var expunged: [(uid: UInt32, modseq: Int64)] = []
    }

    let lock = NSLock()
    var folders: [String: Folder] = [:]
    var order: [String] = []
    var caps: Set<String>
    var modseq: Int64 = 100
    var password = "secret"
    /// Журнал команд (без тегов) — что делал клиент.
    var log: [String] = []
    /// Команды, на которые отвечать NO (по префиксу).
    var failing: [String] = []
    /// Соединения в IDLE: им — уведомление о новом письме.
    var idlers: [(String) -> Void] = []

    init(condstore: Bool = true, qresync: Bool = true, move: Bool = true) {
        var c: Set<String> = ["IMAP4REV1", "UIDPLUS", "IDLE", "LITERAL+"]
        if condstore { c.insert("CONDSTORE") }
        if qresync { c.insert("QRESYNC") }
        if move { c.insert("MOVE") }
        caps = c
        add("INBOX", [])
        add("Sent", ["\\Sent"])
        add("Drafts", ["\\Drafts"])
        add("Junk", ["\\Junk"])
    }

    func add(_ name: String, _ attrs: [String]) {
        folders[name] = Folder(name: name, attrs: attrs)
        order.append(name)
    }

    func with<T>(_ f: (FakeIMAPServer) -> T) -> T { lock.withLock { f(self) } }

    /// Положить письмо: возвращает UID.
    @discardableResult
    func deliver(_ folder: String, from: String, to: String, subject: String, date: String = "Thu, 12 Nov 2026 10:00:00 +0300",
                 messageID: String = UUID().uuidString + "@test", seen: Bool = false, body: String = "Привет") -> UInt32 {
        lock.withLock {
            var f = folders[folder]!
            let uid = f.uidNext
            f.uidNext += 1
            modseq += 1
            let raw = "From: \(from)\r\nTo: \(to)\r\nSubject: \(subject)\r\nDate: \(date)\r\nMessage-ID: <\(messageID)>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n\(body)\r\n"
            f.messages.append(Message(uid: uid, flags: seen ? ["\\Seen"] : [], modseq: modseq, raw: raw))
            folders[folder] = f
            for notify in idlers { notify("* \(f.messages.count) EXISTS\r\n") }
            return uid
        }
    }

    func expunge(_ folder: String, uid: UInt32) {
        lock.withLock {
            modseq += 1
            folders[folder]!.messages.removeAll { $0.uid == uid }
            folders[folder]!.expunged.append((uid, modseq))
        }
    }

    func setFlag(_ folder: String, uid: UInt32, _ flag: String, _ on: Bool) {
        lock.withLock {
            modseq += 1
            guard let i = folders[folder]!.messages.firstIndex(where: { $0.uid == uid }) else { return }
            if on { folders[folder]!.messages[i].flags.insert(flag) } else { folders[folder]!.messages[i].flags.remove(flag) }
            folders[folder]!.messages[i].modseq = modseq
        }
    }

    /// Сменить UIDVALIDITY: письма перенумерованы.
    func renumber(_ folder: String) {
        lock.withLock {
            var f = folders[folder]!
            f.uidValidity += 1
            var n: UInt32 = 1
            for i in f.messages.indices { f.messages[i].uid = n; n += 1 }
            f.uidNext = n
            f.expunged = []
            folders[folder] = f
        }
    }

    func flags(_ folder: String) -> [UInt32: Set<String>] {
        lock.withLock { Dictionary(uniqueKeysWithValues: folders[folder]!.messages.map { ($0.uid, $0.flags) }) }
    }

    func uids(_ folder: String) -> [UInt32] { lock.withLock { folders[folder]!.messages.map(\.uid) } }

    var connector: FakeConnector { FakeConnector(server: self) }
}

struct FakeConnector: MailConnector {
    let server: FakeIMAPServer
    func connect(_ endpoint: MailEndpoint, timeout: Duration) async throws -> MailSocket {
        FakeIMAPSocket(server)
    }
}

/// Соединение с фальшивым сервером: команды клиента разбираются построчно.
final class FakeIMAPSocket: MailSocket, @unchecked Sendable {
    let server: FakeIMAPServer
    private let lock = NSLock()
    private var outbox: [Data] = []
    private var waiters: [CheckedContinuation<Data, Never>] = []
    private var inbuf = Data()
    private var selected: String?
    private var qresync = false
    private var idleTag: String?
    /// Ждём байты литерала APPEND.
    private var pendingAppend: (tag: String, folder: String, flags: [String], size: Int)?

    init(_ server: FakeIMAPServer) {
        self.server = server
        push("* OK [CAPABILITY \(server.with { $0.caps.sorted().joined(separator: " ") })] fake ready\r\n")
    }

    private func push(_ s: String) {
        let d = Data(s.utf8)
        lock.withLock {
            if !waiters.isEmpty { waiters.removeFirst().resume(returning: d) } else { outbox.append(d) }
        }
    }

    func receive() async throws -> Data {
        await withCheckedContinuation { (c: CheckedContinuation<Data, Never>) in
            lock.withLock {
                if !outbox.isEmpty { c.resume(returning: outbox.removeFirst()) } else { waiters.append(c) }
            }
        }
    }

    func startTLS() async throws {}
    func close() {}

    func send(_ data: Data) async throws {
        lock.withLock { inbuf.append(data) }
        while true {
            let line: String? = lock.withLock {
                if let p = pendingAppend {
                    guard inbuf.count >= p.size + 2 else { return nil }
                    let msg = String(decoding: inbuf.prefix(p.size), as: UTF8.self)
                    inbuf.removeFirst(p.size + 2)
                    pendingAppend = nil
                    return "\u{0}APPEND\u{0}\(p.tag)\u{0}\(p.folder)\u{0}\(p.flags.joined(separator: " "))\u{0}\(msg)"
                }
                guard let r = inbuf.range(of: Data("\r\n".utf8)) else { return nil }
                let l = String(decoding: inbuf[inbuf.startIndex..<r.lowerBound], as: UTF8.self)
                inbuf.removeSubrange(inbuf.startIndex..<r.upperBound)
                return l
            }
            guard let line else { return }
            handle(line)
        }
    }

    private func handle(_ line: String) {
        if line.hasPrefix("\u{0}APPEND") {
            let p = line.split(separator: "\u{0}", omittingEmptySubsequences: false).map(String.init)
            let tag = p[2], folder = p[3], flags = p[4].split(separator: " ").map(String.init), raw = p[5]
            server.with { s in
                var f = s.folders[folder]!
                s.modseq += 1
                f.messages.append(.init(uid: f.uidNext, flags: Set(flags), modseq: s.modseq, raw: raw))
                f.uidNext += 1
                s.folders[folder] = f
            }
            push("\(tag) OK APPEND done\r\n")
            return
        }
        if line == "DONE" {
            if let t = idleTag {
                idleTag = nil
                push("\(t) OK IDLE terminated\r\n")
            }
            return
        }
        let sp = line.firstIndex(of: " ") ?? line.endIndex
        let tag = String(line[..<sp])
        let cmd = line[sp...].trimmingCharacters(in: .whitespaces)
        server.with { $0.log.append(cmd) }
        if server.with({ s in s.failing.contains { cmd.hasPrefix($0) } }) {
            push("\(tag) NO [SERVERBUG] fails\r\n")
            return
        }
        let upper = cmd.uppercased()
        func ok(_ t: String = "done") { push("\(tag) OK \(t)\r\n") }

        if upper == "CAPABILITY" {
            push("* CAPABILITY \(server.with { $0.caps.sorted().joined(separator: " ") })\r\n")
            ok()
        } else if upper.hasPrefix("LOGIN") {
            let parts = cmd.components(separatedBy: "\"")
            let pw = parts.count >= 4 ? parts[3] : ""
            if pw == server.with({ $0.password }) { ok("logged in") } else { push("\(tag) NO [AUTHENTICATIONFAILED] Invalid credentials\r\n") }
        } else if upper.hasPrefix("ENABLE") {
            if upper.contains("QRESYNC"), server.with({ $0.caps.contains("QRESYNC") }) {
                qresync = true
                push("* ENABLED QRESYNC\r\n")
            }
            ok()
        } else if upper.hasPrefix("LIST") {
            for name in server.with({ $0.order }) {
                let a = server.with { $0.folders[name]!.attrs }
                push("* LIST (\(a.joined(separator: " "))) \"/\" \"\(name)\"\r\n")
            }
            ok()
        } else if upper.hasPrefix("SELECT") || upper.hasPrefix("EXAMINE") {
            select(tag, cmd)
        } else if upper.hasPrefix("FETCH") {
            // FETCH <seq> (UID)
            let seq = Int(cmd.split(separator: " ")[1]) ?? 1
            let msgs = server.with { $0.folders[selected!]!.messages }
            if seq >= 1 && seq <= msgs.count { push("* \(seq) FETCH (UID \(msgs[seq - 1].uid))\r\n") }
            ok()
        } else if upper.hasPrefix("UID FETCH") {
            uidFetch(tag, cmd)
        } else if upper.hasPrefix("UID SEARCH HEADER") {
            let id = cmd.components(separatedBy: "\"").dropFirst().first ?? ""
            let msgs = server.with { $0.folders[selected!]!.messages }
            let hits = msgs.filter { $0.raw.contains("Message-ID: \(id)") }.map { String($0.uid) }
            push("* SEARCH \(hits.joined(separator: " "))\r\n".replacingOccurrences(of: "SEARCH \r", with: "SEARCH\r"))
            ok()
        } else if upper.hasPrefix("UID SEARCH UID") {
            let range = parseRange(String(cmd.split(separator: " ")[3]))
            let msgs = server.with { $0.folders[selected!]!.messages }
            let hits = msgs.filter { range($0.uid) }.map { String($0.uid) }
            push(hits.isEmpty ? "* SEARCH\r\n" : "* SEARCH \(hits.joined(separator: " "))\r\n")
            ok()
        } else if upper.hasPrefix("UID STORE") {
            let p = cmd.split(separator: " ")
            let range = parseRange(String(p[2]))
            let op = String(p[3])
            let flag = cmd[cmd.firstIndex(of: "(")!...].trimmingCharacters(in: CharacterSet(charactersIn: "() "))
            let folder = selected!
            for u in server.uids(folder) where range(u) { server.setFlag(folder, uid: u, flag, op.hasPrefix("+")) }
            ok()
        } else if upper.hasPrefix("UID EXPUNGE") || upper == "EXPUNGE" {
            let folder = selected!
            let range = upper == "EXPUNGE" ? { (_: UInt32) in true } : parseRange(String(cmd.split(separator: " ")[2]))
            let gone = server.with { s in s.folders[folder]!.messages.filter { range($0.uid) && $0.flags.contains("\\Deleted") }.map(\.uid) }
            for u in gone {
                server.expunge(folder, uid: u)
                push(qresync ? "* VANISHED \(u)\r\n" : "* 1 EXPUNGE\r\n")
            }
            ok()
        } else if upper.hasPrefix("UID MOVE") || upper.hasPrefix("UID COPY") {
            let p = cmd.split(separator: " ")
            let range = parseRange(String(p[2]))
            let dest = cmd.components(separatedBy: "\"")[1]
            let from = selected!
            let moving = server.with { s in s.folders[from]!.messages.filter { range($0.uid) } }
            for m in moving {
                server.with { s in
                    var f = s.folders[dest]!
                    s.modseq += 1
                    f.messages.append(.init(uid: f.uidNext, flags: m.flags, modseq: s.modseq, raw: m.raw))
                    f.uidNext += 1
                    s.folders[dest] = f
                }
                if upper.hasPrefix("UID MOVE") { server.expunge(from, uid: m.uid) }
            }
            ok()
        } else if upper.hasPrefix("APPEND") {
            // APPEND "Sent" (\Seen) "date" {n+}
            let folder = cmd.components(separatedBy: "\"")[1]
            let flags = cmd.firstIndex(of: "(").map { i in cmd[i...].prefix { $0 != ")" }.dropFirst().split(separator: " ").map(String.init) } ?? []
            let open = cmd.lastIndex(of: "{")!
            let size = Int(cmd[cmd.index(after: open)...].prefix { $0.isNumber })!
            lock.withLock { pendingAppend = (tag, folder, flags, size) }
        } else if upper == "IDLE" {
            idleTag = tag
            server.with { $0.idlers.append { [weak self] line in self?.push(line) } }
            push("+ idling\r\n")
        } else if upper == "LOGOUT" {
            push("* BYE bye\r\n")
            ok()
        } else {
            ok()
        }
    }

    private func select(_ tag: String, _ cmd: String) {
        let name = cmd.components(separatedBy: "\"")[1]
        selected = name
        let f = server.with { $0.folders[name]! }
        let condstore = server.with { $0.caps.contains("CONDSTORE") }
        push("* \(f.messages.count) EXISTS\r\n")
        push("* OK [UIDVALIDITY \(f.uidValidity)] ok\r\n")
        push("* OK [UIDNEXT \(f.uidNext)] ok\r\n")
        let highest = server.with { $0.modseq }
        push(condstore ? "* OK [HIGHESTMODSEQ \(highest)] ok\r\n" : "* OK [NOMODSEQ] no\r\n")
        if qresync, let r = cmd.range(of: "(QRESYNC ("), let close = cmd[r.upperBound...].firstIndex(of: ")") {
            let p = cmd[r.upperBound..<close].split(separator: " ")
            if UInt32(p[0]) == f.uidValidity, let since = Int64(p[1]) {
                let gone = f.expunged.filter { $0.modseq > since }.map { String($0.uid) }
                if !gone.isEmpty { push("* VANISHED (EARLIER) \(gone.joined(separator: ","))\r\n") }
                for (i, m) in f.messages.enumerated() where m.modseq > since {
                    push("* \(i + 1) FETCH (UID \(m.uid) FLAGS (\(m.flags.sorted().joined(separator: " "))) MODSEQ (\(m.modseq)))\r\n")
                }
            }
        }
        push("\(tag) OK [READ-WRITE] selected\r\n")
    }

    private func parseRange(_ s: String) -> (UInt32) -> Bool {
        let parts = s.split(separator: ",").map { p -> (UInt32, UInt32) in
            let e = p.split(separator: ":")
            let a = UInt32(e[0]) ?? 0
            let b = e.count > 1 ? (e[1] == "*" ? UInt32.max : UInt32(e[1]) ?? a) : a
            return (min(a, b), max(a, b))
        }
        return { u in parts.contains { u >= $0.0 && u <= $0.1 } }
    }

    private func uidFetch(_ tag: String, _ cmd: String) {
        let p = cmd.split(separator: " ")
        let set = String(p[2])
        let msgs = server.with { $0.folders[selected!]!.messages }
        var range = parseRange(set)
        // `n:*` с n больше последнего — последнее письмо (как у настоящих серверов).
        if set.hasSuffix(":*"), let n = UInt32(set.dropLast(2)), let last = msgs.last, n > last.uid {
            let u = last.uid
            range = { $0 == u }
        }
        let since: Int64? = cmd.range(of: "CHANGEDSINCE ").map { r in Int64(cmd[r.upperBound...].prefix { $0.isNumber })! }
        let header = cmd.contains("HEADER.FIELDS")
        let body = cmd.contains("BODY.PEEK[]")
        for (i, m) in msgs.enumerated() where range(m.uid) {
            if let since, m.modseq <= since { continue }
            var items = "UID \(m.uid) FLAGS (\(m.flags.sorted().joined(separator: " ")))"
            if cmd.contains("MODSEQ") || since != nil { items += " MODSEQ (\(m.modseq))" }
            if header {
                let h = m.raw.components(separatedBy: "\r\n\r\n")[0] + "\r\n\r\n"
                items += " INTERNALDATE \"12-Nov-2026 10:00:00 +0300\" BODY[HEADER.FIELDS (FROM TO)] {\(h.utf8.count)}\r\n\(h)"
            }
            if body { items += " BODY[] {\(m.raw.utf8.count)}\r\n\(m.raw)" }
            push("* \(i + 1) FETCH (\(items))\r\n")
        }
        if let since, cmd.contains("VANISHED") {
            let f = server.with { $0.folders[selected!]! }
            let gone = f.expunged.filter { $0.modseq > since }.map { String($0.uid) }
            if !gone.isEmpty { push("* VANISHED (EARLIER) \(gone.joined(separator: ","))\r\n") }
        }
        push("\(tag) OK fetched\r\n")
    }
}
