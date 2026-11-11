import DDMailAPI
import Foundation

/// Календарная дата без времени и пояса (как `LocalDate`).
public struct Day: Hashable, Comparable, Sendable, CustomStringConvertible {
    public let year: Int
    public let month: Int
    public let day: Int

    public init(_ year: Int, _ month: Int, _ day: Int) {
        self.year = year
        self.month = month
        self.day = day
    }

    public init(_ date: Date, _ cal: Calendar) {
        let c = cal.dateComponents([.year, .month, .day], from: date)
        self.init(c.year ?? 1970, c.month ?? 1, c.day ?? 1)
    }

    public static func < (a: Day, b: Day) -> Bool { (a.year, a.month, a.day) < (b.year, b.month, b.day) }

    /// Полночь этого дня в календаре `cal`.
    public func start(_ cal: Calendar) -> Date {
        cal.date(from: DateComponents(year: year, month: month, day: day)) ?? .distantPast
    }

    public func adding(_ days: Int, _ cal: Calendar) -> Day {
        Day(cal.date(byAdding: .day, value: days, to: start(cal)) ?? start(cal), cal)
    }

    public var month1: Month { Month(year, month) }
    public var description: String { String(format: "%04d-%02d-%02d", year, month, day) }
}

/// Месяц (как `YearMonth`).
public struct Month: Hashable, Comparable, Sendable {
    public let year: Int
    public let month: Int

    public init(_ year: Int, _ month: Int) {
        let total = year * 12 + (month - 1)
        self.year = total / 12
        self.month = total % 12 + 1
    }

    public static func < (a: Month, b: Month) -> Bool { (a.year, a.month) < (b.year, b.month) }
    public func adding(_ n: Int) -> Month { Month(year, month + n) }
    public var first: Day { Day(year, month, 1) }

    public func length(_ cal: Calendar) -> Int {
        cal.range(of: .day, in: .month, for: first.start(cal))?.count ?? 30
    }
}

/// Вхождение события на экране: мастер развёрнут, цвет решён.
public struct Occ: Equatable, Identifiable, Sendable {
    public let event: EventDTO
    public let start: Int64
    public let end: Int64
    /// `0xAARRGGBB`.
    public let color: UInt32

    public var id: String { "\(event.id):\(start)" }
    public var allDay: Bool { event.allDay }
    public var recurring: Bool { !event.rrule.trimmingCharacters(in: .whitespaces).isEmpty }

    /// Даты, которые вхождение занимает (конец исключён).
    public func dates(_ cal: Calendar) -> [Day] {
        let first = Occ.dateOf(start, allDay: allDay, cal)
        var lastExcl: Day
        if allDay {
            lastExcl = Occ.dateOf(end, allDay: true, cal)
            if lastExcl <= first { lastExcl = first.adding(1, cal) }
        } else {
            lastExcl = Occ.dateOf(max(end - 1, start), allDay: false, cal).adding(1, cal)
        }
        var out: [Day] = []
        var d = first
        while d < lastExcl && out.count < 400 {
            out.append(d)
            d = d.adding(1, cal)
        }
        return out
    }

    /// Дата события «на весь день». Из CalDAV они приходят полночью UTC
    /// (`VALUE=DATE`), созданные клиентом — местной полночью; полночь UTC
    /// читается как дата UTC, иначе к западу от Гринвича день съезжал бы на
    /// сутки назад.
    public static func dateOf(_ ms: Int64, allDay: Bool, _ cal: Calendar) -> Day {
        let d = date(ms)
        if allDay && ms % 86_400_000 == 0 {
            var utc = Calendar(identifier: .gregorian)
            utc.timeZone = TimeZone(identifier: "UTC")!
            return Day(d, utc)
        }
        return Day(d, cal)
    }
}

/// Цвета календарей: серверный, а без него — из той же палитры, что у Android.
public enum CalendarColors {
    static let fallback: [UInt32] = [0xFF3B82F6, 0xFF10B981, 0xFFF59E0B, 0xFFEF4444, 0xFF8B5CF6, 0xFF06B6D4, 0xFFEC4899, 0xFF84CC16]

    /// `#RRGGBB` или `#RRGGBBAA` (CalDAV/Apple) → `0xAARRGGBB`.
    public static func parse(_ hex: String) -> UInt32? {
        var h = Substring(hex.trimmingCharacters(in: .whitespaces))
        if h.hasPrefix("#") { h = h.dropFirst() }
        guard h.allSatisfy(\.isHexDigit), let v = UInt64(h, radix: 16) else { return nil }
        switch h.count {
        case 6: return 0xFF00_0000 | UInt32(v)
        case 8: return UInt32((v & 0xFF) << 24 | v >> 8)
        default: return nil
        }
    }

    public static func of(_ cal: CalendarDTO?, id: Int64, eventColor: String = "") -> UInt32 {
        parse(eventColor) ?? cal.flatMap { parse($0.color) } ?? fallback[Int(((id % 8) + 8) % 8)]
    }
}

/// Развёртка событий месяца по дням. Многодневное попадает в каждый свой день;
/// месяц знает только свои дни — соседний приходит своим запросом (сервер
/// отдаёт всё, что пересекает окно).
public func indexMonth(
    _ month: Month, events: [EventDTO], calendars: [Int64: CalendarDTO], hidden: Set<Int64>, calendar cal: Calendar
) -> [Day: [Occ]] {
    let from = ms(month.first.start(cal))
    let to = ms(month.adding(1).first.start(cal))
    var out: [Day: [Occ]] = [:]
    var seen = Set<String>()
    for ev in events where !hidden.contains(ev.calendarID) {
        let color = CalendarColors.of(calendars[ev.calendarID], id: ev.calendarID, eventColor: ev.color)
        let occs: [Occurrence] = ev.allDay && ev.rrule.trimmingCharacters(in: .whitespaces).isEmpty
            ? [Occurrence(startMs: ev.dtstart, endMs: ev.dtend ?? ev.dtstart)]
            // Окно расширено на сутки: у дат «весь день» в UTC другая полночь.
            : Recurrence.expand(dtstartMs: ev.dtstart, dtendMs: ev.dtend, rrule: ev.rrule, exdates: ev.exdates,
                                windowStartMs: from - 86_400_000, windowEndMs: to + 86_400_000, timeZone: cal.timeZone)
        for o in occs {
            let occ = Occ(event: ev, start: o.startMs, end: o.endMs, color: color)
            guard seen.insert(occ.id).inserted else { continue }
            for d in occ.dates(cal) where d.month1 == month { out[d, default: []].append(occ) }
        }
    }
    return out.mapValues { list in
        list.sorted { a, b in
            if a.allDay != b.allDay { return a.allDay }
            if a.start != b.start { return a.start < b.start }
            return a.event.summary < b.event.summary
        }
    }
}

/// Ссылки события (§5в десктопа, упрощённо, порт `EventLinks` Android): созвон
/// лежит в LOCATION, в CONFERENCE/X-…-CONFERENCE/URL или в описании, часто
/// голым хостом без схемы. Голый хост признаём, только когда он занимает поле
/// целиком или начинается с `www.` — иначе `report.docx` и адреса почты стали
/// бы ссылками.
public enum EventLinks {
    nonisolated(unsafe) static let url = try! Regex(#"https?://[^\s<>"']+"#).ignoresCase()
    nonisolated(unsafe) static let bare = try! Regex(#"^(www\.)?[a-z0-9-]+(\.[a-z0-9-]+)*\.[a-z]{2,}(:\d+)?(/\S*)?$"#).ignoresCase()
    nonisolated(unsafe) static let www = try! Regex(#"\bwww\.[a-z0-9-]+(\.[a-z0-9-]+)+(/[^\s<>"']*)?"#).ignoresCase()
    static let fileExt: Set<String> = ["doc", "docx", "xls", "xlsx", "pdf", "txt", "zip", "png", "jpg", "ppt", "pptx", "csv"]

    public static func trimTail(_ u: String) -> String {
        var s = u
        while let last = s.last, ".,;:)]!?»\"'".contains(last) { s.removeLast() }
        return s
    }

    /// Ссылки из текста: со схемой и `www.`.
    public static func inText(_ text: String) -> [String] {
        var out = text.matches(of: url).map { trimTail(String(text[$0.range])) }
        for m in text.matches(of: www) {
            let v = trimTail(String(text[m.range]))
            if !out.contains(where: { $0.localizedCaseInsensitiveContains(v) }) { out.append("https://\(v)") }
        }
        return out
    }

    /// Поле целиком — голый хост (`telemost.yandex.ru/j/123`)? Тогда ссылка с https.
    public static func bareField(_ value: String) -> String? {
        let v = value.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !v.contains("@"), !v.contains(" "), v.wholeMatch(of: bare) != nil else { return nil }
        let host = v.split(separator: "/").first.map(String.init) ?? v
        let tld = (host.split(separator: ":").first.map(String.init) ?? host).split(separator: ".").last.map { $0.lowercased() } ?? ""
        if fileExt.contains(tld) { return nil }
        return "https://\(v)"
    }

    public static func target(_ value: String) -> String? {
        let v = value.trimmingCharacters(in: .whitespacesAndNewlines)
        if v.wholeMatch(of: url) != nil { return trimTail(v) }
        return bareField(value) ?? inText(value).first
    }

    /// Все ссылки события без повторов (сравнение по нормализованному виду).
    public static func of(_ ev: EventDTO) -> [String] {
        var found: [String] = []
        for e in ev.extras where e.name == "URL" || e.name.hasSuffix("CONFERENCE") {
            if let t = target(e.value) { found.append(t) }
        }
        if let t = target(ev.location) { found.append(t) }
        found += inText(ev.description)
        var seen = Set<String>()
        return found.filter { u in
            var k = u.lowercased()
            for p in ["https://", "http://"] where k.hasPrefix(p) { k.removeFirst(p.count) }
            while k.hasSuffix("/") { k.removeLast() }
            return seen.insert(k).inserted
        }
    }
}
