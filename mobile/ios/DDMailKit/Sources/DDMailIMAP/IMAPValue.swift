import Foundation

/// Значение ответа IMAP (RFC 9051 §4): атом (числа, флаги, `BODY[...]`),
/// строка (в кавычках или литерал — байты как есть), `NIL`, список.
public indirect enum IMAPValue: Equatable, Sendable {
    case atom(String)
    case string([UInt8])
    case nil_
    case list([IMAPValue])

    public var atom: String? { if case let .atom(a) = self { a } else { nil } }
    public var list: [IMAPValue]? { if case let .list(l) = self { l } else { nil } }

    /// Байты строки (атом — тоже строка: некоторые серверы так шлют).
    public var bytes: [UInt8]? {
        switch self {
        case let .string(b): b
        case let .atom(a): Array(a.utf8)
        default: nil
        }
    }

    /// Строка как текст (UTF-8, иначе windows-1251 — восьмибитные заголовки).
    public var text: String? {
        bytes.map { b in String(data: Data(b), encoding: .utf8) ?? String(data: Data(b), encoding: .windowsCP1251) ?? String(decoding: b, as: UTF8.self) }
    }

    public var int: Int64? { atom.flatMap { Int64($0) } }
}

/// Разбор значений из байтов ответа, где литералы уже на месте:
/// `{n}\r\n` и следом ровно n байт.
struct IMAPTokenizer {
    let b: [UInt8]
    var i: Int

    init(_ bytes: [UInt8], at index: Int = 0) {
        b = bytes
        i = index
    }

    var atEnd: Bool { i >= b.count }

    mutating func skipSpaces() {
        while i < b.count, b[i] == 32 { i += 1 }
    }

    /// Остаток как текст (текст статуса).
    mutating func rest() -> String {
        defer { i = b.count }
        return i < b.count ? String(decoding: b[i...], as: UTF8.self) : ""
    }

    /// Следующее слово до пробела (тег, ключевое слово ответа).
    mutating func word() -> String {
        skipSpaces()
        let s = i
        while i < b.count, b[i] != 32 { i += 1 }
        return String(decoding: b[s..<i], as: UTF8.self)
    }

    mutating func value() throws -> IMAPValue {
        skipSpaces()
        guard i < b.count else { throw IMAPError.parse("неожиданный конец ответа") }
        switch b[i] {
        case 40: // (
            i += 1
            var items: [IMAPValue] = []
            while true {
                skipSpaces()
                guard i < b.count else { throw IMAPError.parse("незакрытый список") }
                if b[i] == 41 { i += 1; return .list(items) } // )
                items.append(try value())
            }
        case 34: // "
            i += 1
            var out: [UInt8] = []
            while i < b.count {
                let c = b[i]
                if c == 92, i + 1 < b.count { out.append(b[i + 1]); i += 2; continue }
                if c == 34 { i += 1; return .string(out) }
                out.append(c)
                i += 1
            }
            throw IMAPError.parse("незакрытая строка")
        case 123: // {
            guard let close = b[i...].firstIndex(of: 125) else { throw IMAPError.parse("битый литерал") }
            var digits = String(decoding: b[(i + 1)..<close], as: UTF8.self)
            if digits.hasSuffix("+") || digits.hasSuffix("-") { digits.removeLast() }
            if digits.hasPrefix("~") { digits.removeFirst() }
            guard let n = Int(digits) else { throw IMAPError.parse("битый литерал") }
            var j = close + 1
            if j < b.count, b[j] == 13 { j += 1 }
            if j < b.count, b[j] == 10 { j += 1 }
            guard j + n <= b.count else { throw IMAPError.parse("литерал короче объявленного") }
            i = j + n
            return .string(Array(b[j..<(j + n)]))
        default:
            let s = i
            var depth = 0
            while i < b.count {
                let c = b[i]
                if c == 91 { depth += 1 } // [
                else if c == 93 { depth = max(0, depth - 1) } // ]
                else if depth == 0 && (c == 32 || c == 40 || c == 41 || c == 13 || c == 10) { break }
                i += 1
            }
            let a = String(decoding: b[s..<i], as: UTF8.self)
            return a.caseInsensitiveCompare("NIL") == .orderedSame ? .nil_ : .atom(a)
        }
    }
}

public enum IMAPError: Error, Equatable, Sendable {
    case parse(String)
}

// MARK: - Наборы UID

/// Набор UID (`1:5,7,9:12`): для команд и разбора `VANISHED`/`SEARCH`.
public struct UIDSet: Equatable, Sendable {
    public private(set) var ranges: [ClosedRange<UInt32>] = []

    public init(_ uids: some Sequence<UInt32>) {
        let sorted = Set(uids).sorted()
        var out: [ClosedRange<UInt32>] = []
        for u in sorted {
            if let last = out.last, last.upperBound &+ 1 == u {
                out[out.count - 1] = last.lowerBound...u
            } else {
                out.append(u...u)
            }
        }
        ranges = out
    }

    init(ranges: [ClosedRange<UInt32>]) { self.ranges = ranges }

    /// `1:5,7`; `*` не поддерживается (разбор только ответов сервера).
    public init?(parse s: String) {
        var out: [ClosedRange<UInt32>] = []
        for part in s.split(separator: ",") {
            let ends = part.split(separator: ":")
            if ends.count == 1, let a = UInt32(ends[0]) {
                out.append(a...a)
            } else if ends.count == 2, let a = UInt32(ends[0]), let b = UInt32(ends[1]) {
                out.append(min(a, b)...max(a, b))
            } else {
                return nil
            }
        }
        ranges = out
    }

    public var isEmpty: Bool { ranges.isEmpty }
    public var count: Int { ranges.reduce(0) { $0 + Int($1.count) } }

    public var string: String {
        ranges.map { $0.lowerBound == $0.upperBound ? "\($0.lowerBound)" : "\($0.lowerBound):\($0.upperBound)" }
            .joined(separator: ",")
    }

    public func contains(_ u: UInt32) -> Bool { ranges.contains { $0.contains(u) } }

    /// Все UID (для небольших наборов).
    public var all: [UInt32] { ranges.flatMap { Array($0) } }

    /// Куски не длиннее `max` элементов в строке команды — длинные строки
    /// некоторые серверы режут.
    public func chunked(_ maxRanges: Int = 400) -> [UIDSet] {
        stride(from: 0, to: ranges.count, by: maxRanges).map { UIDSet(ranges: Array(ranges[$0..<min($0 + maxRanges, ranges.count)])) }
    }
}

// MARK: - modified UTF-7 (RFC 3501 §5.1.3)

public enum ModifiedUTF7 {
    /// Имя папки с сервера → текст: `&BB4EQgQ,BEAEMAQyBDsENQQ9BD0ESwQ1-` → «Отправленные».
    public static func decode(_ s: String) -> String {
        var out = ""
        var chars = Substring(s)
        while let amp = chars.firstIndex(of: "&") {
            out += chars[chars.startIndex..<amp]
            let after = chars.index(after: amp)
            guard let dash = chars[after...].firstIndex(of: "-") else {
                out += chars[amp...]
                return out
            }
            let enc = chars[after..<dash]
            if enc.isEmpty {
                out += "&"
            } else {
                var b64 = enc.replacingOccurrences(of: ",", with: "/")
                while b64.count % 4 != 0 { b64 += "=" }
                if let data = Data(base64Encoded: b64), let str = String(data: data, encoding: .utf16BigEndian) {
                    out += str
                } else {
                    out += chars[amp...dash]
                }
            }
            chars = chars[chars.index(after: dash)...]
        }
        out += chars
        return out
    }

    /// Текст → имя для команды.
    public static func encode(_ s: String) -> String {
        var out = ""
        var pending: [UInt16] = []
        func flush() {
            guard !pending.isEmpty else { return }
            var bytes: [UInt8] = []
            for u in pending { bytes.append(UInt8(u >> 8)); bytes.append(UInt8(u & 0xFF)) }
            let b64 = Data(bytes).base64EncodedString().replacingOccurrences(of: "=", with: "").replacingOccurrences(of: "/", with: ",")
            out += "&" + b64 + "-"
            pending.removeAll()
        }
        for scalar in s.unicodeScalars {
            if scalar.value >= 0x20 && scalar.value <= 0x7E {
                flush()
                out += scalar == "&" ? "&-" : String(scalar)
            } else {
                pending += Array(String(scalar).utf16)
            }
        }
        flush()
        return out
    }
}
