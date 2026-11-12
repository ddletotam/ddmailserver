import DDMailAPI
import Foundation
import Testing
@testable import DDMailDAV

struct ICalTests {
    static let ics = """
    BEGIN:VCALENDAR\r
    VERSION:2.0\r
    BEGIN:VTIMEZONE\r
    TZID:Europe/Moscow\r
    END:VTIMEZONE\r
    BEGIN:VEVENT\r
    UID:series-1\r
    DTSTART;TZID=Europe/Moscow:20261112T100000\r
    DURATION:PT1H30M\r
    RRULE:FREQ=WEEKLY;BYDAY=TH\r
    EXDATE;TZID=Europe/Moscow:20261119T100000\r
    SUMMARY:Планёрка\\, еженедельная\r
    DESCRIPTION:Строка 1\\nСтрока 2 очень длинная, чтобы её перенесли по правилам сво\r
     рачивания строк\r
    ORGANIZER;CN=Боб:mailto:bob@example.net\r
    ATTENDEE;CN="Я, лично";PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:Me@Example.org\r
    BEGIN:VALARM\r
    TRIGGER:-PT15M\r
    ACTION:DISPLAY\r
    END:VALARM\r
    BEGIN:VALARM\r
    TRIGGER;VALUE=DATE-TIME:20261112T060000Z\r
    END:VALARM\r
    END:VEVENT\r
    BEGIN:VEVENT\r
    UID:series-1\r
    RECURRENCE-ID;TZID=Europe/Moscow:20261126T100000\r
    DTSTART;TZID=Europe/Moscow:20261126T120000\r
    DTEND;TZID=Europe/Moscow:20261126T130000\r
    SUMMARY:Планёрка (перенос)\r
    END:VEVENT\r
    END:VCALENDAR\r

    """

    @Test func eventsWithTimezoneRecurrenceOverridesAlarms() throws {
        let evs = ICal.events(Self.ics, calendarID: -5) { uid, rid in davID("a", uid, rid) }
        #expect(evs.count == 2)
        let m = try #require(evs.first { $0.recurrenceID.isEmpty })
        // 10:00 Москвы = 07:00 UTC.
        #expect(m.dtstart == 1_794_466_800_000)
        #expect(m.dtend == Int64(1_794_466_800_000 + 90 * 60_000))
        #expect(m.rrule == "FREQ=WEEKLY;BYDAY=TH")
        #expect(m.summary == "Планёрка, еженедельная")
        #expect(m.description == "Строка 1\nСтрока 2 очень длинная, чтобы её перенесли по правилам сворачивания строк")
        #expect(m.organizerEmail == "bob@example.net")
        #expect(m.attendees.first?.email == "me@example.org")
        #expect(m.attendees.first?.name == "Я, лично")
        #expect(m.alarmLeads == [15])
        #expect(m.calendarID == -5)
        // Исключения: EXDATE и дата переопределения.
        #expect(Set(m.exdates) == Set<Int64>([1_794_466_800_000 + 7 * 86_400_000, 1_794_466_800_000 + 14 * 86_400_000]))
        let o = try #require(evs.first { !$0.recurrenceID.isEmpty })
        #expect(o.rrule.isEmpty)
        #expect(o.dtstart == 1_794_466_800_000 + 14 * 86_400_000 + 2 * 3600_000)
        #expect(o.id != m.id)
    }

    @Test func allDayAndFloatingAndWindowsZone() throws {
        let ics = "BEGIN:VEVENT\r\nUID:x\r\nDTSTART;VALUE=DATE:20261112\r\nSUMMARY:День\r\nEND:VEVENT\r\n"
        let e = try #require(ICal.events(ics, calendarID: 1) { _, _ in 1 }.first)
        #expect(e.allDay)
        #expect(e.dtstart == 1_794_441_600_000)
        #expect(e.dtend == Int64(1_794_441_600_000 + 86_400_000))
        #expect(ICal.timeZone("Russian Standard Time").identifier == "Europe/Moscow")
        #expect(ICal.timeZone("/mozilla.org/20050126_1/Europe/Berlin").identifier == "Europe/Berlin")
        #expect(ICal.duration("-P1DT2H") == -(86_400 + 7200) * 1000)
    }

    @Test func buildAndMergeKeepOtherFields() throws {
        let built = ICal.build(uid: "u1", summary: "Встреча; важная", description: "", location: "Офис",
                               startMs: 1_794_466_800_000, endMs: 1_794_470_400_000, allDay: false, now: Date(timeIntervalSince1970: 0))
        #expect(built.contains("DTSTART:20261112T070000Z\r\n"))
        #expect(built.contains("SUMMARY:Встреча\\; важная\r\n"))
        let merged = ICal.merge(Self.ics, summary: "Новое", description: "", location: "Зал", startMs: 1_794_470_400_000,
                                endMs: 1_794_474_000_000, allDay: false)
        let evs = ICal.events(merged, calendarID: 1) { u, r in davID(u, r) }
        let m = try #require(evs.first { $0.recurrenceID.isEmpty })
        #expect(m.summary == "Новое")
        #expect(m.location == "Зал")
        #expect(m.description.isEmpty)
        #expect(m.dtend == Int64(1_794_474_000_000))
        #expect(m.rrule == "FREQ=WEEKLY;BYDAY=TH")
        #expect(m.alarmLeads == [15])
        // Переопределение не тронуто.
        #expect(evs.first { !$0.recurrenceID.isEmpty }?.summary == "Планёрка (перенос)")
        #expect(!merged.contains("DURATION"))
        #expect(merged.components(separatedBy: "\r\n").allSatisfy { $0.utf8.count <= 75 })
    }

    @Test func rsvpSetsOnlyMyPartstat() {
        let out = ICal.setPartstat(Self.ics, emails: ["me@example.org"], partstat: "ACCEPTED")
        let m = ICal.events(out, calendarID: 1) { u, r in davID(u, r) }.first { $0.recurrenceID.isEmpty }
        #expect(m?.attendees.first?.partstat == "ACCEPTED")
        #expect(m?.attendees.first?.name == "Я, лично")
    }
}

struct VCardTests {
    @Test func parseFoldedAndBuildMerge() throws {
        let raw = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:c1\r\nFN:Иван\r\n  Петров\r\nitem1.EMAIL;TYPE=INTERNET:ivan@example.org\r\nTEL;TYPE=CELL:+7 900 000-00-00\r\nORG:Рога\\, и копыта;Отдел\r\nPHOTO;VALUE=URI:https://example.org/p.jpg\r\nEND:VCARD\r\n"
        let c = try #require(VCard.parse(raw).first)
        #expect(c.fullName == "Иван Петров")
        #expect(c.emails == ["ivan@example.org"])
        #expect(c.phones == ["+7 900 000-00-00"])
        #expect(c.organization == "Рога, и копыта")
        let merged = VCard.merge(raw, fullName: "Иван П.", emails: ["new@example.org"], phones: [], organization: "", title: "Инженер")
        let m = try #require(VCard.parse(merged).first)
        #expect(m.fullName == "Иван П.")
        #expect(m.emails == ["new@example.org"])
        #expect(m.title == "Инженер")
        #expect(merged.contains("PHOTO;VALUE=URI:https://example.org/p.jpg"))
        #expect(VCard.parse("BEGIN:VCARD\r\nUID:x\r\nEND:VCARD\r\n").isEmpty)
        let built = VCard.build(uid: "u", fullName: "Анна", emails: ["a@example.org"], phones: ["1"], organization: "", title: "")
        #expect(VCard.parse(built).first?.emails == ["a@example.org"])
    }
}

struct DAVXMLTests {
    @Test func collectionsFromMultistatus() throws {
        let xml = """
        <?xml version="1.0"?>
        <d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:ic="http://apple.com/ns/ical/">
          <d:response><d:href>/cal/user/</d:href>
            <d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
          <d:response><d:href>/cal/user/work/</d:href>
            <d:propstat><d:prop><d:resourcetype><d:collection/><c:calendar/></d:resourcetype><d:displayname>Работа</d:displayname>
              <ic:calendar-color>#FF0000FF</ic:calendar-color>
              <d:current-user-privilege-set><d:privilege><d:read/></d:privilege></d:current-user-privilege-set>
            </d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
          <d:response><d:href>/cal/user/tasks/</d:href>
            <d:propstat><d:prop><d:resourcetype><d:collection/><c:calendar/></d:resourcetype>
              <c:supported-calendar-component-set><c:comp name="VTODO"/></c:supported-calendar-component-set>
            </d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
          <d:response><d:href>https://dav.example.org/cal/user/home/</d:href>
            <d:propstat><d:prop><d:resourcetype><d:collection/><c:calendar/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
        </d:multistatus>
        """
        let x = try #require(XMLTree.parse(xml))
        let cols = DAVClient.collections(x, base: URL(string: "https://dav.example.org/cal/user/")!, kind: .calendar)
        #expect(cols.map(\.url.absoluteString) == ["https://dav.example.org/cal/user/work/", "https://dav.example.org/cal/user/home/"])
        #expect(cols[0].name == "Работа")
        #expect(cols[0].canWrite == false)
        #expect(cols[1].canWrite)
        #expect(DAVCalendarSource.color("#FF0000FF") == "#FF0000")
        #expect(davID("a", "b") < 0 && davID("a", "b") == davID("a", "b") && davID("a", "b") != davID("a", "c"))
    }

    @Test func candidatesPreferKnownThenWellKnown() {
        let c = DAVClient.candidates(email: "me@yandex.ru", imapHost: "imap.yandex.ru", kind: .calendar).map(\.absoluteString)
        #expect(c.first == "https://caldav.yandex.ru/")
        #expect(c.contains("https://yandex.ru/.well-known/caldav"))
    }
}
