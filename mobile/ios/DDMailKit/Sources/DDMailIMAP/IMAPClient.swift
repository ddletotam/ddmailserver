import DDMailMIME
import DDMailNet
import Foundation

extension LineStream: LineStreamReading {}

/// Часть команды: текст как есть или строка (кавычки либо литерал — решает клиент).
public enum IMAPArg: Sendable {
    case raw(String)
    case string(String)
    case literal(Data)
}

/// Итог команды: всё нетегированное, что пришло до ответа, и сам ответ.
public struct IMAPResult: Sendable {
    public var untagged: [Untagged]
    public var code: ResponseCode?
    public var text: String

    public var fetches: [FetchData] {
        untagged.compactMap { if case let .fetch(_, f) = $0 { f } else { nil } }
    }

    public var vanished: [UIDSet] {
        untagged.compactMap { if case let .vanished(_, s) = $0 { s } else { nil } }
    }

    public var searchUIDs: [UInt32] {
        untagged.flatMap { u -> [UInt32] in if case let .search(s) = u { s } else { [] } }
    }
}

/// Состояние папки после SELECT/EXAMINE.
public struct SelectedMailbox: Sendable {
    public var exists = 0
    public var uidValidity: UInt32 = 0
    public var uidNext: UInt32 = 0
    /// 0 — сервер не ведёт MODSEQ для папки (`NOMODSEQ`) или нет CONDSTORE.
    public var highestModSeq: Int64 = 0
    /// QRESYNC: удалённые с прошлого раза (`VANISHED (EARLIER)`).
    public var vanished: [UIDSet] = []
    /// QRESYNC: изменившиеся флаги с прошлого раза.
    public var changed: [FetchData] = []
}

/// Клиент IMAP4rev1/rev2 поверх `MailSocket`: команды по одной, без
/// конвейера; всё нетегированное копится в итог команды. Не держит
/// состояния синхронизации — это дело движка.
public actor IMAPClient {
    let stream: LineStream
    private var tagN = 0
    public private(set) var capabilities: Set<String> = []
    public private(set) var qresyncEnabled = false
    public private(set) var selected: String?

    init(stream: LineStream) {
        self.stream = stream
    }

    /// Соединиться, прочесть приветствие, STARTTLS при необходимости. Вход — отдельно.
    public static func connect(_ endpoint: MailEndpoint, connector: MailConnector, timeout: Duration = .seconds(25)) async throws -> IMAPClient {
        let socket = try await connector.connect(endpoint, timeout: timeout)
        let stream = LineStream(socket: socket, timeout: timeout)
        let c = IMAPClient(stream: stream)
        do {
            try await c.greet(startTLS: endpoint.security == .startTLS)
        } catch {
            stream.close()
            throw error
        }
        stream.timeout = .seconds(60)
        return c
    }

    private func greet(startTLS: Bool) async throws {
        let g = try IMAPResponse.parse(try await readIMAPResponse(stream))
        guard case let .untagged(.status(st, code, text)) = g, st == .ok || st == .preauth else {
            if case let .untagged(.status(.bye, _, text)) = g { throw MailNetError.server(text) }
            throw MailNetError.protocolError("нет приветствия IMAP")
        }
        absorb(code)
        _ = text
        if startTLS {
            _ = try await run("STARTTLS")
            try await stream.startTLS()
            capabilities = []
        }
        if capabilities.isEmpty { try await refreshCapabilities() }
    }

    public func refreshCapabilities() async throws {
        let r = try await run("CAPABILITY")
        for u in r.untagged { if case let .capability(c) = u { capabilities = Set(c) } }
    }

    public func has(_ cap: String) -> Bool { capabilities.contains(cap.uppercased()) }

    private func absorb(_ code: ResponseCode?) {
        if let code, code.name == "CAPABILITY" { capabilities = Set(code.args.map { $0.uppercased() }) }
    }

    // MARK: Вход

    /// LOGIN; отказ — `MailNetError.auth`. После входа — возможности заново
    /// (сервер расширяет их после аутентификации) и `ENABLE QRESYNC`.
    public func login(user: String, password: String) async throws {
        if has("LOGINDISABLED") { throw MailNetError.server("Сервер запрещает вход паролем.") }
        do {
            let r = try await run([.raw("LOGIN "), .string(user), .raw(" "), .string(password)])
            // Сервер расширяет возможности после входа; прислал их в ответе — не спрашиваем.
            if r.code?.name != "CAPABILITY" { try? await refreshCapabilities() }
        } catch let MailNetError.server(text) {
            // `NO [UNAVAILABLE]` и подобное — временное; прочий отказ — неверный вход.
            if text.uppercased().contains("UNAVAILABLE") || text.uppercased().contains("SERVERBUG") { throw MailNetError.server(text) }
            throw MailNetError.auth(text)
        }
        if has("QRESYNC") || has("CONDSTORE") {
            let what = has("QRESYNC") ? "QRESYNC" : "CONDSTORE"
            if let r = try? await run("ENABLE \(what)") {
                for u in r.untagged { if case let .enabled(e) = u, e.contains("QRESYNC") { qresyncEnabled = true } }
            }
        }
    }

    public func logout() async {
        _ = try? await run("LOGOUT")
        stream.close()
    }

    public func close() { stream.close() }

    // MARK: Папки

    public func list() async throws -> [ListEntry] {
        let r = try await run("LIST \"\" \"*\"")
        return r.untagged.compactMap { if case let .list(l) = $0 { l } else { nil } }
    }

    /// SELECT (или EXAMINE — только чтение). `qresync` — (UIDVALIDITY,
    /// MODSEQ) прошлого прохода: сервер сам пришлёт удалённое и изменившееся.
    public func select(_ rawName: String, readOnly: Bool = false, qresync: (UInt32, Int64)? = nil) async throws -> SelectedMailbox {
        var cmd = "\(readOnly ? "EXAMINE" : "SELECT") \(quote(rawName))"
        if let (v, m) = qresync, qresyncEnabled, v > 0, m > 0 {
            cmd += " (QRESYNC (\(v) \(m)))"
        } else if has("CONDSTORE") {
            cmd += " (CONDSTORE)"
        }
        selected = nil
        let r = try await run(cmd)
        var s = SelectedMailbox()
        for u in r.untagged {
            switch u {
            case let .exists(n): s.exists = n
            case let .status(.ok, code?, _):
                switch code.name {
                case "UIDVALIDITY": s.uidValidity = UInt32(clamping: code.number ?? 0)
                case "UIDNEXT": s.uidNext = UInt32(clamping: code.number ?? 0)
                case "HIGHESTMODSEQ": s.highestModSeq = code.number ?? 0
                case "NOMODSEQ": s.highestModSeq = 0
                default: break
                }
            case let .vanished(_, set): s.vanished.append(set)
            case let .fetch(_, f): s.changed.append(f)
            default: break
            }
        }
        selected = rawName
        return s
    }

    // MARK: Письма

    /// `UID FETCH`; `changedSince` — CONDSTORE (`CHANGEDSINCE`), с QRESYNC —
    /// ещё и `VANISHED` удалённых.
    public func uidFetch(_ set: String, _ items: String, changedSince: Int64? = nil) async throws -> IMAPResult {
        var cmd = "UID FETCH \(set) (\(items))"
        if let m = changedSince, m > 0, has("CONDSTORE") || qresyncEnabled {
            cmd += qresyncEnabled ? " (CHANGEDSINCE \(m) VANISHED)" : " (CHANGEDSINCE \(m))"
        }
        return try await run(cmd)
    }

    /// `FETCH` по номеру — только чтобы найти нижний UID окна.
    public func fetchUID(seq: Int) async throws -> UInt32? {
        let r = try await run("FETCH \(seq) (UID)")
        return r.fetches.first?.uid
    }

    public func uidSearch(_ criteria: String) async throws -> [UInt32] {
        try await run("UID SEARCH \(criteria)").searchUIDs
    }

    /// `UID SEARCH HEADER Message-ID <id>` — есть ли письмо в папке.
    public func searchMessageID(_ id: String) async throws -> [UInt32] {
        let r = try await run([.raw("UID SEARCH HEADER Message-ID "), .string("<\(normalizeMessageID(id))>")])
        return r.searchUIDs
    }

    public func uidStore(_ set: UIDSet, _ op: String, flags: [String]) async throws {
        for chunk in set.chunked() {
            _ = try await run("UID STORE \(chunk.string) \(op) (\(flags.joined(separator: " ")))")
        }
    }

    /// Удалить: `\Deleted` и `UID EXPUNGE` (UIDPLUS), иначе `EXPUNGE`.
    public func delete(_ set: UIDSet) async throws {
        try await uidStore(set, "+FLAGS.SILENT", flags: ["\\Deleted"])
        if has("UIDPLUS") {
            for chunk in set.chunked() { _ = try await run("UID EXPUNGE \(chunk.string)") }
        } else {
            _ = try await run("EXPUNGE")
        }
    }

    /// Перенести: `UID MOVE`, без него — `UID COPY` и удаление.
    public func move(_ set: UIDSet, to rawName: String) async throws {
        if has("MOVE") {
            for chunk in set.chunked() { _ = try await run("UID MOVE \(chunk.string) \(quote(rawName))") }
        } else {
            for chunk in set.chunked() { _ = try await run("UID COPY \(chunk.string) \(quote(rawName))") }
            try await delete(set)
        }
    }

    /// `APPEND` письма в папку с флагами.
    public func append(_ rawName: String, flags: [String], date: Date, message: Data) async throws {
        _ = try await run([
            .raw("APPEND \(quote(rawName)) (\(flags.joined(separator: " "))) \"\(MailDate.imapFormat(date))\" "),
            .literal(message),
        ])
    }

    public func noop() async throws { _ = try await run("NOOP") }

    // MARK: IDLE

    /// IDLE на выбранной папке до события или `timeout`. `true` — пришло
    /// событие (новое письмо, удаление, флаги), `false` — истёк срок. Срок
    /// отмеряет таймер: он шлёт DONE, пока чтение ждёт сервер, — соединение
    /// остаётся живым для следующего IDLE.
    public func idle(timeout: Duration) async throws -> Bool {
        let tag = nextTag()
        idleDone = false
        try await stream.write("\(tag) IDLE\r\n")
        var event = false
        var started = false
        let timer = Task { [weak self] in
            try? await Task.sleep(for: timeout)
            if !Task.isCancelled { try? await self?.finishIdle() }
        }
        defer { timer.cancel() }
        while true {
            let r = try IMAPResponse.parse(try await readIMAPResponse(stream, timeout: timeout + .seconds(120)))
            switch r {
            case .continuation:
                started = true
            case let .untagged(u):
                if case let .status(.bye, _, text) = u { throw MailNetError.server(text) }
                if isEvent(u) {
                    event = true
                    if started { try await finishIdle() }
                }
            case let .tagged(t, st, _, text):
                guard t == tag else { continue }
                guard st == .ok else { throw MailNetError.server("IDLE: \(text)") }
                return event
            }
            if event && started { try await finishIdle() }
        }
    }

    private var idleDone = false

    private func finishIdle() async throws {
        guard !idleDone else { return }
        idleDone = true
        try await stream.write("DONE\r\n")
    }

    private func isEvent(_ u: Untagged) -> Bool {
        switch u {
        case .exists, .expunge, .fetch, .vanished, .recent: true
        default: false
        }
    }

    // MARK: Команды

    public func run(_ command: String) async throws -> IMAPResult {
        try await run([.raw(command)])
    }

    /// Отправить команду (литералы — с ожиданием «+» или LITERAL+) и дождаться ответа.
    public func run(_ args: [IMAPArg]) async throws -> IMAPResult {
        let tag = nextTag()
        var pending = Data("\(tag) ".utf8)
        for a in args {
            switch a {
            case let .raw(s): pending += Data(s.utf8)
            case let .string(s):
                if let q = quotedIfSafe(s) {
                    pending += Data(q.utf8)
                } else {
                    try await sendLiteral(Data(s.utf8), before: &pending)
                }
            case let .literal(d):
                try await sendLiteral(d, before: &pending)
            }
        }
        pending += Data("\r\n".utf8)
        try await stream.write(pending)
        return try await waitTagged(tag)
    }

    private func sendLiteral(_ d: Data, before pending: inout Data) async throws {
        let plus = has("LITERAL+") || (has("LITERAL-") && d.count <= 4096)
        pending += Data("{\(d.count)\(plus ? "+" : "")}\r\n".utf8)
        try await stream.write(pending)
        pending = Data()
        if !plus {
            while true {
                let r = try IMAPResponse.parse(try await readIMAPResponse(stream))
                if case .continuation = r { break }
                if case let .tagged(_, _, _, text) = r { throw MailNetError.server(text) }
            }
        }
        try await stream.write(d)
    }

    private func waitTagged(_ tag: String) async throws -> IMAPResult {
        var untagged: [Untagged] = []
        while true {
            let r = try IMAPResponse.parse(try await readIMAPResponse(stream))
            switch r {
            case let .untagged(u):
                if case let .status(.bye, _, text) = u {
                    // BYE перед ответом на LOGOUT — норма; иначе сервер уходит.
                    untagged.append(u)
                    if !text.isEmpty && tag.isEmpty { throw MailNetError.server(text) }
                    continue
                }
                if case let .capability(c) = u { capabilities = Set(c) }
                untagged.append(u)
            case .continuation:
                continue
            case let .tagged(t, st, code, text):
                guard t == tag else { continue }
                absorb(code)
                switch st {
                case .ok: return IMAPResult(untagged: untagged, code: code, text: text)
                case .no where code?.name == "AUTHENTICATIONFAILED":
                    throw MailNetError.auth(text)
                default:
                    throw MailNetError.server(text.isEmpty ? st.rawValue : text)
                }
            }
        }
    }

    private func nextTag() -> String {
        tagN += 1
        return "D\(tagN)"
    }

    /// Строка в кавычках, если её можно так послать (7 бит, без CR/LF).
    func quotedIfSafe(_ s: String) -> String? {
        guard s.utf8.allSatisfy({ $0 >= 32 && $0 < 127 }) else { return nil }
        return "\"" + s.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"") + "\""
    }

    /// Имя папки для команды: в кавычках (имена — уже modified UTF-7).
    func quote(_ rawName: String) -> String {
        quotedIfSafe(rawName) ?? "\"\(rawName)\""
    }
}
