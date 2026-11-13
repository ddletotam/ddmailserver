import Foundation
import Testing
@testable import DDMailCalendar

/// Перенос и растяжение событий на шкале (Android §12, десктоп §5и).
struct EventDragTests {
    let msk: Calendar = {
        var c = Calendar(identifier: .gregorian)
        c.timeZone = TimeZone(identifier: "Europe/Moscow")!
        return c
    }()

    func at(_ c: Calendar, _ y: Int, _ m: Int, _ d: Int, _ h: Int, _ min: Int = 0) -> Int64 {
        Int64(c.date(from: DateComponents(year: y, month: m, day: d, hour: h, minute: min))!.timeIntervalSince1970 * 1000)
    }

    @Test func snapsToFifteenMinutes() {
        // Час — 52 точки: 13 точек = 15 минут.
        #expect(EventDrag.snap(0, hourHeight: 52) == 0)
        #expect(EventDrag.snap(6, hourHeight: 52) == 0)
        #expect(EventDrag.snap(7, hourHeight: 52) == 15)
        #expect(EventDrag.snap(26, hourHeight: 52) == 30)
        #expect(EventDrag.snap(-20, hourHeight: 52) == -30)
        #expect(EventDrag.snap(10, hourHeight: 0) == 0)
    }

    @Test func edgesResizeMiddleMoves() {
        #expect(EventDrag.kind(at: 3, height: 104) == .top)
        #expect(EventDrag.kind(at: 50, height: 104) == .move)
        #expect(EventDrag.kind(at: 100, height: 104) == .bottom)
        // Короткое событие (15 минут): кромки узкие, середина — перенос.
        #expect(EventDrag.kind(at: 6.5, height: 13) == .move)
        #expect(EventDrag.kind(at: 1, height: 13) == .top)
    }

    @Test func moveKeepsDurationAndCrossesDays() {
        let s = at(msk, 2026, 11, 13, 10), e = at(msk, 2026, 11, 13, 11, 30)
        let r = EventDrag.apply(.move, start: s, end: e, minutes: 45, days: 2, calendar: msk)
        #expect(r.start == at(msk, 2026, 11, 15, 10, 45))
        #expect(r.end == at(msk, 2026, 11, 15, 12, 15))
    }

    /// Через переход на летнее время — тот же час по стенным часам.
    @Test func moveAcrossDstKeepsWallClock() {
        var berlin = Calendar(identifier: .gregorian)
        berlin.timeZone = TimeZone(identifier: "Europe/Berlin")!
        let s = at(berlin, 2026, 3, 28, 10), e = at(berlin, 2026, 3, 28, 11)
        let r = EventDrag.apply(.move, start: s, end: e, minutes: 0, days: 1, calendar: berlin)
        #expect(r.start == at(berlin, 2026, 3, 29, 10))
        #expect(r.end == at(berlin, 2026, 3, 29, 11))
    }

    @Test func resizeNeverShorterThanStep() {
        let s = at(msk, 2026, 11, 13, 10), e = at(msk, 2026, 11, 13, 11)
        #expect(EventDrag.apply(.bottom, start: s, end: e, minutes: 30, calendar: msk) == (s, at(msk, 2026, 11, 13, 11, 30)))
        #expect(EventDrag.apply(.bottom, start: s, end: e, minutes: -90, calendar: msk) == (s, at(msk, 2026, 11, 13, 10, 15)))
        #expect(EventDrag.apply(.top, start: s, end: e, minutes: -15, calendar: msk) == (at(msk, 2026, 11, 13, 9, 45), e))
        #expect(EventDrag.apply(.top, start: s, end: e, minutes: 120, calendar: msk) == (at(msk, 2026, 11, 13, 10, 45), e))
    }
}
