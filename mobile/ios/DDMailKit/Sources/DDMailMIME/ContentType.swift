import Foundation

/// Значение с параметрами: `Content-Type: text/plain; charset=koi8-r`,
/// `Content-Disposition: attachment; filename*=UTF-8''%D0%BE...`.
/// Параметры — с RFC 2231 (продолжения `*0*`, `*1`, кодировка и язык) и
/// RFC 2047 в кавычках (так пишут имена файлов Outlook и Mail.ru).
public struct ParameterizedValue: Equatable, Sendable {
    /// Основное значение в нижнем регистре (`text/plain`, `attachment`).
    public var value: String
    /// Имена параметров — в нижнем регистре, значения — раскодированы.
    public var params: [String: String]

    public init(value: String, params: [String: String] = [:]) {
        self.value = value
        self.params = params
    }

    public init(_ raw: String) {
        let parts = Self.splitParams(raw)
        value = (parts.first ?? "").trimmingCharacters(in: .whitespaces).lowercased()
        var simple: [String: String] = [:]
        // name → [(номер, закодирован ли, значение)]
        var pieces: [String: [(Int, Bool, String)]] = [:]
        for p in parts.dropFirst() {
            guard let eq = p.firstIndex(of: "=") else { continue }
            var name = p[..<eq].trimmingCharacters(in: .whitespaces).lowercased()
            var v = p[p.index(after: eq)...].trimmingCharacters(in: .whitespaces)
            v = MailAddress.unquote(v)
            var encoded = false
            if name.hasSuffix("*") { encoded = true; name.removeLast() }
            if let star = name.lastIndex(of: "*"), let n = Int(name[name.index(after: star)...]) {
                pieces[String(name[..<star]), default: []].append((n, encoded, v))
            } else if encoded {
                pieces[name, default: []].append((0, true, v))
            } else {
                simple[name] = v
            }
        }
        for (name, list) in pieces {
            let sorted = list.sorted { $0.0 < $1.0 }
            var charset: String?
            var bytes: [UInt8] = []
            for (i, piece) in sorted.enumerated() {
                var v = piece.2
                if piece.1 && i == 0 {
                    // charset'language'значение
                    let comps = v.split(separator: "'", maxSplits: 2, omittingEmptySubsequences: false)
                    if comps.count == 3 {
                        charset = String(comps[0])
                        v = String(comps[2])
                    }
                }
                bytes += piece.1 ? Self.percentDecode(v) : Array(v.utf8)
            }
            simple[name] = Charset.decode(bytes, charset: charset ?? "utf-8")
        }
        params = simple.mapValues { EncodedWord.decode($0) }
    }

    public subscript(_ name: String) -> String? { params[name.lowercased()] }

    static func percentDecode(_ s: String) -> [UInt8] {
        let a = Array(s.utf8)
        var out: [UInt8] = []
        var i = 0
        while i < a.count {
            if a[i] == 37, i + 2 < a.count, let v = UInt8(String(bytes: a[(i + 1)...(i + 2)], encoding: .ascii) ?? "", radix: 16) {
                out.append(v)
                i += 3
            } else {
                out.append(a[i])
                i += 1
            }
        }
        return out
    }

    /// Делим по `;` вне кавычек.
    static func splitParams(_ s: String) -> [String] {
        var parts: [String] = []
        var cur = ""
        var quoted = false
        var escape = false
        for ch in s {
            if escape { cur.append(ch); escape = false; continue }
            if ch == "\\" && quoted { cur.append(ch); escape = true; continue }
            if ch == "\"" { quoted.toggle() }
            if ch == ";" && !quoted {
                parts.append(cur)
                cur = ""
            } else {
                cur.append(ch)
            }
        }
        parts.append(cur)
        return parts
    }
}
