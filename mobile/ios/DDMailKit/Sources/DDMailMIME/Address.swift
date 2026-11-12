import Foundation

/// Адрес из заголовка: имя (RFC 2047 раскодировано) и адрес в нижнем регистре.
public struct MailAddress: Equatable, Hashable, Sendable {
    public var name: String
    public var email: String

    public init(name: String = "", email: String) {
        self.name = name
        self.email = email
    }

    /// `Имя <адрес>` или голый адрес — для полей To/Cc тела письма.
    public var display: String {
        name.isEmpty ? email : "\(name) <\(email)>"
    }

    /// Разобрать список адресов (`To`, `Cc`, `From`): кавычки, комментарии,
    /// группы (`Друзья: a@x, b@y;`), имя без кавычек с запятой внутри
    /// слова RFC 2047. Пустые элементы пропускаются.
    public static func parseList(_ value: String) -> [MailAddress] {
        var out: [MailAddress] = []
        for item in splitTopLevel(value) {
            if let a = parseOne(item) { out.append(a) }
        }
        return out
    }

    /// Делим по запятым и `;`/`:` групп вне кавычек, `<>` и скобок.
    static func splitTopLevel(_ s: String) -> [String] {
        var parts: [String] = []
        var cur = ""
        var quoted = false, angle = 0, paren = 0
        var escape = false
        for ch in s {
            if escape { cur.append(ch); escape = false; continue }
            if ch == "\\" && (quoted || paren > 0) { cur.append(ch); escape = true; continue }
            switch ch {
            case "\"" where paren == 0: quoted.toggle(); cur.append(ch)
            case "<" where !quoted && paren == 0: angle += 1; cur.append(ch)
            case ">" where !quoted && paren == 0: angle = max(0, angle - 1); cur.append(ch)
            case "(" where !quoted: paren += 1; cur.append(ch)
            case ")" where !quoted: paren = max(0, paren - 1); cur.append(ch)
            case "," where !quoted && angle == 0 && paren == 0,
                 ";" where !quoted && angle == 0 && paren == 0:
                parts.append(cur); cur = ""
            case ":" where !quoted && angle == 0 && paren == 0 && !cur.contains("@"):
                // Имя группы — выбрасываем.
                cur = ""
            default: cur.append(ch)
            }
        }
        parts.append(cur)
        return parts.map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }.filter { !$0.isEmpty }
    }

    static func parseOne(_ item: String) -> MailAddress? {
        if let lt = item.lastIndex(of: "<"), let gt = item[lt...].firstIndex(of: ">") {
            let addr = item[item.index(after: lt)..<gt].trimmingCharacters(in: .whitespaces).lowercased()
            var name = String(item[..<lt]).trimmingCharacters(in: .whitespaces)
            name = unquote(stripComments(name))
            name = EncodedWord.decode(name).trimmingCharacters(in: .whitespaces)
            let email = addr.hasPrefix("mailto:") ? String(addr.dropFirst(7)) : addr
            guard !email.isEmpty else { return nil }
            return MailAddress(name: name, email: email)
        }
        // Голый адрес, возможно с комментарием-именем: `a@x (Имя)`.
        var comment = ""
        if let open = item.firstIndex(of: "("), let close = item.lastIndex(of: ")"), open < close {
            comment = String(item[item.index(after: open)..<close])
        }
        let bare = stripComments(item).trimmingCharacters(in: .whitespaces).lowercased()
        guard !bare.isEmpty else { return nil }
        let name = EncodedWord.decode(comment).trimmingCharacters(in: .whitespaces)
        return MailAddress(name: name.contains("@") ? "" : name, email: unquote(bare))
    }

    static func stripComments(_ s: String) -> String {
        var out = ""
        var depth = 0
        var quoted = false
        for ch in s {
            if ch == "\"" && depth == 0 { quoted.toggle() }
            if !quoted && ch == "(" { depth += 1; continue }
            if !quoted && ch == ")" && depth > 0 { depth -= 1; continue }
            if depth == 0 { out.append(ch) }
        }
        return out
    }

    static func unquote(_ s: String) -> String {
        var t = s.trimmingCharacters(in: .whitespaces)
        if t.count >= 2, t.hasPrefix("\""), t.hasSuffix("\"") {
            t = String(t.dropFirst().dropLast())
            t = t.replacingOccurrences(of: "\\\"", with: "\"").replacingOccurrences(of: "\\\\", with: "\\")
        }
        return t
    }
}

/// Имя без адресной части (порт `clean_display_name` десктопа):
/// «John <j@x>» → «John», «j@x» → «».
public func cleanDisplayName(_ s: String) -> String {
    var r = s
    if let i = r.firstIndex(of: "<") { r = String(r[..<i]) }
    if let open = r.firstIndex(of: "("), let close = r.firstIndex(of: ")"), open < close, r[open..<close].contains("@") {
        r = String(r[..<open])
    }
    let t = r.trimmingCharacters(in: .whitespaces)
    return t.contains("@") ? "" : t
}
