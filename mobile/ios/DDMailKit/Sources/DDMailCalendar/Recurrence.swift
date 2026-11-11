import Foundation

/// Одно вхождение события, мс.
public struct Occurrence: Equatable, Hashable, Sendable {
    public let startMs: Int64
    public let endMs: Int64

    public init(startMs: Int64, endMs: Int64) {
        self.startMs = startMs
        self.endMs = endMs
    }
}

/// Развёртка RRULE — порт `Recurrence` Android, тот же поднабор, что у
/// десктопа (`recurrence.rs`): FREQ=DAILY|WEEKLY|MONTHLY|YEARLY, INTERVAL,
/// COUNT, UNTIL, BYDAY (у WEEKLY). Прочее (BYMONTHDAY, BYSETPOS…)
/// игнорируется; неизвестная частота — событие как разовое.
///
/// Отличие от десктопа намеренное (Android §12): шаг считается по стенным
/// часам пояса устройства, а не по UTC. Иначе событие «по понедельникам в 01:00
/// МСК» (в UTC — воскресенье 22:00) раскладывалось бы на вторники, а
/// ежедневное уезжало бы на час при переходе на летнее время там, где он есть.
public enum Recurrence {
    private static let maxIter = 20_000
    private static let defaultDuration: Int64 = 30 * 60 * 1000

    enum Freq: String { case DAILY, WEEKLY, MONTHLY, YEARLY }

    struct Rule {
        let freq: Freq
        let interval: Int
        let count: Int?
        let untilMs: Int64?
        /// ISO: 1 — понедельник … 7 — воскресенье.
        let byDay: [Int]
    }

    public static func expand(
        dtstartMs: Int64, dtendMs: Int64?, rrule: String, exdates: some Collection<Int64>,
        windowStartMs: Int64, windowEndMs: Int64, timeZone: TimeZone = .current
    ) -> [Occurrence] {
        let dur = dtendMs.map { max($0 - dtstartMs, 0) } ?? defaultDuration
        let ex = Set(exdates)
        var out: [Occurrence] = []
        func overlaps(_ s: Int64) -> Bool {
            let e = s + dur
            // Нулевая длительность (точка во времени) тоже попадает в окно.
            return (e > windowStartMs || (dur == 0 && s >= windowStartMs)) && s < windowEndMs
        }
        func push(_ s: Int64) {
            if overlaps(s) && !ex.contains(s) { out.append(Occurrence(startMs: s, endMs: s + dur)) }
        }
        guard let rule = parse(rrule) else {
            push(dtstartMs)
            return out
        }
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = timeZone
        let start = date(dtstartMs)

        if rule.freq == .WEEKLY && !rule.byDay.isEmpty {
            let startDay = cal.startOfDay(for: start)
            let fromMonday = isoWeekday(start, cal) - 1
            guard let monday = cal.date(byAdding: .day, value: -fromMonday, to: startDay) else { return out }
            let t = cal.dateComponents([.hour, .minute, .second], from: start)
            let days = rule.byDay.sorted()
            var produced = 0
            for block in 0..<maxIter {
                guard let weekStart = cal.date(byAdding: .day, value: 7 * block * rule.interval, to: monday) else { break }
                var any = false
                for d in days {
                    guard let day = cal.date(byAdding: .day, value: d - 1, to: weekStart),
                          let at = cal.date(bySettingHour: t.hour ?? 0, minute: t.minute ?? 0, second: t.second ?? 0, of: day)
                    else { continue }
                    let s = ms(at)
                    if s < dtstartMs { continue }
                    if let u = rule.untilMs, s > u { return out }
                    if let c = rule.count, produced >= c { return out }
                    if s > windowEndMs && produced > 0 { return out }
                    produced += 1
                    any = true
                    push(s)
                }
                if !any && weekStart > startDay && ms(weekStart) > windowEndMs { return out }
            }
            return out
        }

        for i in 0..<maxIter {
            guard let s = nth(start, rule, i, cal) else { continue }
            if let u = rule.untilMs, s > u { break }
            if let c = rule.count, i >= c { break }
            if s > windowEndMs { break }
            push(s)
        }
        return out
    }

    /// n-е вхождение простого правила; `nil` — такой даты нет (31 февраля).
    private static func nth(_ start: Date, _ rule: Rule, _ n: Int, _ cal: Calendar) -> Int64? {
        let step = n * rule.interval
        switch rule.freq {
        case .DAILY:
            return cal.date(byAdding: .day, value: step, to: start).map(ms)
        case .WEEKLY:
            return cal.date(byAdding: .day, value: 7 * step, to: start).map(ms)
        case .MONTHLY, .YEARLY:
            let months = rule.freq == .MONTHLY ? step : step * 12
            let c = cal.dateComponents([.year, .month, .day, .hour, .minute, .second], from: start)
            guard let year = c.year, let month = c.month, let day = c.day else { return nil }
            let total = (year * 12 + (month - 1)) + months
            let y = total / 12, m = total % 12 + 1
            guard let first = cal.date(from: DateComponents(year: y, month: m, day: 1)),
                  let len = cal.range(of: .day, in: .month, for: first)?.count
            else { return nil }
            // Нет такого числа в месяце — вхождение пропускается (как у десктопа и RFC 5545).
            if day > len { return nil }
            return cal.date(from: DateComponents(year: y, month: m, day: day, hour: c.hour, minute: c.minute, second: c.second)).map(ms)
        }
    }

    static func parse(_ rrule: String) -> Rule? {
        var s = rrule.trimmingCharacters(in: .whitespaces)
        if s.hasPrefix("RRULE:") { s.removeFirst(6) }
        guard !s.isEmpty else { return nil }
        var freq: Freq?
        var interval = 1
        var count: Int?
        var until: Int64?
        var byDay: [Int] = []
        for part in s.split(separator: ";") {
            let kv = part.split(separator: "=", maxSplits: 1).map { $0.trimmingCharacters(in: .whitespaces) }
            guard kv.count == 2, !kv[0].isEmpty else { return nil }
            let v = kv[1]
            switch kv[0].uppercased() {
            case "FREQ":
                guard let f = Freq(rawValue: v.uppercased()) else { return nil }
                freq = f
            case "INTERVAL": interval = Int(v).flatMap { $0 > 0 ? $0 : nil } ?? 1
            case "COUNT": count = Int(v)
            case "UNTIL": until = parseUntil(v)
            case "BYDAY":
                for tok in v.split(separator: ",") {
                    let name = tok.trimmingCharacters(in: .whitespaces).drop { "+-0123456789".contains($0) }
                    if let d = weekday(String(name)) { byDay.append(d) }
                }
            default: break
            }
        }
        guard let freq else { return nil }
        return Rule(freq: freq, interval: interval, count: count, untilMs: until, byDay: byDay)
    }

    /// UNTIL: `YYYYMMDD` (конец дня) или `YYYYMMDDTHHMMSS[Z]`, как у десктопа — в UTC.
    static func parseUntil(_ v: String) -> Int64? {
        var t = v
        while t.hasSuffix("Z") { t.removeLast() }
        let parts = t.split(separator: "T", maxSplits: 1).map(String.init)
        let date = parts.first ?? ""
        guard date.count == 8, let y = Int(date.prefix(4)), let m = Int(date.dropFirst(4).prefix(2)), let d = Int(date.suffix(2)) else { return nil }
        var h = 23, mi = 59, se = 59
        if parts.count == 2, parts[1].count >= 6 {
            let time = parts[1]
            guard let hh = Int(time.prefix(2)), let mm = Int(time.dropFirst(2).prefix(2)), let ss = Int(time.dropFirst(4).prefix(2)) else { return nil }
            (h, mi, se) = (hh, mm, ss)
        }
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = TimeZone(identifier: "UTC")!
        let c = DateComponents(year: y, month: m, day: d, hour: h, minute: mi, second: se)
        guard c.isValidDate(in: cal), let at = cal.date(from: c) else { return nil }
        return ms(at)
    }

    private static func weekday(_ s: String) -> Int? {
        ["MO": 1, "TU": 2, "WE": 3, "TH": 4, "FR": 5, "SA": 6, "SU": 7][s.uppercased()]
    }

    /// Человеческое описание правила для карточки: «каждую неделю», «каждые 2 дня»…
    public static func describe(_ rrule: String) -> String {
        guard let r = parse(rrule) else { return "" }
        let n = r.interval
        let base = switch r.freq {
        case .DAILY: n == 1 ? "Каждый день" : "Каждые \(n) дн."
        case .WEEKLY: n == 1 ? "Каждую неделю" : "Каждые \(n) нед."
        case .MONTHLY: n == 1 ? "Каждый месяц" : "Каждые \(n) мес."
        case .YEARLY: n == 1 ? "Каждый год" : "Каждые \(n) г."
        }
        guard !r.byDay.isEmpty else { return base }
        let names = ["пн", "вт", "ср", "чт", "пт", "сб", "вс"]
        return base + ": " + r.byDay.sorted().map { names[$0 - 1] }.joined(separator: ", ")
    }
}

func date(_ ms: Int64) -> Date { Date(timeIntervalSince1970: TimeInterval(ms) / 1000) }
func ms(_ d: Date) -> Int64 { Int64((d.timeIntervalSince1970 * 1000).rounded()) }

/// ISO-день недели: 1 — понедельник … 7 — воскресенье.
func isoWeekday(_ d: Date, _ cal: Calendar) -> Int {
    (cal.component(.weekday, from: d) + 5) % 7 + 1
}
