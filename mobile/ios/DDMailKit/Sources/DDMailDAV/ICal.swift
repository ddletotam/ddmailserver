import DDMailAPI
import Foundation

/// Свойство iCalendar/vCard: имя, параметры, значение (строки развёрнуты).
public struct ContentLine: Equatable, Sendable {
    public var name: String
    public var params: [String: String]
    public var value: String

    /// Строки RFC 5545/6350: перенос с пробелом или табом — продолжение.
    public static func parse(_ text: String) -> [ContentLine] {
        var unfolded: [String] = []
        for raw in text.replacingOccurrences(of: "\r\n", with: "\n").split(separator: "\n", omittingEmptySubsequences: false) {
            if let f = raw.first, f == " " || f == "\t", !unfolded.isEmpty {
                unfolded[unfolded.count - 1] += raw.dropFirst()
            } else if !raw.isEmpty {
                unfolded.append(String(raw))
            }
        }
        return unfolded.compactMap(parseLine)
    }

    static func parseLine(_ line: String) -> ContentLine? {
        // Имя и параметры — до первого «:» вне кавычек.
        var quoted = false
        var colon: String.Index?
        for i in line.indices {
            let c = line[i]
            if c == "\"" { quoted.toggle() }
            if c == ":" && !quoted { colon = i; break }
        }
        guard let colon else { return nil }
        let head = line[..<colon]
        let value = String(line[line.index(after: colon)...])
        var parts: [String] = []
        var cur = ""
        quoted = false
        for c in head {
            if c == "\"" { quoted.toggle(); continue }
            if c == ";" && !quoted { parts.append(cur); cur = ""; continue }
            cur.append(c)
        }
        parts.append(cur)
        var params: [String: String] = [:]
        for p in parts.dropFirst() {
            let kv = p.split(separator: "=", maxSplits: 1)
            params[kv[0].uppercased()] = kv.count > 1 ? String(kv[1]) : ""
        }
        // Группа vCard (`item1.EMAIL`) — отбрасываем.
        var name = parts[0].uppercased()
        if let dot = name.lastIndex(of: ".") { name = String(name[name.index(after: dot)...]) }
        return ContentLine(name: name, params: params, value: value)
    }

    /// Текст с экранированием RFC 5545 (`\n`, `\,`, `\;`, `\\`).
    public var text: String { unescapeText(value) }
}

func unescapeText(_ s: String) -> String {
    var out = ""
    var esc = false
    for c in s {
        if esc {
            switch c {
            case "n", "N": out.append("\n")
            default: out.append(c)
            }
            esc = false
        } else if c == "\\" {
            esc = true
        } else {
            out.append(c)
        }
    }
    return out
}

func escapeText(_ s: String) -> String {
    s.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\n", with: "\\n")
        .replacingOccurrences(of: ",", with: "\\,").replacingOccurrences(of: ";", with: "\\;")
}

/// Строка свойства с переносом по 75 октетов (RFC 5545 §3.1).
func folded(_ line: String) -> String {
    var out = ""
    var cur = ""
    for ch in line {
        if (cur + String(ch)).utf8.count > 75 {
            out += cur + "\r\n "
            cur = ""
        }
        cur.append(ch)
    }
    return out + cur + "\r\n"
}

/// iCalendar (RFC 5545): события внешнего календаря в модель событий сервера
/// (`EventDTO`) — её уже понимают развёртка повторов, шкалы и напоминания.
public enum ICal {
    /// Компоненты `BEGIN:X … END:X` верхнего уровня внутри текста.
    static func components(_ name: String, in lines: [ContentLine]) -> [[ContentLine]] {
        var out: [[ContentLine]] = []
        var depth = 0
        var cur: [ContentLine] = []
        for l in lines {
            if l.name == "BEGIN" && l.value.uppercased() == name {
                depth += 1
                if depth == 1 { cur = []; continue }
            }
            if l.name == "END" && l.value.uppercased() == name {
                depth -= 1
                if depth == 0 { out.append(cur); continue }
            }
            if depth >= 1 { cur.append(l) }
        }
        return out
    }

    /// Пояс по TZID — база ОС (IANA); имена Windows (Outlook, Exchange) — по
    /// таблице частых. Неизвестный — пояс устройства (лучше, чем UTC десктопа).
    public static func timeZone(_ tzid: String?) -> TimeZone {
        guard var id = tzid?.trimmingCharacters(in: CharacterSet(charactersIn: "\" ")), !id.isEmpty else { return .current }
        if let tz = TimeZone(identifier: id) { return tz }
        // «/mozilla.org/20050126_1/Europe/Moscow» и подобные префиксы.
        if let r = id.range(of: #"[A-Za-z]+/[A-Za-z_]+(/[A-Za-z_]+)?$"#, options: .regularExpression) {
            id = String(id[r])
            if let tz = TimeZone(identifier: id) { return tz }
        }
        let windows: [String: String] = [
            "Russian Standard Time": "Europe/Moscow", "Russia Time Zone 3": "Europe/Samara",
            "Ekaterinburg Standard Time": "Asia/Yekaterinburg", "Omsk Standard Time": "Asia/Omsk",
            "N. Central Asia Standard Time": "Asia/Novosibirsk", "North Asia Standard Time": "Asia/Krasnoyarsk",
            "North Asia East Standard Time": "Asia/Irkutsk", "Yakutsk Standard Time": "Asia/Yakutsk",
            "Vladivostok Standard Time": "Asia/Vladivostok", "Kaliningrad Standard Time": "Europe/Kaliningrad",
            "Central Asia Standard Time": "Asia/Almaty", "West Asia Standard Time": "Asia/Tashkent",
            "UTC": "UTC", "GMT Standard Time": "Europe/London", "W. Europe Standard Time": "Europe/Berlin",
            "Central European Standard Time": "Europe/Warsaw", "FLE Standard Time": "Europe/Kiev",
            "E. Europe Standard Time": "Europe/Chisinau", "Belarus Standard Time": "Europe/Minsk",
            "Turkey Standard Time": "Europe/Istanbul", "Pacific Standard Time": "America/Los_Angeles",
            "Eastern Standard Time": "America/New_York", "Central Standard Time": "America/Chicago",
        ]
        return windows[id].flatMap(TimeZone.init(identifier:)) ?? .current
    }

    /// Дата-время свойства → (мс, весь день). `VALUE=DATE` — полночь UTC (как у
    /// сервера), `Z` — UTC, TZID — его пояс, плавающее — пояс устройства.
    public static func date(_ l: ContentLine) -> (ms: Int64, allDay: Bool)? {
        let v = l.value.trimmingCharacters(in: .whitespaces)
        var c = DateComponents()
        let digits = v.filter(\.isNumber)
        guard digits.count >= 8 else { return nil }
        let a = Array(digits)
        func n(_ r: Range<Int>) -> Int { Int(String(a[r])) ?? 0 }
        c.year = n(0..<4); c.month = n(4..<6); c.day = n(6..<8)
        let allDay = l.params["VALUE"]?.uppercased() == "DATE" || (v.count == 8 && !v.contains("T"))
        var cal = Calendar(identifier: .gregorian)
        if allDay {
            cal.timeZone = TimeZone(secondsFromGMT: 0)!
        } else {
            guard a.count >= 12 else { return nil }
            c.hour = n(8..<10); c.minute = n(10..<12); c.second = a.count >= 14 ? n(12..<14) : 0
            cal.timeZone = v.hasSuffix("Z") ? TimeZone(secondsFromGMT: 0)! : timeZone(l.params["TZID"])
        }
        guard let d = cal.date(from: c) else { return nil }
        return (Int64(d.timeIntervalSince1970 * 1000), allDay)
    }

    /// `DURATION` (`P1DT2H`, `PT30M`, `-PT15M`) → мс.
    public static func duration(_ s: String) -> Int64? {
        var t = s.uppercased()
        var sign: Int64 = 1
        if t.hasPrefix("-") { sign = -1; t.removeFirst() } else if t.hasPrefix("+") { t.removeFirst() }
        guard t.hasPrefix("P") else { return nil }
        t.removeFirst()
        var total: Int64 = 0
        var num = ""
        for ch in t {
            if ch.isNumber { num.append(ch); continue }
            let v = Int64(num) ?? 0
            num = ""
            switch ch {
            case "W": total += v * 7 * 86_400
            case "D": total += v * 86_400
            case "H": total += v * 3600
            case "M": total += v * 60
            case "S": total += v
            default: break
            }
        }
        return sign * total * 1000
    }

    /// Ресурс календаря (один VCALENDAR: мастер и его переопределения).
    /// Переопределение вхождения (`RECURRENCE-ID`) — отдельное событие без
    /// повтора, а его дата — исключение мастера. `makeID` даёт стабильный id.
    public static func events(_ text: String, calendarID: Int64, makeID: (String, String) -> Int64) -> [EventDTO] {
        let lines = ContentLine.parse(text)
        let vevents = components("VEVENT", in: lines)
        var masters: [String: Int] = [:]
        var out: [EventDTO] = []
        var overrides: [(uid: String, recurrence: Int64)] = []
        for comp in vevents {
            guard var e = event(comp, calendarID: calendarID) else { continue }
            let rid = comp.first { $0.name == "RECURRENCE-ID" }.flatMap(date)
            e.id = makeID(e.uid, rid.map { String($0.ms) } ?? "")
            if let rid {
                e.recurrenceID = String(rid.ms)
                e.rrule = ""
                overrides.append((e.uid, rid.ms))
            } else {
                masters[e.uid] = out.count
            }
            out.append(e)
        }
        for o in overrides {
            if let i = masters[o.uid], !out[i].exdates.contains(o.recurrence) { out[i].exdates.append(o.recurrence) }
        }
        return out
    }

    static func event(_ comp: [ContentLine], calendarID: Int64) -> EventDTO? {
        func first(_ n: String) -> ContentLine? { comp.first { $0.name == n } }
        guard let start = first("DTSTART").flatMap(date) else { return nil }
        var e = EventDTO(id: 0, calendarID: calendarID, dtstart: start.ms, allDay: start.allDay)
        e.uid = first("UID")?.value ?? ""
        e.summary = first("SUMMARY")?.text ?? ""
        e.description = first("DESCRIPTION")?.text ?? ""
        e.location = first("LOCATION")?.text ?? ""
        e.status = first("STATUS")?.value.uppercased() ?? ""
        if let end = first("DTEND").flatMap(date) {
            e.dtend = end.ms
        } else if let d = first("DURATION").flatMap({ duration($0.value) }) {
            e.dtend = start.ms + d
        } else if start.allDay {
            e.dtend = start.ms + 86_400_000
        }
        e.rrule = first("RRULE")?.value ?? ""
        for ex in comp where ex.name == "EXDATE" {
            for part in ex.value.split(separator: ",") {
                var l = ex
                l.value = String(part)
                if let d = date(l) { e.exdates.append(d.ms) }
            }
        }
        if let org = first("ORGANIZER") {
            e.organizerEmail = mailto(org.value)
            e.organizerName = org.params["CN"] ?? ""
        }
        e.attendees = comp.filter { $0.name == "ATTENDEE" }.map { a in
            AttendeeDTO(email: mailto(a.value), name: a.params["CN"] ?? "", role: a.params["ROLE"] ?? "",
                        partstat: a.params["PARTSTAT"]?.uppercased() ?? "")
        }
        // Напоминания — VALARM с относительным TRIGGER до начала.
        let lines = comp
        var leads: [Int] = []
        for alarm in components("VALARM", in: lines) {
            guard let trig = alarm.first(where: { $0.name == "TRIGGER" }),
                  trig.params["VALUE"]?.uppercased() != "DATE-TIME",
                  let d = duration(trig.value), d <= 0 else { continue }
            leads.append(Int(-d / 60_000))
        }
        e.alarmLeads = Array(Set(leads)).sorted(by: >)
        return e
    }

    static func mailto(_ v: String) -> String {
        let t = v.trimmingCharacters(in: .whitespaces)
        return (t.lowercased().hasPrefix("mailto:") ? String(t.dropFirst(7)) : t).lowercased()
    }

    // MARK: Запись

    /// Новое событие (порт `build_ical` десктопа).
    public static func build(uid: String, summary: String, description: String, location: String,
                             startMs: Int64, endMs: Int64?, allDay: Bool, now: Date = Date()) -> String {
        var s = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//ddmail//ios//RU\r\nBEGIN:VEVENT\r\n"
        s += folded("UID:\(uid)")
        s += folded("DTSTAMP:\(utc(Int64(now.timeIntervalSince1970 * 1000)))")
        s += dtLine("DTSTART", startMs, allDay)
        if let endMs { s += dtLine("DTEND", endMs, allDay) }
        if !summary.isEmpty { s += folded("SUMMARY:\(escapeText(summary))") }
        if !description.isEmpty { s += folded("DESCRIPTION:\(escapeText(description))") }
        if !location.isEmpty { s += folded("LOCATION:\(escapeText(location))") }
        return s + "END:VEVENT\r\nEND:VCALENDAR\r\n"
    }

    /// Правка существующего (порт `merge_ical`): меняются только свои поля
    /// мастера; RRULE, участники, напоминания, переопределения — остаются.
    public static func merge(_ existing: String, summary: String?, description: String?, location: String?,
                             startMs: Int64?, endMs: Int64?, allDay: Bool?) -> String {
        let isAllDay = allDay ?? false
        var updates: [(String, String?)] = []
        if let summary { updates.append(("SUMMARY", "SUMMARY:\(escapeText(summary))")) }
        if let description { updates.append(("DESCRIPTION", description.isEmpty ? nil : "DESCRIPTION:\(escapeText(description))")) }
        if let location { updates.append(("LOCATION", location.isEmpty ? nil : "LOCATION:\(escapeText(location))")) }
        if let startMs { updates.append(("DTSTART", dtLine("DTSTART", startMs, isAllDay).trimmingCharacters(in: .newlines))) }
        if startMs != nil {
            updates.append(("DTEND", endMs.map { dtLine("DTEND", $0, isAllDay).trimmingCharacters(in: .newlines) }))
            updates.append(("DURATION", nil))
        }
        return rewriteMaster(existing, updates)
    }

    /// Свой ответ на приглашение: PARTSTAT у участника с моим адресом.
    public static func setPartstat(_ existing: String, emails: Set<String>, partstat: String) -> String {
        rewriteMaster(existing, [], attendee: { l in
            guard emails.contains(mailto(l.value)) else { return nil }
            var p = l.params
            p["PARTSTAT"] = partstat
            p.removeValue(forKey: "RSVP")
            return serialize(ContentLine(name: "ATTENDEE", params: p, value: l.value))
        })
    }

    static func serialize(_ l: ContentLine) -> String {
        let params = l.params.sorted { $0.key < $1.key }.map { k, v in
            v.contains(":") || v.contains(";") || v.contains(",") ? "\(k)=\"\(v)\"" : "\(k)=\(v)"
        }
        return ([l.name] + params).joined(separator: ";") + ":" + l.value
    }

    /// Переписать свойства мастера (VEVENT без RECURRENCE-ID), сохраняя
    /// остальной текст как есть (с переносами строк — в исходном виде).
    static func rewriteMaster(_ text: String, _ updates: [(String, String?)], attendee: ((ContentLine) -> String?)? = nil) -> String {
        // Работаем по развёрнутым строкам: свойство может занимать несколько физических.
        var logical: [String] = []
        for raw in text.replacingOccurrences(of: "\r\n", with: "\n").split(separator: "\n", omittingEmptySubsequences: false) {
            if let f = raw.first, f == " " || f == "\t", !logical.isEmpty { logical[logical.count - 1] += raw.dropFirst() } else if !raw.isEmpty { logical.append(String(raw)) }
        }
        // Мастер — первый VEVENT без RECURRENCE-ID.
        var blocks: [(start: Int, end: Int, master: Bool)] = []
        var begin: Int?
        var hasRID = false
        var depth = 0
        for (i, l) in logical.enumerated() {
            let u = l.uppercased()
            if u == "BEGIN:VEVENT" { begin = i; hasRID = false; depth = 0 }
            if begin != nil && u.hasPrefix("BEGIN:") && u != "BEGIN:VEVENT" { depth += 1 }
            if begin != nil && u.hasPrefix("END:") && u != "END:VEVENT" { depth -= 1 }
            if begin != nil && depth == 0 && u.hasPrefix("RECURRENCE-ID") { hasRID = true }
            if u == "END:VEVENT", let b = begin { blocks.append((b, i, !hasRID)); begin = nil }
        }
        guard let m = blocks.first(where: \.master) ?? blocks.first else { return text }
        var out: [String] = []
        var seen = Set<String>()
        var inner = 0
        for (i, l) in logical.enumerated() {
            if i == m.end {
                // Не было в мастере — перед его END:VEVENT.
                out += updates.filter { !seen.contains($0.0) }.compactMap(\.1)
                out.append(l)
                continue
            }
            guard i > m.start && i < m.end else { out.append(l); continue }
            let u = l.uppercased()
            if u.hasPrefix("BEGIN:") { inner += 1 }
            if inner > 0 {
                out.append(l)
                if u.hasPrefix("END:") { inner -= 1 }
                continue
            }
            let parsed = ContentLine.parseLine(l)
            let name = parsed?.name ?? ""
            if let (_, repl) = updates.first(where: { $0.0 == name }) {
                seen.insert(name)
                if let repl { out.append(repl) }
                continue
            }
            if name == "ATTENDEE", let f = attendee, let p = parsed, let repl = f(p) {
                out.append(repl)
                continue
            }
            out.append(l)
        }
        return out.map { folded($0) }.joined()
    }

    static func dtLine(_ prop: String, _ ms: Int64, _ allDay: Bool) -> String {
        if allDay {
            let f = DateFormatter()
            f.locale = Locale(identifier: "en_US_POSIX")
            f.timeZone = TimeZone(secondsFromGMT: 0)
            f.dateFormat = "yyyyMMdd"
            return "\(prop);VALUE=DATE:\(f.string(from: Date(timeIntervalSince1970: TimeInterval(ms) / 1000)))\r\n"
        }
        return "\(prop):\(utc(ms))\r\n"
    }

    /// `20260715T120000Z`.
    public static func utc(_ ms: Int64) -> String {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.timeZone = TimeZone(secondsFromGMT: 0)
        f.dateFormat = "yyyyMMdd'T'HHmmss'Z'"
        return f.string(from: Date(timeIntervalSince1970: TimeInterval(ms) / 1000))
    }
}
