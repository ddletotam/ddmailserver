import Foundation

/// Даты писем: RFC 5322 (`Date:`) терпимо к отклонениям и `INTERNALDATE` IMAP.
public enum MailDate {
    private static let months = ["jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"]
    private static let zones: [String: Int] = [
        "ut": 0, "utc": 0, "gmt": 0, "z": 0,
        "est": -5 * 60, "edt": -4 * 60, "cst": -6 * 60, "cdt": -5 * 60,
        "mst": -7 * 60, "mdt": -6 * 60, "pst": -8 * 60, "pdt": -7 * 60,
        "msk": 3 * 60, "cet": 60, "cest": 2 * 60,
    ]

    /// Unix-секунды или `nil`, если дату не разобрать. Принимает день недели
    /// (или без него), двузначный год, пояс числом или именем, комментарий
    /// в скобках, секунды по желанию.
    public static func parse(_ raw: String) -> Int64? {
        var s = raw
        if let p = s.firstIndex(of: "(") { s = String(s[..<p]) }
        if let comma = s.firstIndex(of: ",") { s = String(s[s.index(after: comma)...]) }
        let tokens = s.split(whereSeparator: \.isWhitespace).map(String.init)
        // Ожидаем: день месяц год время [пояс] (IMAP: "17-Jul-1996 02:44:25 -0700").
        var t = tokens
        if t.count >= 1, t[0].contains("-"), t[0].split(separator: "-").count == 3 {
            t = t[0].split(separator: "-").map(String.init) + t.dropFirst()
        }
        guard t.count >= 4 else { return nil }
        // День недели без запятой («Mon 17 Jul …») — пропускаем.
        if Int(t[0]) == nil, t.count >= 5 { t.removeFirst() }
        guard let day = Int(t[0]),
              let mi = months.firstIndex(of: String(t[1].prefix(3)).lowercased()),
              var year = Int(t[2])
        else { return nil }
        if year < 50 { year += 2000 } else if year < 100 { year += 1900 }
        let hms = t[3].split(separator: ":").compactMap { Int($0) }
        guard hms.count >= 2 else { return nil }
        var offset = 0
        if t.count >= 5 {
            let z = t[4]
            if let sign = z.first, sign == "+" || sign == "-", z.count >= 5, let v = Int(z.dropFirst().prefix(4)) {
                offset = (v / 100 * 60 + v % 100) * (sign == "-" ? -1 : 1)
            } else {
                offset = zones[z.lowercased()] ?? 0
            }
        }
        var c = DateComponents()
        c.year = year; c.month = mi + 1; c.day = day
        c.hour = hms[0]; c.minute = hms[1]; c.second = hms.count > 2 ? hms[2] : 0
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = TimeZone(secondsFromGMT: 0)!
        guard let d = cal.date(from: c) else { return nil }
        return Int64(d.timeIntervalSince1970) - Int64(offset * 60)
    }

    /// Дата для заголовка исходящего письма (RFC 5322, английские имена).
    public static func format(_ date: Date, timeZone: TimeZone = .current) -> String {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.timeZone = timeZone
        f.dateFormat = "EEE, dd MMM yyyy HH:mm:ss Z"
        return f.string(from: date)
    }

    /// Дата для `APPEND` IMAP: `"17-Jul-1996 02:44:25 -0700"`.
    public static func imapFormat(_ date: Date, timeZone: TimeZone = .current) -> String {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.timeZone = timeZone
        f.dateFormat = "dd-MMM-yyyy HH:mm:ss Z"
        return f.string(from: date)
    }
}
