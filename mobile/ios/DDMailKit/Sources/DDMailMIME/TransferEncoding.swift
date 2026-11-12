import Foundation

/// Content-Transfer-Encoding и кодировки слов RFC 2047: терпимо к мусору —
/// реальная почта нарушает стандарт чаще, чем соблюдает.
public enum TransferEncoding {
    /// base64 без требований к переносам и паддингу; посторонние символы пропускаются.
    public static func base64Decode(_ bytes: some Collection<UInt8>) -> [UInt8] {
        var out: [UInt8] = []
        out.reserveCapacity(bytes.count * 3 / 4)
        var acc: UInt32 = 0
        var bits = 0
        for b in bytes {
            let v: UInt32
            switch b {
            case 65...90: v = UInt32(b - 65)
            case 97...122: v = UInt32(b - 97 + 26)
            case 48...57: v = UInt32(b - 48 + 52)
            case 43, 45: v = 62 // '+' и '-' (url-safe)
            case 47, 95: v = 63 // '/' и '_'
            case 61: // '=' — конец данных
                return out
            default: continue
            }
            acc = (acc << 6) | v
            bits += 6
            if bits >= 8 {
                bits -= 8
                out.append(UInt8((acc >> UInt32(bits)) & 0xFF))
            }
        }
        return out
    }

    /// quoted-printable тела; `header` — вариант «Q» RFC 2047 (`_` — пробел).
    public static func quotedPrintableDecode(_ bytes: some Collection<UInt8>, header: Bool = false) -> [UInt8] {
        let a = Array(bytes)
        var out: [UInt8] = []
        out.reserveCapacity(a.count)
        var i = 0
        while i < a.count {
            let b = a[i]
            if b == 61 { // '='
                // Мягкий перенос: "=\r\n" или "=\n" (и пробелы перед переносом).
                var j = i + 1
                while j < a.count, a[j] == 32 || a[j] == 9 { j += 1 }
                if j < a.count, a[j] == 13 || a[j] == 10 {
                    if a[j] == 13, j + 1 < a.count, a[j + 1] == 10 { j += 1 }
                    i = j + 1
                    continue
                }
                if j >= a.count { i = j; continue }
                if i + 2 < a.count, let h = hex(a[i + 1]), let l = hex(a[i + 2]) {
                    out.append(h << 4 | l)
                    i += 3
                    continue
                }
                out.append(b)
                i += 1
            } else if header && b == 95 {
                out.append(32)
                i += 1
            } else {
                out.append(b)
                i += 1
            }
        }
        return out
    }

    private static func hex(_ c: UInt8) -> UInt8? {
        switch c {
        case 48...57: return c - 48
        case 65...70: return c - 55
        case 97...102: return c - 87
        default: return nil
        }
    }

    /// Тело части по её Content-Transfer-Encoding.
    public static func decode(_ bytes: some Collection<UInt8>, encoding: String?) -> [UInt8] {
        switch (encoding ?? "").trimmingCharacters(in: .whitespaces).lowercased() {
        case "base64": return base64Decode(bytes)
        case "quoted-printable": return quotedPrintableDecode(bytes)
        default: return Array(bytes)
        }
    }

    /// base64 по 76 символов в строке (RFC 2045) — для исходящих.
    public static func base64Lines(_ data: Data) -> String {
        let s = data.base64EncodedString()
        var out = ""
        out.reserveCapacity(s.count + s.count / 76 * 2 + 2)
        var i = s.startIndex
        while i < s.endIndex {
            let j = s.index(i, offsetBy: 76, limitedBy: s.endIndex) ?? s.endIndex
            out += s[i..<j]
            out += "\r\n"
            i = j
        }
        return out
    }

    /// quoted-printable для исходящего текста (UTF-8): строки ≤ 76, мягкие переносы.
    public static func quotedPrintableEncode(_ text: String) -> String {
        var out = ""
        let lines = text.replacingOccurrences(of: "\r\n", with: "\n").split(separator: "\n", omittingEmptySubsequences: false)
        for (n, line) in lines.enumerated() {
            var cur = ""
            let bytes = Array(line.utf8)
            for (k, b) in bytes.enumerated() {
                let last = k == bytes.count - 1
                var piece: String
                if (b == 32 || b == 9) && last {
                    piece = String(format: "=%02X", b)
                } else if (b >= 33 && b <= 126 && b != 61) || b == 32 || b == 9 {
                    piece = String(UnicodeScalar(b))
                } else {
                    piece = String(format: "=%02X", b)
                }
                // '.' в начале строки — экранируем, чтобы SMTP-точка не сломала тело.
                if cur.isEmpty && b == 46 { piece = "=2E" }
                if cur.utf8.count + piece.utf8.count > 75 {
                    out += cur + "=\r\n"
                    cur = ""
                }
                cur += piece
            }
            out += cur
            if n < lines.count - 1 { out += "\r\n" }
        }
        return out
    }
}
