import Foundation
import Testing
@testable import DDMailCalendar

/// Те же случаи, что `RecurrenceTest.kt` Android и `recurrence.rs` десктопа,
/// плюс стенные часы пояса.
struct RecurrenceTests {
    let utc = TimeZone(identifier: "UTC")!

    func at(_ y: Int, _ mo: Int, _ d: Int, _ h: Int, _ tz: TimeZone) -> Int64 {
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = tz
        return ms(cal.date(from: DateComponents(year: y, month: mo, day: d, hour: h))!)
    }

    var mon0900: Int64 { at(2026, 6, 8, 9, utc) }
    func day(_ n: Int64) -> Int64 { n * 86_400_000 }

    func expand(_ s: Int64, _ e: Int64?, _ rule: String, _ ex: [Int64], _ from: Int64, _ to: Int64, _ tz: TimeZone? = nil) -> [Occurrence] {
        Recurrence.expand(dtstartMs: s, dtendMs: e, rrule: rule, exdates: ex, windowStartMs: from, windowEndMs: to, timeZone: tz ?? utc)
    }

    @Test func nonRecurringInWindow() {
        let occ = expand(mon0900, mon0900 + 3_600_000, "", [], mon0900 - day(1), mon0900 + day(1))
        #expect(occ == [Occurrence(startMs: mon0900, endMs: mon0900 + 3_600_000)])
    }

    @Test func nonRecurringOutOfWindow() {
        #expect(expand(mon0900, nil, "", [], mon0900 + day(5), mon0900 + day(7)).isEmpty)
    }

    @Test func dailyMasterInPastStillFillsWindow() {
        let occ = expand(mon0900, mon0900 + 3_600_000, "FREQ=DAILY", [], mon0900 + day(30), mon0900 + day(31))
        #expect(occ.map(\.startMs) == [mon0900 + day(30)])
    }

    @Test func dailyIntervalAndCount() {
        let occ = expand(mon0900, nil, "FREQ=DAILY;INTERVAL=2;COUNT=3", [], mon0900 - day(1), mon0900 + day(30))
        #expect(occ.map(\.startMs) == [mon0900, mon0900 + day(2), mon0900 + day(4)])
    }

    @Test func weeklyUntilBound() {
        let occ = expand(mon0900, nil, "FREQ=WEEKLY;UNTIL=20260624T090000Z", [], mon0900 - day(1), mon0900 + day(60))
        #expect(occ.count == 3)
        #expect(occ.last?.startMs == mon0900 + day(14))
    }

    @Test func weeklyBydayMulti() {
        let occ = expand(mon0900, nil, "FREQ=WEEKLY;BYDAY=MO,WE,FR", [], mon0900, mon0900 + day(7))
        #expect(occ.map(\.startMs) == [mon0900, mon0900 + day(2), mon0900 + day(4)])
    }

    @Test func exdateRemovesInstance() {
        let occ = expand(mon0900, nil, "FREQ=DAILY;COUNT=3", [mon0900 + day(1)], mon0900 - day(1), mon0900 + day(10))
        #expect(occ.map(\.startMs) == [mon0900, mon0900 + day(2)])
    }

    @Test func monthlyKeepsDayOfMonth() {
        let jan31 = at(2026, 1, 31, 9, utc)
        let end = at(2026, 4, 1, 0, utc)
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = utc
        let months = expand(jan31, nil, "FREQ=MONTHLY", [], jan31, end).map { cal.component(.month, from: date($0.startMs)) }
        #expect(months == [1, 3])
    }

    @Test func yearlyStepsAYear() {
        let s = at(2024, 2, 29, 12, utc)
        let end = at(2029, 1, 1, 0, utc)
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = utc
        let years = expand(s, nil, "FREQ=YEARLY", [], s, end).map { cal.component(.year, from: date($0.startMs)) }
        #expect(years == [2024, 2028])
    }

    /// Понедельник 01:00 по Москве — в UTC это воскресенье; дни недели — по местному времени.
    @Test func bydayUsesLocalWallClock() {
        let msk = TimeZone(identifier: "Europe/Moscow")!
        let s = at(2026, 6, 8, 1, msk)
        let occ = expand(s, nil, "FREQ=WEEKLY;BYDAY=MO", [], s, s + day(15), msk)
        #expect(occ.count == 3)
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = msk
        #expect(occ.allSatisfy { cal.component(.weekday, from: date($0.startMs)) == 2 })
    }

    /// Ежедневное в 09:00 остаётся в 09:00 по обе стороны перехода на летнее время.
    @Test func dailyKeepsWallClockAcrossDst() {
        let berlin = TimeZone(identifier: "Europe/Berlin")!
        let s = at(2026, 3, 27, 9, berlin)
        let occ = expand(s, nil, "FREQ=DAILY;COUNT=4", [], s, s + day(10), berlin)
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = berlin
        #expect(occ.count == 4)
        #expect(occ.allSatisfy { cal.component(.hour, from: date($0.startMs)) == 9 })
    }

    @Test func describeRule() {
        #expect(Recurrence.describe("FREQ=WEEKLY;BYDAY=WE,MO") == "Каждую неделю: пн, ср")
        #expect(Recurrence.describe("RRULE:FREQ=DAILY;INTERVAL=2") == "Каждые 2 дн.")
        #expect(Recurrence.describe("") == "")
    }

    @Test func untilFormats() {
        #expect(Recurrence.parseUntil("20260624") == at(2026, 6, 24, 23, utc) + 59 * 60_000 + 59_000)
        #expect(Recurrence.parseUntil("20260230T000000Z") == nil)
        #expect(Recurrence.parseUntil("bad") == nil)
    }
}
