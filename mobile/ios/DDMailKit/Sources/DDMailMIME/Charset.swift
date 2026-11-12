import Foundation

/// Кодировки текста писем (контракт внешних учёток §3): всё, что встречается
/// в RU-почте — UTF-8, windows-1251, koi8-r/u, iso-8859-*, cp866, а также
/// прочие, которые знает система (`CFStringConvertIANACharSetNameToEncoding`).
public enum Charset {
    /// Байты → строка по имени кодировки из заголовка. Неизвестная кодировка
    /// или битые байты не роняют письмо: UTF-8 с заменой, затем windows-1251
    /// (самая частая «голая» кодировка RU-почты).
    public static func decode(_ bytes: some Collection<UInt8>, charset: String?) -> String {
        let data = Data(bytes)
        if let name = charset.map(normalize), !name.isEmpty, let enc = encoding(name) {
            if let s = String(data: data, encoding: enc) { return s }
        }
        return guess(data)
    }

    /// Без объявленной кодировки: ASCII/UTF-8, иначе windows-1251.
    public static func guess(_ data: Data) -> String {
        if let s = String(data: data, encoding: .utf8) { return s }
        if let s = String(data: data, encoding: .windowsCP1251) { return s }
        return String(decoding: data, as: UTF8.self)
    }

    static func normalize(_ s: String) -> String {
        s.trimmingCharacters(in: CharacterSet(charactersIn: "\"' \t")).lowercased()
    }

    /// Имя IANA → кодировка Foundation. Частые — явно (быстро и без
    /// сюрпризов CF), остальные — через CoreFoundation.
    static func encoding(_ name: String) -> String.Encoding? {
        switch name {
        case "utf-8", "utf8", "unicode-1-1-utf-8": return .utf8
        case "us-ascii", "ascii", "ansi_x3.4-1968": return .utf8
        case "windows-1251", "cp1251", "win-1251", "x-cp1251": return .windowsCP1251
        case "windows-1252", "cp1252": return .windowsCP1252
        case "iso-8859-1", "latin1", "iso8859-1", "l1": return .isoLatin1
        case "utf-16": return .utf16
        default: break
        }
        // Синонимы, которых не знает CF.
        let alias: [String: String] = ["koi8r": "koi8-r", "koi8u": "koi8-u", "cp866": "ibm866", "866": "ibm866", "win1251": "windows-1251"]
        let iana = alias[name] ?? name
        let cf = CFStringConvertIANACharSetNameToEncoding(iana as CFString)
        guard cf != kCFStringEncodingInvalidId else { return nil }
        return String.Encoding(rawValue: CFStringConvertEncodingToNSStringEncoding(cf))
    }
}
