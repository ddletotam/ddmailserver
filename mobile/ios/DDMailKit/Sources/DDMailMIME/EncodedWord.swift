import Foundation

/// Слова RFC 2047 в заголовках (`=?utf-8?B?...?=`, `=?koi8-r?Q?...?=`).
public enum EncodedWord {
    /// Раскодировать значение заголовка. Пробелы между соседними словами
    /// выбрасываются (RFC 2047 §6.2); соседние слова одной кодировки
    /// склеиваются по байтам до перевода в текст — почтовики режут
    /// многобайтовый символ между словами.
    public static func decode(_ value: String) -> String {
        guard value.contains("=?") else { return value }
        var out = ""
        var pending: (charset: String, bytes: [UInt8])?
        var gap = ""
        var rest = Substring(value)

        func flush() {
            if let p = pending { out += Charset.decode(p.bytes, charset: p.charset) }
            pending = nil
        }

        while let start = rest.range(of: "=?") {
            let before = rest[rest.startIndex..<start.lowerBound]
            guard let word = parseWord(rest[start.lowerBound...]) else {
                // Не слово — обычный текст до «=?» включительно.
                flush()
                out += gap + before + "=?"
                gap = ""
                rest = rest[start.upperBound...]
                continue
            }
            let onlySpace = before.allSatisfy(\.isWhitespace)
            if pending != nil && onlySpace {
                // Между словами — только пробел: он не часть текста.
            } else {
                flush()
                out += gap + before
            }
            gap = ""
            if let p = pending, Charset.normalize(p.charset) == Charset.normalize(word.charset) {
                pending = (p.charset, p.bytes + word.bytes)
            } else {
                flush()
                pending = (word.charset, word.bytes)
            }
            rest = rest[word.end...]
        }
        flush()
        out += gap + rest
        return out
    }

    private struct Word {
        let charset: String
        let bytes: [UInt8]
        let end: Substring.Index
    }

    /// `=?charset[*lang]?B|Q?text?=` с начала `s`.
    private static func parseWord(_ s: Substring) -> Word? {
        let body = s.dropFirst(2)
        guard let q1 = body.firstIndex(of: "?") else { return nil }
        var charset = String(body[body.startIndex..<q1])
        if let star = charset.firstIndex(of: "*") { charset = String(charset[..<star]) } // RFC 2231 язык
        let afterCharset = body[body.index(after: q1)...]
        guard let enc = afterCharset.first, afterCharset.dropFirst().first == "?" else { return nil }
        let text = afterCharset.dropFirst(2)
        guard let end = text.range(of: "?=") else { return nil }
        let payload = text[text.startIndex..<end.lowerBound]
        // Слово не может содержать пробелов; иначе «?=» нашёлся чужой.
        guard !payload.contains(" "), !charset.isEmpty else { return nil }
        let raw = Array(payload.utf8)
        let bytes: [UInt8]
        switch enc {
        case "B", "b": bytes = TransferEncoding.base64Decode(raw)
        case "Q", "q": bytes = TransferEncoding.quotedPrintableDecode(raw, header: true)
        default: return nil
        }
        return Word(charset: charset, bytes: bytes, end: end.upperBound)
    }

    /// Закодировать значение для заголовка исходящего письма: ASCII — как
    /// есть, иначе слова `=?UTF-8?B?…?=` не длиннее 75 символов, без разрыва
    /// символа между словами.
    public static func encode(_ value: String) -> String {
        if value.unicodeScalars.allSatisfy({ $0.isASCII && $0.value >= 32 && $0.value != 127 }) { return value }
        var words: [String] = []
        var chunk = ""
        // 75 − len("=?UTF-8?B??=") = 63 символа base64 → 45 байт текста.
        for ch in value {
            let next = chunk + String(ch)
            if next.utf8.count > 45 {
                words.append(chunk)
                chunk = String(ch)
            } else {
                chunk = next
            }
        }
        if !chunk.isEmpty { words.append(chunk) }
        return words.map { "=?UTF-8?B?\(Data($0.utf8).base64EncodedString())?=" }.joined(separator: "\r\n ")
    }
}
