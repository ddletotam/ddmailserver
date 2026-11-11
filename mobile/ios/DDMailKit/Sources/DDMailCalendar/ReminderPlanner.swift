import DDMailAPI
import Foundation

/// Строка напоминания — одно вхождение события (как `ReminderEntity` Android).
public struct ReminderRow: Codable, Equatable, Hashable, Sendable {
    public var eventID: Int64
    public var occStart: Int64
    /// 0 — у события нет конца.
    public var occEnd: Int64
    public var calendarID: Int64
    public var summary: String
    /// Минуты до начала, по убыванию: «15,5».
    public var leads: String
    public var signature: String
    /// Индекс следующего звена каскада.
    public var next: Int
    public var snoozeAt: Int64?
    public var state: String

    public var key: Key { Key(eventID: eventID, occStart: occStart) }

    public struct Key: Hashable, Sendable, Codable {
        public let eventID: Int64
        public let occStart: Int64
    }
}

/// Машина состояний напоминаний — порт `ReminderPlanner` Android, та же модель,
/// что у десктопа (`reminders.rs`, спецификация 2026-07-11): посев из событий,
/// срок следующего звена, что делать при срабатывании.
///
/// Каскад — VALARM'ы события (минуты до начала). На iOS приложение не будится в
/// срок, поэтому каскад планируется целиком локальными уведомлениями
/// (`schedule`), а «✕» и «напомнить позже» меняют строку и расписание.
public enum ReminderPlanner {
    public static let armed = "armed"
    public static let done = "done"
    public static let cancelled = "cancelled"

    /// Вхождение без конца считается идущим столько.
    public static let noEndRunningMs: Int64 = 30 * 60 * 1000
    /// Окно посева: от сегодняшней полуночи на 8 суток (как `for_reminders` десктопа).
    public static let windowDays = 8
    static let pruneAfterMs: Int64 = 48 * 3600 * 1000
    /// «Наступило» превращается в «уже идёт», если опоздали больше чем на это.
    static let lateMs: Int64 = 2 * 60 * 1000

    public enum Mode: Sendable, Equatable { case soon, atStart, running }

    public struct Seed: Equatable, Sendable {
        public var delete: [ReminderRow.Key]
        public var put: [ReminderRow]
    }

    public static func leads(of r: ReminderRow) -> [Int] {
        r.leads.split(separator: ",").compactMap { Int($0.trimmingCharacters(in: .whitespaces)) }
    }

    /// Когда сработать; `nil` — ничего не взведено.
    public static func fireAt(_ r: ReminderRow) -> Int64? {
        guard r.state == armed else { return nil }
        if let s = r.snoozeAt { return s }
        let l = leads(of: r)
        guard l.indices.contains(r.next) else { return nil }
        return r.occStart - Int64(l[r.next]) * 60_000
    }

    public static func end(_ r: ReminderRow) -> Int64 { r.occEnd > 0 ? r.occEnd : r.occStart + noEndRunningMs }

    /// Первое звено, которое ещё впереди; все прошли, а событие нет — последнее (сработает сразу).
    static func firstNext(_ occStart: Int64, _ leads: [Int], _ now: Int64) -> Int {
        leads.firstIndex { occStart - Int64($0) * 60_000 > now } ?? max(leads.count - 1, 0)
    }

    /// Посев из событий окна `[windowFrom, windowTo)`. Скрытые календари и
    /// события на весь день не звонят. Строки окна, которых события больше не
    /// производят (удалили, перенесли, скрыли календарь), — удаляются. Решение
    /// пользователя (снуз, ✕) держится за логическое вхождение «начало +
    /// название», а не за id события: сервер может пересоздать событие с новым id.
    public static func seed(
        existing: [ReminderRow], events: [EventDTO], hidden: Set<Int64>, now: Int64,
        windowFrom: Int64, windowTo: Int64, timeZone: TimeZone = .current
    ) -> Seed {
        var byKey = Dictionary(existing.map { ($0.key, $0) }, uniquingKeysWith: { a, _ in a })
        var put: [ReminderRow] = []
        var delete: [ReminderRow.Key] = []
        var produced = Set<ReminderRow.Key>()
        struct Logical: Hashable { let start: Int64; let summary: String }
        var logical = Dictionary(existing.map { (Logical(start: $0.occStart, summary: $0.summary), $0) }, uniquingKeysWith: { a, _ in a })

        for ev in events {
            let summary = ev.summary.trimmingCharacters(in: .whitespaces)
            if summary.isEmpty || ev.allDay || hidden.contains(ev.calendarID) { continue }
            let raw = ev.alarmLeads.isEmpty ? (ev.alarmLeadMin > 0 ? [ev.alarmLeadMin] : [10]) : ev.alarmLeads
            var seen = Set<Int>()
            let leads = raw.filter { $0 >= 0 && seen.insert($0).inserted }.sorted(by: >)
            if leads.isEmpty { continue }
            let leadsText = leads.map(String.init).joined(separator: ",")
            let signature = "\(ev.dtstart)|\(ev.dtend ?? 0)|\(leadsText)|\(summary)"
            let dur = ev.dtend.map { max($0 - ev.dtstart, 0) } ?? 0
            let starts = ev.rrule.trimmingCharacters(in: .whitespaces).isEmpty
                ? [ev.dtstart]
                : Recurrence.expand(dtstartMs: ev.dtstart, dtendMs: ev.dtend, rrule: ev.rrule, exdates: ev.exdates,
                                    windowStartMs: windowFrom, windowEndMs: windowTo, timeZone: timeZone).map(\.startMs)
            for start in starts {
                if start <= now || start >= windowTo { continue }
                let key = ReminderRow.Key(eventID: ev.id, occStart: start)
                produced.insert(key)
                if var old = byKey[key] {
                    if old.signature != signature && old.state != cancelled {
                        old.occEnd = dur > 0 ? start + dur : 0
                        old.calendarID = ev.calendarID
                        old.summary = summary
                        old.leads = leadsText
                        old.signature = signature
                        old.next = firstNext(start, leads, now)
                        old.snoozeAt = nil
                        old.state = armed
                        put.append(old)
                    }
                    continue
                }
                var row = ReminderRow(
                    eventID: ev.id, occStart: start, occEnd: dur > 0 ? start + dur : 0, calendarID: ev.calendarID,
                    summary: summary, leads: leadsText, signature: signature,
                    next: firstNext(start, leads, now), snoozeAt: nil, state: armed
                )
                let lk = Logical(start: start, summary: summary)
                if let twin = logical[lk], twin.eventID != ev.id {
                    // Живой близнец — одна встреча, одно напоминание.
                    if produced.contains(twin.key) { continue }
                    // Событие пересоздано с новым id: решение переезжает на него.
                    row.next = twin.next
                    row.snoozeAt = twin.snoozeAt
                    row.state = twin.state
                    put.append(row)
                    delete.append(twin.key)
                    byKey[twin.key] = nil
                } else {
                    put.append(row)
                }
                logical[lk] = row
            }
        }
        for r in byKey.values where !produced.contains(r.key) {
            let inWindow = r.occStart >= windowFrom && r.occStart < windowTo
            if inWindow || r.occStart < now - pruneAfterMs { delete.append(r.key) }
        }
        var seenDel = Set<ReminderRow.Key>()
        return Seed(delete: delete.filter { seenDel.insert($0).inserted }, put: put)
    }

    /// Звено сработало: что показать и какой стала строка. `nil`-режим — молча.
    public static func fire(_ r: ReminderRow, now: Int64) -> (Mode?, ReminderRow) {
        var out = r
        if now >= end(r) {
            out.state = done
            out.snoozeAt = nil
            return (nil, out)
        }
        if now >= r.occStart {
            out.state = done
            out.snoozeAt = nil
            return (now - r.occStart > lateMs ? .running : .atStart, out)
        }
        let wasSnooze = r.snoozeAt != nil
        let l = leads(of: r)
        // После снуза каскад не продолжается: пользователь выбрал срок сам.
        var n = wasSnooze ? l.count : r.next + 1
        let mode: Mode = r.occStart - now <= 60_000 ? .atStart : .soon
        // Звенья, которые уже в прошлом (устройство спало), пропускаем.
        while n < l.count && r.occStart - Int64(l[n]) * 60_000 <= now { n += 1 }
        out.next = n
        out.snoozeAt = nil
        out.state = n < l.count ? armed : done
        return (mode, out)
    }

    /// «Напомнить позже» — шаги, которые ложатся до начала (как `snooze_steps` десктопа).
    public static let snoozeSteps = [1, 5, 10, 15, 30, 60, 120, 180, 360, 720, 1440]

    public static func snoozeSteps(minutesUntil: Int64) -> [Int] { snoozeSteps.filter { Int64($0) < minutesUntil } }

    public static func snoozeLabel(_ m: Int) -> String {
        m < 60 ? "Через \(m) мин" : m < 1440 ? "Через \(m / 60) ч" : "Через \(m / 1440) д"
    }

    // MARK: Расписание для iOS (контракт iOS §11)

    /// Запланированное локальное уведомление: одно звено каскада.
    public struct Planned: Equatable, Sendable {
        public let id: String
        public let key: ReminderRow.Key
        public let fireAt: Int64
        public let mode: Mode
        public let summary: String
    }

    public static func notificationID(_ key: ReminderRow.Key, link: Int) -> String {
        "rem|\(key.eventID)|\(key.occStart)|\(link)"
    }

    public static func parseNotificationID(_ id: String) -> ReminderRow.Key? {
        let p = id.split(separator: "|")
        guard p.count == 4, p[0] == "rem", let e = Int64(p[1]), let s = Int64(p[2]) else { return nil }
        return ReminderRow.Key(eventID: e, occStart: s)
    }

    /// Все звенья всех взведённых строк, которые ещё впереди, по времени; не
    /// больше `limit` (iOS держит до 64 ожидающих уведомлений на приложение —
    /// ближайшие важнее, дальние доедут следующим пересевом). Снуз — одно
    /// звено вместо каскада. Звенья, которые уже в прошлом, кроме самого
    /// позднего, отбрасываются — как `fire` пропускает их на Android.
    public static func schedule(_ rows: [ReminderRow], now: Int64, limit: Int = 60) -> [Planned] {
        var out: [Planned] = []
        for r in rows where r.state == armed {
            if let s = r.snoozeAt {
                out.append(Planned(id: notificationID(r.key, link: 99), key: r.key, fireAt: max(s, now + 1000),
                                   mode: r.occStart - s <= 60_000 ? .atStart : .soon, summary: r.summary))
                continue
            }
            let l = leads(of: r)
            guard l.indices.contains(r.next) else { continue }
            var links = Array(r.next..<l.count).map { (i: $0, at: r.occStart - Int64(l[$0]) * 60_000) }
            // Прошедшие звенья — одно уведомление сразу (последнее из прошедших).
            if let lastPast = links.last(where: { $0.at <= now }) {
                links = [(lastPast.i, now + 1000)] + links.filter { $0.at > now }
            }
            for link in links where link.at < end(r) {
                out.append(Planned(id: notificationID(r.key, link: link.i), key: r.key, fireAt: link.at,
                                   mode: r.occStart - link.at <= 60_000 ? .atStart : .soon, summary: r.summary))
            }
        }
        return Array(out.sorted { ($0.fireAt, $0.id) < ($1.fireAt, $1.id) }.prefix(limit))
    }
}
