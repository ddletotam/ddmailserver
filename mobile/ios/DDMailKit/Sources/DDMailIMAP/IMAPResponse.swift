import DDMailMIME
import Foundation

public enum IMAPStatus: String, Sendable {
    case ok = "OK", no = "NO", bad = "BAD", bye = "BYE", preauth = "PREAUTH"
}

/// Код ответа в `[...]`: `UIDVALIDITY 1`, `HIGHESTMODSEQ 7`, `AUTHENTICATIONFAILED`.
public struct ResponseCode: Equatable, Sendable {
    public var name: String
    public var args: [String]

    init?(_ text: String) {
        let parts = text.split(separator: " ", omittingEmptySubsequences: true).map(String.init)
        guard let first = parts.first else { return nil }
        name = first.uppercased()
        args = Array(parts.dropFirst())
    }

    public var number: Int64? { args.first.flatMap { Int64($0) } }
}

/// Адрес ENVELOPE: `(имя adl ящик хост)`.
public struct EnvelopeAddress: Equatable, Sendable {
    public var name: String
    public var email: String
}

/// ENVELOPE письма (RFC 9051 §7.5.2). Тексты — сырые (RFC 2047 не раскодирован).
public struct Envelope: Equatable, Sendable {
    public var date: String
    public var subject: String
    public var from: [EnvelopeAddress]
    public var to: [EnvelopeAddress]
    public var cc: [EnvelopeAddress]
    public var inReplyTo: String
    public var messageID: String

    init(_ v: [IMAPValue]) {
        func s(_ i: Int) -> String { i < v.count ? (v[i].text ?? "") : "" }
        func addrs(_ i: Int) -> [EnvelopeAddress] {
            guard i < v.count, let list = v[i].list else { return [] }
            var out: [EnvelopeAddress] = []
            for a in list {
                guard let f = a.list, f.count >= 4 else { continue }
                let mailbox = f[2].text, host = f[3].text
                // Начало и конец группы (host NIL) — не адрес.
                guard let mailbox, let host else { continue }
                let email = "\(mailbox)@\(host)".replacingOccurrences(of: "<", with: "").replacingOccurrences(of: ">", with: "").lowercased()
                let name = f[0].text.map(EncodedWord.decode) ?? ""
                out.append(EnvelopeAddress(name: name, email: email.hasSuffix("@") ? String(email.dropLast()) : email))
            }
            return out
        }
        date = s(0)
        subject = s(1)
        from = addrs(2)
        to = addrs(5)
        cc = addrs(6)
        inReplyTo = s(8)
        messageID = s(9)
    }
}

/// Данные одного `FETCH`.
public struct FetchData: Equatable, Sendable {
    public var uid: UInt32?
    public var flags: [String]?
    public var envelope: Envelope?
    public var internalDate: String?
    public var modseq: Int64?
    public var size: Int64?
    /// Секции `BODY[...]` по спецификации в скобках в верхнем регистре
    /// (`""` — письмо целиком, `HEADER.FIELDS (FROM TO)` — выбранные заголовки).
    public var sections: [String: [UInt8]] = [:]

    public init() {}

    /// Целиком (`BODY[]`).
    public var body: [UInt8]? { sections[""] }
    /// Заголовки (`BODY[HEADER]` или `BODY[HEADER.FIELDS (...)]`).
    public var header: [UInt8]? { sections.first { $0.key.hasPrefix("HEADER") }?.value }

    public func has(flag: String) -> Bool { flags?.contains { $0.caseInsensitiveCompare(flag) == .orderedSame } ?? false }

    init(_ items: [IMAPValue]) throws {
        var i = 0
        while i + 1 < items.count {
            guard let key = items[i].atom?.uppercased() else { i += 1; continue }
            let v = items[i + 1]
            switch key {
            case "UID": uid = v.int.flatMap { UInt32(exactly: $0) }
            case "FLAGS": flags = v.list?.compactMap(\.atom) ?? []
            case "ENVELOPE": envelope = v.list.map(Envelope.init)
            case "INTERNALDATE": internalDate = v.text
            case "MODSEQ": modseq = v.list?.first?.int
            case "RFC822.SIZE": size = v.int
            default:
                if key.hasPrefix("BODY[") || key.hasPrefix("BODY.PEEK[") || key.hasPrefix("BINARY[") {
                    let open = key.firstIndex(of: "[")!
                    let close = key.lastIndex(of: "]") ?? key.endIndex
                    let spec = String(key[key.index(after: open)..<close])
                    sections[spec] = v.bytes ?? []
                } else if key == "RFC822" {
                    sections[""] = v.bytes ?? []
                } else if key == "RFC822.HEADER" {
                    sections["HEADER"] = v.bytes ?? []
                }
            }
            i += 2
        }
    }
}

/// Строка `LIST`.
public struct ListEntry: Equatable, Sendable {
    public var attributes: [String]
    public var delimiter: String?
    /// Имя, как его прислал сервер (modified UTF-7) — для команд.
    public var rawName: String
    /// Имя для людей.
    public var name: String { ModifiedUTF7.decode(rawName) }

    public func has(_ attr: String) -> Bool { attributes.contains { $0.caseInsensitiveCompare(attr) == .orderedSame } }
}

public enum Untagged: Equatable, Sendable {
    case status(IMAPStatus, code: ResponseCode?, text: String)
    case capability([String])
    case list(ListEntry)
    case exists(Int)
    case recent(Int)
    case expunge(Int)
    case fetch(seq: Int, FetchData)
    case search([UInt32])
    case flags([String])
    case vanished(earlier: Bool, UIDSet)
    case enabled([String])
    case other(String)
}

public enum IMAPResponse: Equatable, Sendable {
    case tagged(tag: String, status: IMAPStatus, code: ResponseCode?, text: String)
    case untagged(Untagged)
    case continuation(String)

    /// Разобрать собранный ответ (литералы на месте).
    public static func parse(_ bytes: [UInt8]) throws -> IMAPResponse {
        var t = IMAPTokenizer(bytes)
        let first = t.word()
        if first == "+" {
            t.skipSpaces()
            return .continuation(t.rest())
        }
        if first == "*" { return .untagged(try parseUntagged(&t)) }
        let statusWord = t.word().uppercased()
        guard let st = IMAPStatus(rawValue: statusWord) else {
            throw IMAPError.parse("ответ без статуса: \(String(decoding: bytes.prefix(80), as: UTF8.self))")
        }
        let (code, text) = statusText(&t)
        return .tagged(tag: first, status: st, code: code, text: text)
    }

    static func statusText(_ t: inout IMAPTokenizer) -> (ResponseCode?, String) {
        t.skipSpaces()
        var text = t.rest()
        var code: ResponseCode?
        if text.hasPrefix("["), let close = text.firstIndex(of: "]") {
            code = ResponseCode(String(text[text.index(after: text.startIndex)..<close]))
            text = String(text[text.index(after: close)...]).trimmingCharacters(in: .whitespaces)
        }
        return (code, text)
    }

    static func parseUntagged(_ t: inout IMAPTokenizer) throws -> Untagged {
        let w = t.word()
        if let n = Int(w) {
            let kind = t.word().uppercased()
            switch kind {
            case "EXISTS": return .exists(n)
            case "RECENT": return .recent(n)
            case "EXPUNGE": return .expunge(n)
            case "FETCH":
                guard case let .list(items) = try t.value() else { throw IMAPError.parse("FETCH без списка") }
                return .fetch(seq: n, try FetchData(items))
            default: return .other("\(n) \(kind)")
            }
        }
        let kind = w.uppercased()
        if let st = IMAPStatus(rawValue: kind) {
            let (code, text) = statusText(&t)
            return .status(st, code: code, text: text)
        }
        switch kind {
        case "CAPABILITY":
            return .capability(t.rest().split(separator: " ").map { $0.uppercased() })
        case "ENABLED":
            return .enabled(t.rest().split(separator: " ").map { $0.uppercased() })
        case "LIST", "LSUB", "XLIST":
            let attrs = (try t.value()).list?.compactMap(\.atom) ?? []
            let delim = try t.value()
            let name = try t.value()
            return .list(ListEntry(attributes: attrs, delimiter: delim.text, rawName: name.text ?? ""))
        case "SEARCH":
            var uids: [UInt32] = []
            while !t.atEnd {
                let v = try t.value()
                if let a = v.atom, let u = UInt32(a) { uids.append(u) }
                // `(MODSEQ n)` в конце CONDSTORE-поиска пропускаем.
            }
            return .search(uids)
        case "FLAGS":
            return .flags((try t.value()).list?.compactMap(\.atom) ?? [])
        case "VANISHED":
            t.skipSpaces()
            var earlier = false
            var v = try t.value()
            if let l = v.list, l.contains(where: { $0.atom?.uppercased() == "EARLIER" }) {
                earlier = true
                v = try t.value()
            }
            guard let s = v.atom, let set = UIDSet(parse: s) else { throw IMAPError.parse("битый VANISHED") }
            return .vanished(earlier: earlier, set)
        default:
            return .other("\(kind) \(t.rest())")
        }
    }
}

/// Сборка ответа целиком: строка, и если она кончается `{n}` — литерал из
/// n байт и продолжение строки, и так далее.
public func readIMAPResponse(_ s: LineStreamReading, timeout: Duration? = nil) async throws -> [UInt8] {
    var out: [UInt8] = []
    while true {
        let line = try await s.readLine(timeout: timeout)
        out += line
        guard let n = literalLength(line) else { return out }
        out += [13, 10]
        out += try await s.readBytes(n, timeout: timeout)
    }
}

/// `...{123}` или `{123+}` в конце строки → 123.
func literalLength(_ line: [UInt8]) -> Int? {
    guard line.last == 125, let open = line.lastIndex(of: 123) else { return nil }
    var digits = String(decoding: line[(open + 1)..<(line.count - 1)], as: UTF8.self)
    if digits.hasSuffix("+") || digits.hasSuffix("-") { digits.removeLast() }
    return Int(digits)
}

/// Что нужно от потока для чтения ответов (в тестах — записанная сессия).
public protocol LineStreamReading: AnyObject, Sendable {
    func readLine(timeout: Duration?) async throws -> [UInt8]
    func readBytes(_ n: Int, timeout: Duration?) async throws -> [UInt8]
}
