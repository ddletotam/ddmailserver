import DDMailAPI

/// Цвет строки беседы по группе адреса, на который пришла почта (Android §5,
/// десктоп §1в). Документ групп — `/identity-groups`, общий с десктопом:
/// у группы свой `color`, пустой — пастель палитры по индексу группы
/// (`identities::group_color`). `user+tag@` без своей записи берёт цвет
/// `user@`; адрес вне групп — серый `unknown`.
///
/// Цвета — `0xAARRGGBB`, чтобы модуль не зависел от SwiftUI.
public struct IdentityColors: Equatable, Sendable {
    private let byAddr: [String: UInt32]

    public init(byAddr: [String: UInt32] = [:]) {
        self.byAddr = byAddr
    }

    /// `IDENT_PASTEL` десктопа (sidebar.rs) — тот же порядок.
    public static let pastel: [UInt32] = [
        0xFFE4E1, 0xE8F5E9, 0xE3F2FD, 0xFFF9C4, 0xF3E5F5, 0xE0F7FA, 0xFBE9E7, 0xF1F8E9,
        0xEDE7F6, 0xE8EAF6, 0xFCE4EC, 0xE0F2F1, 0xFFF3E0, 0xF9FBE7, 0xEFEBE9, 0xECEFF1,
        0xFFF8E1, 0xE1F5FE, 0xDCEDC8, 0xFFCDD2, 0xF8BBD0, 0xD1C4E9, 0xB2DFDB, 0xB3E5FC,
    ].map { 0xFF00_0000 | $0 }

    /// «Некрасивый серый» десктопа (`IDENT_UNKNOWN`).
    public static let unknown: UInt32 = 0xFFD5_D5D0

    public func color(of addr: String) -> UInt32 {
        let lc = addr.trimmingCharacters(in: .whitespaces).lowercased()
        return byAddr[lc] ?? Self.baseOfTagged(lc).flatMap { byAddr[$0] } ?? Self.unknown
    }

    static func baseOfTagged(_ email: String) -> String? {
        guard let at = email.firstIndex(of: "@"), at > email.startIndex,
              let plus = email.firstIndex(of: "+"), plus > email.startIndex, plus < at
        else { return nil }
        return String(email[..<plus]) + String(email[at...])
    }

    /// `#RGB` / `#RRGGBB` / `#AARRGGBB`; прочее — `nil`.
    static func parseHex(_ s: String) -> UInt32? {
        var h = Substring(s.trimmingCharacters(in: .whitespaces))
        if h.hasPrefix("#") { h = h.dropFirst() }
        let full: String
        switch h.count {
        case 3: full = h.map { "\($0)\($0)" }.joined()
        case 6, 8: full = String(h)
        default: return nil
        }
        guard full.allSatisfy(\.isHexDigit), let v = UInt32(full, radix: 16) else { return nil }
        return full.count == 6 ? 0xFF00_0000 | v : v
    }

    public static func parse(_ doc: JSONValue?) -> IdentityColors {
        guard let groups = doc?["groups"]?.array else { return IdentityColors() }
        var m: [String: UInt32] = [:]
        for (gi, g) in groups.enumerated() {
            guard let obj = g.object else { continue }
            let own = obj["color"]?.string.flatMap { $0.trimmingCharacters(in: .whitespaces).isEmpty ? nil : parseHex($0) }
            let color = own ?? pastel[gi % pastel.count]
            for a in obj["addresses"]?.array ?? [] {
                guard let email = a["email"]?.string?.trimmingCharacters(in: .whitespaces).lowercased(),
                      !email.isEmpty, m[email] == nil
                else { continue }
                m[email] = color
            }
        }
        return IdentityColors(byAddr: m)
    }
}
