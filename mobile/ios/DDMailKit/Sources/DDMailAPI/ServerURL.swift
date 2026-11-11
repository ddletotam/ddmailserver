import Foundation

/// Почему адрес сервера не принят. Текст — для пользователя, как у Android.
public enum ServerURLError: Error, Equatable, LocalizedError {
    case empty
    case insecure
    case malformed

    public var errorDescription: String? {
        switch self {
        case .empty: "Введите адрес сервера."
        case .insecure: "Нужен https:// — по http:// пароль ушёл бы открытым текстом."
        case .malformed: "Не похоже на адрес сервера."
        }
    }
}

/// Адрес сервера из ввода пользователя → `https://хост[:порт]`.
///
/// Порт `normalizeServerUrl` Android (`ServerUrl.kt`), с теми же тестами.
/// Только https (контракт §3): по http пароль ушёл бы открытым текстом.
/// Схему можно не писать; путь, слэш в конце и пробелы отбрасываются;
/// кириллический домен переводится в punycode.
public func normalizeServerURL(_ input: String) -> Result<String, ServerURLError> {
    var raw = input.trimmingCharacters(in: .whitespacesAndNewlines)
    while raw.hasSuffix("/") { raw.removeLast() }
    if raw.isEmpty { return .failure(.empty) }
    let lower = raw.lowercased()
    if lower.hasPrefix("http://") { return .failure(.insecure) }
    if lower.hasPrefix("https://") { raw = String(raw.dropFirst("https://".count)) }

    // Authority — до начала пути, запроса или фрагмента.
    let authority = raw.prefix { $0 != "/" && $0 != "?" && $0 != "#" }
    if authority.contains("@") { return .failure(.malformed) }
    var host = Substring(authority)
    var port: Int?
    if let colon = authority.lastIndex(of: ":") {
        host = authority[..<colon]
        let digits = authority[authority.index(after: colon)...]
        guard !digits.isEmpty, digits.allSatisfy(\.isASCIIDigit),
              let p = Int(digits), (1...65535).contains(p)
        else { return .failure(.malformed) }
        port = p
    }
    guard host.contains("."), let ascii = idnToASCII(String(host)) else {
        return .failure(.malformed)
    }
    let suffix = port.map { $0 == 443 ? "" : ":\($0)" } ?? ""
    return .success("https://\(ascii)\(suffix)")
}

/// Домен адреса почты в ASCII (IDN → punycode); `nil` — это не адрес.
public func emailDomain(_ email: String) -> String? {
    let trimmed = email.trimmingCharacters(in: .whitespacesAndNewlines)
    guard let at = trimmed.lastIndex(of: "@"), at > trimmed.startIndex else { return nil }
    var domain = trimmed[trimmed.index(after: at)...].trimmingCharacters(in: .whitespaces)
    while domain.hasSuffix(".") { domain.removeLast() }
    guard domain.contains("."), !domain.hasPrefix(".") else { return nil }
    return idnToASCII(domain)
}

/// Имя хоста → ASCII-форма в нижнем регистре: метки с не-ASCII символами
/// кодируются punycode (RFC 3492) с префиксом `xn--`. `nil` — имя негодное:
/// пустая метка, недопустимый символ, метка длиннее 63.
func idnToASCII(_ host: String) -> String? {
    let labels = host.precomposedStringWithCanonicalMapping.lowercased()
        .split(separator: ".", omittingEmptySubsequences: false)
    var out: [String] = []
    for label in labels {
        guard !label.isEmpty else { return nil }
        let encoded: String
        if label.unicodeScalars.allSatisfy(\.isASCII) {
            encoded = String(label)
        } else {
            guard let p = Punycode.encode(String(label)) else { return nil }
            encoded = "xn--" + p
        }
        guard encoded.count <= 63,
              encoded.unicodeScalars.allSatisfy({ $0.isLDH }),
              !encoded.hasPrefix("-"), !encoded.hasSuffix("-")
        else { return nil }
        out.append(encoded)
    }
    return out.joined(separator: ".")
}

private extension Character {
    var isASCIIDigit: Bool { isASCII && isNumber }
}

private extension Unicode.Scalar {
    /// Буква, цифра или дефис ASCII — то, из чего состоит метка DNS.
    var isLDH: Bool {
        switch value {
        case 0x30...0x39, 0x61...0x7A, 0x2D: true
        default: false
        }
    }
}

/// Кодировщик punycode (RFC 3492, §6.3). Foundation публичного API IDNA не даёт.
enum Punycode {
    private static let base: UInt32 = 36, tMin: UInt32 = 1, tMax: UInt32 = 26
    private static let skew: UInt32 = 38, damp: UInt32 = 700
    private static let initialBias: UInt32 = 72, initialN: UInt32 = 128

    static func encode(_ input: String) -> String? {
        let scalars = input.unicodeScalars.map(\.value)
        var output = String(String.UnicodeScalarView(input.unicodeScalars.filter(\.isASCII)))
        let basic = UInt32(output.unicodeScalars.count)
        var handled = basic
        if basic > 0 { output.append("-") }

        var n = initialN, delta: UInt32 = 0, bias = initialBias
        while handled < UInt32(scalars.count) {
            guard let m = scalars.filter({ $0 >= n }).min() else { return nil }
            let (step, o1) = (m - n).multipliedReportingOverflow(by: handled + 1)
            let (next, o2) = delta.addingReportingOverflow(step)
            if o1 || o2 { return nil }
            delta = next
            n = m
            for c in scalars {
                if c < n {
                    let (d, o) = delta.addingReportingOverflow(1)
                    if o { return nil }
                    delta = d
                }
                guard c == n else { continue }
                var q = delta
                var k = base
                while true {
                    let t = k <= bias ? tMin : (k >= bias + tMax ? tMax : k - bias)
                    if q < t { break }
                    output.append(digit(t + (q - t) % (base - t)))
                    q = (q - t) / (base - t)
                    k += base
                }
                output.append(digit(q))
                bias = adapt(delta, points: handled + 1, first: handled == basic)
                delta = 0
                handled += 1
            }
            delta += 1
            n += 1
        }
        return output
    }

    private static func adapt(_ delta: UInt32, points: UInt32, first: Bool) -> UInt32 {
        var d = first ? delta / damp : delta / 2
        d += d / points
        var k: UInt32 = 0
        while d > ((base - tMin) * tMax) / 2 {
            d /= base - tMin
            k += base
        }
        return k + (base - tMin + 1) * d / (d + skew)
    }

    private static func digit(_ d: UInt32) -> Character {
        Character(Unicode.Scalar(d < 26 ? d + 0x61 : d - 26 + 0x30)!)
    }
}
