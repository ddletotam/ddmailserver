import Foundation

/// Блок заголовков письма или части: поля по порядку, с развёрнутыми
/// переносами (RFC 5322 §2.2.3). Значения — как в письме, слова RFC 2047 не
/// раскодированы: адреса и параметры разбираются до раскодирования.
public struct HeaderBlock: Equatable, Sendable {
    public struct Field: Equatable, Sendable {
        public let name: String
        public let value: String
    }

    public private(set) var fields: [Field] = []

    public init(fields: [Field] = []) { self.fields = fields }

    /// Разобрать байты заголовка (до пустой строки). Восьмибитные байты в
    /// заголовках (нарушение, но частое) — UTF-8, иначе windows-1251.
    public init(_ bytes: some Collection<UInt8>) {
        // Строки режем по байтам: в `String` пара CR LF — один символ, и
        // разбиение по "\n" её не видит.
        var current: (String, String)?
        for rawLine in Array(bytes).split(separator: 10, omittingEmptySubsequences: false) {
            var lineBytes = rawLine
            if lineBytes.last == 13 { lineBytes = lineBytes.dropLast() }
            if lineBytes.isEmpty { break }
            let line = Charset.guess(Data(lineBytes))
            if let first = lineBytes.first, first == 32 || first == 9 {
                // Продолжение: перенос с пробелом — это пробел.
                if let c = current { current = (c.0, c.1 + " " + line.trimmingCharacters(in: .whitespaces)) }
                continue
            }
            if let c = current { fields.append(Field(name: c.0, value: c.1)) }
            if let colon = line.firstIndex(of: ":") {
                let name = line[..<colon].trimmingCharacters(in: .whitespaces)
                let value = line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)
                current = (name, value)
            } else {
                current = nil
            }
        }
        if let c = current { fields.append(Field(name: c.0, value: c.1)) }
    }

    /// Первое значение поля (имя без учёта регистра).
    public subscript(_ name: String) -> String? {
        fields.first { $0.name.caseInsensitiveCompare(name) == .orderedSame }?.value
    }

    public func all(_ name: String) -> [String] {
        fields.filter { $0.name.caseInsensitiveCompare(name) == .orderedSame }.map(\.value)
    }

    /// Значение с раскодированными словами RFC 2047.
    public func text(_ name: String) -> String? { self[name].map(EncodedWord.decode) }
}

/// Где кончается заголовок в сыром письме: индекс пустой строки и начало тела.
public func splitHeaderBody(_ raw: [UInt8]) -> (header: ArraySlice<UInt8>, body: ArraySlice<UInt8>) {
    var i = 0
    while i < raw.count {
        if raw[i] == 10 {
            // "\n\n" или "\n\r\n"
            if i + 1 < raw.count, raw[i + 1] == 10 { return (raw[0..<i], raw[(i + 2)...]) }
            if i + 2 < raw.count, raw[i + 1] == 13, raw[i + 2] == 10 { return (raw[0..<i], raw[(i + 3)...]) }
        }
        i += 1
    }
    return (raw[0...], [])
}

/// Message-ID без `<>` и пробелов.
public func normalizeMessageID(_ s: String) -> String {
    var t = s.trimmingCharacters(in: .whitespacesAndNewlines)
    if t.hasPrefix("<") { t.removeFirst() }
    if t.hasSuffix(">") { t.removeLast() }
    return t.trimmingCharacters(in: .whitespaces)
}

/// Список Message-ID из `References` (порт `extract_msg_ids` десктопа).
public func extractMessageIDs(_ value: String) -> [String] {
    var out: [String] = []
    var cur = ""
    var depth = 0
    for ch in value {
        switch ch {
        case "<":
            depth += 1
            cur = ""
        case ">":
            if depth > 0 {
                depth -= 1
                let id = cur.trimmingCharacters(in: .whitespaces)
                if !id.isEmpty { out.append(id) }
                cur = ""
            }
        default:
            if depth > 0 { cur.append(ch) }
        }
    }
    return out
}
