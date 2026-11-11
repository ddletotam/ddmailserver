import DDMailAPI
import Foundation
import Testing
@testable import DDMailCalendar

struct CalendarIndexTests {
    func cal(_ id: String) -> Calendar {
        var c = Calendar(identifier: .gregorian)
        c.timeZone = TimeZone(identifier: id)!
        return c
    }

    /// Полночь UTC у «весь день» — дата UTC, даже к западу от Гринвича.
    @Test func allDayUtcMidnightIsUtcDate() {
        let ny = cal("America/New_York")
        let msMidnightUTC = ms(cal("UTC").date(from: DateComponents(year: 2026, month: 6, day: 10))!)
        #expect(Occ.dateOf(msMidnightUTC, allDay: true, ny) == Day(2026, 6, 10))
        // Не весь день — та же точка в Нью-Йорке ещё 9 июня.
        #expect(Occ.dateOf(msMidnightUTC, allDay: false, ny) == Day(2026, 6, 9))
    }

    @Test func multiDayEventLandsInEachDayOfItsMonth() {
        let msk = cal("Europe/Moscow")
        let start = ms(msk.date(from: DateComponents(year: 2026, month: 6, day: 29, hour: 22))!)
        let end = ms(msk.date(from: DateComponents(year: 2026, month: 7, day: 2, hour: 10))!)
        let ev = EventDTO(id: 1, calendarID: 1, summary: "Поездка", dtstart: start, dtend: end)
        let june = indexMonth(Month(2026, 6), events: [ev], calendars: [:], hidden: [], calendar: msk)
        let july = indexMonth(Month(2026, 7), events: [ev], calendars: [:], hidden: [], calendar: msk)
        #expect(june.keys.sorted() == [Day(2026, 6, 29), Day(2026, 6, 30)])
        #expect(july.keys.sorted() == [Day(2026, 7, 1), Day(2026, 7, 2)])
    }

    @Test func endingAtMidnightDoesNotSpillIntoNextDay() {
        let msk = cal("Europe/Moscow")
        let start = ms(msk.date(from: DateComponents(year: 2026, month: 6, day: 10, hour: 23))!)
        let end = ms(msk.date(from: DateComponents(year: 2026, month: 6, day: 11))!)
        let idx = indexMonth(Month(2026, 6), events: [EventDTO(id: 1, summary: "x", dtstart: start, dtend: end)], calendars: [:], hidden: [], calendar: msk)
        #expect(idx.keys.sorted() == [Day(2026, 6, 10)])
    }

    @Test func hiddenCalendarIsSkippedAndAllDayFirst() {
        let msk = cal("Europe/Moscow")
        let day = ms(cal("UTC").date(from: DateComponents(year: 2026, month: 6, day: 10))!)
        let nine = ms(msk.date(from: DateComponents(year: 2026, month: 6, day: 10, hour: 9))!)
        let evs = [
            EventDTO(id: 1, calendarID: 1, summary: "Встреча", dtstart: nine, dtend: nine + 3_600_000),
            EventDTO(id: 2, calendarID: 1, summary: "Отпуск", dtstart: day, dtend: day + 86_400_000, allDay: true),
            EventDTO(id: 3, calendarID: 9, summary: "Скрытое", dtstart: nine, dtend: nine + 60_000),
        ]
        let idx = indexMonth(Month(2026, 6), events: evs, calendars: [:], hidden: [9], calendar: msk)
        #expect(idx[Day(2026, 6, 10)]?.map(\.event.summary) == ["Отпуск", "Встреча"])
    }

    @Test func colorsParseAppleOrder() {
        #expect(CalendarColors.parse("#3B82F6") == 0xFF3B_82F6)
        #expect(CalendarColors.parse("#3B82F680") == 0x803B_82F6)
        #expect(CalendarColors.parse("blue") == nil)
        #expect(CalendarColors.of(nil, id: 9) == CalendarColors.fallback[1])
    }

    @Test func eventLinks() {
        #expect(EventLinks.bareField("telemost.yandex.ru/j/123") == "https://telemost.yandex.ru/j/123")
        #expect(EventLinks.bareField("report.docx") == nil)
        #expect(EventLinks.bareField("bob@x.ru") == nil)
        #expect(EventLinks.inText("Созвон: https://meet.x.ru/abc. До встречи") == ["https://meet.x.ru/abc"])
        #expect(EventLinks.inText("см. www.example.com/page") == ["https://www.example.com/page"])
        var ev = EventDTO(id: 1, dtstart: 0)
        ev.location = "meet.example.kz/room"
        ev.description = "Ссылка https://meet.example.kz/room/ и ещё www.x.org"
        #expect(EventLinks.of(ev) == ["https://meet.example.kz/room", "https://www.x.org"])
    }
}
