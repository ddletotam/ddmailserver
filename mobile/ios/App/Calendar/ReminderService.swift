import DDMailAPI
import DDMailCalendar
import Foundation
import os
import UserNotifications

private let remLog = Logger(subsystem: "ru.letotam.ddmail", category: "reminders")

/// Напоминания (контракт iOS §11, модель Android §13). У iOS нет будильника,
/// который будит приложение в срок, поэтому каскад планируется целиком
/// локальными уведомлениями на окно посева `[сегодня, +8 суток)`; окно
/// пересевается на запуске, по `calendar_updated`, после своей правки и в фоне.
///
/// Состояние строк — в файле на устройстве. Звено, которое уже стояло в
/// расписании и чей срок прошёл, система показала сама: строка продвигается
/// без повтора уведомления.
@MainActor
final class ReminderService {
    nonisolated static let category = "reminder"
    static let snoozeActions: [(id: String, minutes: Int?, title: String)] = [
        ("snooze.5", 5, "Через 5 мин"), ("snooze.15", 15, "Через 15 мин"),
        ("snooze.60", 60, "Через 1 ч"), ("snooze.start", nil, "В момент начала"),
    ]

    private let api: SessionAPI
    private var rows: [ReminderRow] = []
    /// id уведомлений, которые мы ставили (их срок прошёл — значит, показаны).
    private var scheduled: Set<String> = []
    private let file = URL.applicationSupportDirectory.appending(path: "ddmail/reminders.json")

    private struct Saved: Codable {
        var rows: [ReminderRow]
        var scheduled: Set<String>
    }

    init(api: SessionAPI) {
        self.api = api
        if let d = try? Data(contentsOf: file), let s = try? JSONDecoder().decode(Saved.self, from: d) {
            rows = s.rows
            scheduled = s.scheduled
        }
    }

    static func registerCategory() async {
        let center = UNUserNotificationCenter.current()
        let actions = snoozeActions.map { UNNotificationAction(identifier: $0.id, title: $0.title, options: []) }
        // customDismissAction: смахнули уведомление — это «✕», тишина по вхождению.
        let cat = UNNotificationCategory(identifier: category, actions: actions, intentIdentifiers: [], options: [.customDismissAction])
        var set = await center.notificationCategories().filter { $0.identifier != category }
        set.insert(cat)
        center.setNotificationCategories(set)
    }

    // MARK: Посев

    private var running = false
    private var again = false

    /// Перечитать окно с сервера и пересеять. Выключены напоминания — снять всё.
    /// Пересевы не идут параллельно: снятие и постановка уведомлений одного
    /// перемешались бы с другим. Просьбы во время прохода сливаются в один повтор.
    func refresh() async {
        if running {
            again = true
            return
        }
        running = true
        defer { running = false }
        repeat {
            again = false
            await refreshOnce()
        } while again
    }

    private func refreshOnce() async {
        guard CalendarPrefs.shared.reminders else {
            await removeAll()
            return
        }
        var cal = Calendar.current
        cal.firstWeekday = 2
        let from = cal.startOfDay(for: Date())
        let to = cal.date(byAdding: .day, value: ReminderPlanner.windowDays, to: from) ?? from
        do {
            let events = try await api.calendarEvents(fromMs: ms(from) - 86_400_000, toMs: ms(to) + 86_400_000)
            reseed(events: events, windowFrom: ms(from), windowTo: ms(to))
            await reschedule()
        } catch {
            remLog.notice("reminders refresh: \(error.localizedDescription, privacy: .public)")
            await reschedule()
        }
    }

    private func reseed(events: [EventDTO], windowFrom: Int64, windowTo: Int64) {
        let now = ms(Date())
        advanceShown(now: now)
        let seed = ReminderPlanner.seed(
            existing: rows, events: events, hidden: CalendarPrefs.shared.hidden,
            now: now, windowFrom: windowFrom, windowTo: windowTo
        )
        let deleted = Set(seed.delete)
        var byKey = Dictionary(rows.filter { !deleted.contains($0.key) }.map { ($0.key, $0) }, uniquingKeysWith: { a, _ in a })
        for r in seed.put { byKey[r.key] = r }
        rows = byKey.values.sorted { $0.occStart < $1.occStart }
    }

    /// Звенья, которые стояли в расписании и уже прошли, система показала:
    /// продвинуть строку, не показывая их снова.
    private func advanceShown(now: Int64) {
        rows = rows.map { r in
            var r = r
            var guardN = 0
            while let at = ReminderPlanner.fireAt(r), at <= now, guardN < 32 {
                let id = r.snoozeAt != nil
                    ? ReminderPlanner.notificationID(r.key, link: 99)
                    : ReminderPlanner.notificationID(r.key, link: r.next)
                guard scheduled.contains(id) else { break }
                r = ReminderPlanner.fire(r, now: at).1
                guardN += 1
            }
            return r
        }
    }

    // MARK: Расписание

    private func reschedule() async {
        let center = UNUserNotificationCenter.current()
        let pending = await center.pendingNotificationRequests().map(\.identifier).filter { $0.hasPrefix("rem|") }
        center.removePendingNotificationRequests(withIdentifiers: pending)
        let now = ms(Date())
        let plan = ReminderPlanner.schedule(rows, now: now)
        var placed = Set<String>()
        for p in plan {
            let content = UNMutableNotificationContent()
            let minutes = max((p.key.occStart - p.fireAt) / 60_000, 0)
            content.title = p.mode == .soon ? "Скоро: \(p.summary)" : "Наступило: \(p.summary)"
            content.body = p.mode == .soon
                ? "Через \(Self.minutesText(minutes)) · \(date(p.key.occStart).formatted(date: .omitted, time: .shortened))"
                : "Начинается в \(date(p.key.occStart).formatted(date: .omitted, time: .shortened))"
            content.categoryIdentifier = Self.category
            content.threadIdentifier = "rem|\(p.key.eventID)|\(p.key.occStart)"
            content.userInfo = ["eventID": p.key.eventID, "occStart": p.key.occStart]
            content.sound = .default
            content.interruptionLevel = .timeSensitive
            let delay = max(TimeInterval(p.fireAt - now) / 1000, 1)
            let trigger = UNTimeIntervalNotificationTrigger(timeInterval: delay, repeats: false)
            do {
                try await center.add(UNNotificationRequest(identifier: p.id, content: content, trigger: trigger))
                placed.insert(p.id)
            } catch {
                remLog.error("schedule \(p.id, privacy: .public): \(error.localizedDescription, privacy: .public)")
            }
        }
        remLog.debug("reminders: rows \(self.rows.count), scheduled \(placed.count)")
        // Помним и прежние: их срок мог пройти, пока приложение спало.
        scheduled = scheduled.union(placed).filter { id in
            ReminderPlanner.parseNotificationID(id).map { k in rows.contains { $0.key == k } } ?? false
        }
        save()
    }

    private func removeAll() async {
        let center = UNUserNotificationCenter.current()
        let pending = await center.pendingNotificationRequests().map(\.identifier).filter { $0.hasPrefix("rem|") }
        center.removePendingNotificationRequests(withIdentifiers: pending)
        let delivered = await center.deliveredNotifications().map(\.request.identifier).filter { $0.hasPrefix("rem|") }
        center.removeDeliveredNotifications(withIdentifiers: delivered)
        rows = []
        scheduled = []
        save()
    }

    // MARK: Ответы пользователя

    /// Смахнули уведомление — «✕»: тишина по этому вхождению навсегда.
    func dismiss(_ key: ReminderRow.Key) async {
        update(key) { $0.state = ReminderPlanner.cancelled }
        await reschedule()
    }

    /// «Напомнить позже»: каскад заменяется одним сроком (`nil` — к началу).
    func snooze(_ key: ReminderRow.Key, minutes: Int?) async {
        let now = ms(Date())
        update(key) { r in
            let target = minutes.map { now + Int64($0) * 60_000 } ?? r.occStart
            r.snoozeAt = min(target, r.occStart)
            r.state = ReminderPlanner.armed
        }
        await reschedule()
    }

    private func update(_ key: ReminderRow.Key, _ change: (inout ReminderRow) -> Void) {
        guard let i = rows.firstIndex(where: { $0.key == key }) else { return }
        change(&rows[i])
    }

    /// Выход из учётки: напоминания чужой учётки не звонят.
    func clear() async { await removeAll() }

    private func save() {
        try? FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
        if let d = try? JSONEncoder().encode(Saved(rows: rows, scheduled: scheduled)) {
            try? d.write(to: file, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
        }
    }

    static func minutesText(_ m: Int64) -> String {
        m >= 1440 ? "\(m / 1440) д" : m >= 60 ? (m % 60 == 0 ? "\(m / 60) ч" : "\(m / 60) ч \(m % 60) мин") : "\(m) мин"
    }
}
