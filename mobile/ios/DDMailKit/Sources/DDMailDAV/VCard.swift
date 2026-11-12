import DDMailAPI
import Foundation

/// vCard (RFC 6350/2426) — порт `build_vcard`/`parse_vcard` десктопа; правка
/// существующей карточки меняет только свои поля (фото, адреса, дни
/// рождения остаются).
public enum VCard {
    public struct Card: Equatable, Sendable {
        public var uid = ""
        public var fullName = ""
        public var emails: [String] = []
        public var phones: [String] = []
        public var organization = ""
        public var title = ""
    }

    public static func parse(_ text: String) -> [Card] {
        let lines = ContentLine.parse(text)
        return ICal.components("VCARD", in: lines).compactMap { comp in
            var c = Card()
            for l in comp {
                let v = l.value.trimmingCharacters(in: .whitespaces)
                guard !v.isEmpty else { continue }
                switch l.name {
                case "UID": c.uid = v
                case "FN": c.fullName = unescapeText(v)
                case "EMAIL": c.emails.append(unescapeText(v).replacingOccurrences(of: "mailto:", with: "", options: .caseInsensitive))
                case "TEL": c.phones.append(unescapeText(v).replacingOccurrences(of: "tel:", with: "", options: .caseInsensitive))
                // ORG структурирован («Компания;Отдел») — первая часть.
                case "ORG": c.organization = unescapeText(String(v.split(separator: ";", omittingEmptySubsequences: false).first ?? ""))
                case "TITLE": c.title = unescapeText(v)
                default: break
                }
            }
            if c.fullName.isEmpty, let n = comp.first(where: { $0.name == "N" }) {
                let parts = n.value.split(separator: ";", omittingEmptySubsequences: false).map { unescapeText(String($0)) }
                c.fullName = [parts.count > 1 ? parts[1] : "", parts.first ?? ""].filter { !$0.isEmpty }.joined(separator: " ")
            }
            return c.fullName.isEmpty && c.emails.isEmpty ? nil : c
        }
    }

    public static func build(uid: String, fullName: String, emails: [String], phones: [String], organization: String, title: String) -> String {
        var v = "BEGIN:VCARD\r\nVERSION:3.0\r\n"
        v += folded("UID:\(uid)")
        if !fullName.isEmpty { v += folded("FN:\(escapeText(fullName))") }
        v += folded("N:;\(escapeText(fullName));;;")
        for e in emails where !e.trimmingCharacters(in: .whitespaces).isEmpty { v += folded("EMAIL:\(escapeText(e))") }
        for p in phones where !p.trimmingCharacters(in: .whitespaces).isEmpty { v += folded("TEL:\(escapeText(p))") }
        if !organization.isEmpty { v += folded("ORG:\(escapeText(organization))") }
        if !title.isEmpty { v += folded("TITLE:\(escapeText(title))") }
        return v + "END:VCARD\r\n"
    }

    /// Заменить FN/N/EMAIL/TEL/ORG/TITLE, остальное оставить.
    public static func merge(_ existing: String, fullName: String, emails: [String], phones: [String], organization: String, title: String) -> String {
        var logical: [String] = []
        for raw in existing.replacingOccurrences(of: "\r\n", with: "\n").split(separator: "\n", omittingEmptySubsequences: false) {
            if let f = raw.first, f == " " || f == "\t", !logical.isEmpty { logical[logical.count - 1] += raw.dropFirst() } else if !raw.isEmpty { logical.append(String(raw)) }
        }
        let replaced: Set<String> = ["FN", "N", "EMAIL", "TEL", "ORG", "TITLE"]
        var out: [String] = []
        for l in logical {
            let name = ContentLine.parseLine(l)?.name ?? ""
            if replaced.contains(name) { continue }
            if l.uppercased() == "END:VCARD" {
                if !fullName.isEmpty { out.append("FN:\(escapeText(fullName))") }
                out.append("N:;\(escapeText(fullName));;;")
                out += emails.filter { !$0.isEmpty }.map { "EMAIL:\(escapeText($0))" }
                out += phones.filter { !$0.isEmpty }.map { "TEL:\(escapeText($0))" }
                if !organization.isEmpty { out.append("ORG:\(escapeText(organization))") }
                if !title.isEmpty { out.append("TITLE:\(escapeText(title))") }
            }
            out.append(l)
        }
        return out.map { folded($0) }.joined()
    }
}
