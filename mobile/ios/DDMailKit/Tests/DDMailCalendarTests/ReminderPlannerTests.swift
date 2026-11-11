import DDMailAPI
import Foundation
import Testing
@testable import DDMailCalendar

/// Векторы `ReminderPlannerTest.kt` Android плюс расписание iOS.
struct ReminderPlannerTests {
    let min: Int64 = 60_000
    let now: Int64 = 1_800_000_000_000
    var from: Int64 { now - 10 * 3_600_000 }
    var to: Int64 { now + 8 * 86_400_000 }
    typealias P = ReminderPlanner

    func ev(_ id: Int64, _ start: Int64, leads: [Int] = [15, 5], summary: String = "Созвон", cal: Int64 = 1, rrule: String = "", allDay: Bool = false) -> EventDTO {
        var e = EventDTO(id: id, calendarID: cal, summary: summary, dtstart: start, dtend: start + 60 * min, allDay: allDay, rrule: rrule, alarmLeads: leads)
        e.summary = summary
        return e
    }

    func seed(_ existing: [ReminderRow], _ e: EventDTO..., hidden: Set<Int64> = [], at: Int64? = nil) -> P.Seed {
        P.seed(existing: existing, events: e, hidden: hidden, now: at ?? now, windowFrom: from, windowTo: to, timeZone: TimeZone(identifier: "UTC")!)
    }

    @Test func seedsCascadeSortedAndFiresInOrder() throws {
        let r = try #require(seed([], ev(1, now + 60 * min, leads: [5, 15])).put.first)
        #expect(r.leads == "15,5")
        #expect(P.fireAt(r) == now + 45 * min)
        let (mode, after) = P.fire(r, now: now + 45 * min)
        #expect(mode == .soon)
        #expect(P.fireAt(after) == now + 55 * min)
        let (_, last) = P.fire(after, now: now + 55 * min)
        #expect(last.state == P.done)
        #expect(P.fireAt(last) == nil)
    }

    @Test func pastLeadsSkippedButLateSeedStillFiresOnce() throws {
        let r = try #require(seed([], ev(1, now + 3 * min)).put.first)
        #expect(r.next == 1)
        #expect(try #require(P.fireAt(r)) <= now)
    }

    @Test func hiddenCalendarAndAllDayAreSilent() {
        #expect(seed([], ev(1, now + 60 * min, cal: 7), ev(2, now + 60 * min, allDay: true), hidden: [7]).put.isEmpty)
    }

    @Test func unchangedEventKeepsProgress() throws {
        let r = try #require(seed([], ev(1, now + 60 * min)).put.first)
        let progressed = P.fire(r, now: now + 45 * min).1
        let again = seed([progressed], ev(1, now + 60 * min), at: now + 46 * min)
        #expect(again.put.isEmpty)
        #expect(again.delete.isEmpty)
    }

    @Test func movedEventDropsOldOccurrence() throws {
        let r = try #require(seed([], ev(1, now + 60 * min)).put.first)
        let moved = seed([r], ev(1, now + 120 * min))
        #expect(moved.delete == [ReminderRow.Key(eventID: 1, occStart: now + 60 * min)])
        #expect(moved.put.first?.occStart == now + 120 * min)
    }

    @Test func dismissalSurvivesEventIDChurn() throws {
        var r = try #require(seed([], ev(1, now + 60 * min)).put.first)
        r.state = P.cancelled
        let churn = seed([r], ev(99, now + 60 * min))
        let row = try #require(churn.put.first)
        #expect(churn.put.count == 1)
        #expect(row.eventID == 99)
        #expect(row.state == P.cancelled)
        #expect(churn.delete == [ReminderRow.Key(eventID: 1, occStart: now + 60 * min)])
    }

    @Test func twinsInTwoCalendarsRingOnce() {
        #expect(seed([], ev(1, now + 60 * min, cal: 1), ev(2, now + 60 * min, cal: 2)).put.count == 1)
    }

    @Test func snoozeReplacesCascade() throws {
        var r = try #require(seed([], ev(1, now + 60 * min)).put.first)
        r.snoozeAt = now + 10 * min
        #expect(P.fireAt(r) == now + 10 * min)
        let (mode, after) = P.fire(r, now: now + 10 * min)
        #expect(mode == .soon)
        #expect(after.state == P.done)
    }

    @Test func firingAfterStartIsAtStartOrRunning() throws {
        let r = try #require(seed([], ev(1, now + 60 * min, leads: [0])).put.first)
        #expect(P.fire(r, now: now + 60 * min).0 == .atStart)
        #expect(P.fire(r, now: now + 70 * min).0 == .running)
        let (mode, row) = P.fire(r, now: now + 61 * 60 * min)
        #expect(mode == nil)
        #expect(row.state == P.done)
    }

    @Test func recurringMasterSeedsEachOccurrence() {
        let s = seed([], ev(1, now - 86_400_000 + 60 * min, rrule: "FREQ=DAILY"))
        #expect(s.put.count >= 7)
        #expect(s.put.allSatisfy { $0.state == P.armed && $0.occStart > now })
    }

    @Test func snoozeStepsLandBeforeStart() {
        #expect(P.snoozeSteps(minutesUntil: 20) == [1, 5, 10, 15])
    }

    // MARK: iOS

    @Test func scheduleLaysOutWholeCascade() throws {
        let r = try #require(seed([], ev(1, now + 60 * min, leads: [15, 5, 0])).put.first)
        let plan = P.schedule([r], now: now)
        #expect(plan.map(\.fireAt) == [now + 45 * min, now + 55 * min, now + 60 * min])
        #expect(plan.map(\.mode) == [.soon, .soon, .atStart])
        #expect(P.parseNotificationID(plan[0].id) == r.key)
    }

    /// Посев уже пропустил прошедшие звенья; если в прошлом и оставшееся
    /// (до начала 3 минуты, звенья 15 и 5) — одно уведомление сразу.
    @Test func schedulePastLinksCollapseToOneNow() throws {
        let r = try #require(seed([], ev(1, now + 3 * min, leads: [15, 5])).put.first)
        let plan = P.schedule([r], now: now)
        #expect(plan.map(\.fireAt) == [now + 1000])
        #expect(plan.first?.mode == .soon)
    }

    @Test func scheduleRespectsLimitNearestFirst() {
        let rows = seed([], ev(1, now - 86_400_000 + 60 * min, leads: [30, 15, 5, 0], rrule: "FREQ=DAILY")).put
        let plan = P.schedule(rows, now: now, limit: 10)
        #expect(plan.count == 10)
        #expect(plan.map(\.fireAt) == plan.map(\.fireAt).sorted())
    }

    @Test func cancelledAndSnoozedAreRespected() throws {
        var r = try #require(seed([], ev(1, now + 60 * min)).put.first)
        r.state = P.cancelled
        #expect(P.schedule([r], now: now).isEmpty)
        r.state = P.armed
        r.snoozeAt = now + 20 * min
        let plan = P.schedule([r], now: now)
        #expect(plan.map(\.fireAt) == [now + 20 * min])
    }
}
