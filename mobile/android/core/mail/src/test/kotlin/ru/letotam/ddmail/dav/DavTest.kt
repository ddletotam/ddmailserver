package ru.letotam.ddmail.dav

import kotlinx.coroutines.runBlocking
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.time.ZoneId

/** Порт тестов `caldav_client.rs`/`carddav_client.rs` и то, чего у десктопа нет (пояса, все календари). */
class DavTest {
    private val msk = ZoneId.of("Europe/Moscow")

    @Test
    fun parsesUtcVevent() {
        val e = Ical.events("BEGIN:VEVENT\nUID:abc-1\nSUMMARY:Standup\nDTSTART:20260715T090000Z\nDTEND:20260715T093000Z\nRRULE:FREQ=WEEKLY\nEND:VEVENT").single()
        assertEquals("abc-1", e.uid)
        assertEquals("Standup", e.summary)
        assertFalse(e.allDay)
        assertEquals("FREQ=WEEKLY", e.rrule)
        assertEquals(1_784_106_000_000L, e.dtstart)
        assertTrue(e.dtend!! > e.dtstart)
    }

    @Test
    fun parsesAllDayAsUtcMidnight() {
        val e = Ical.events("BEGIN:VEVENT\nUID:d1\nSUMMARY:Holiday\nDTSTART;VALUE=DATE:20260101\nEND:VEVENT").single()
        assertTrue(e.allDay)
        assertEquals(1_767_225_600_000L, e.dtstart)
    }

    @Test
    fun tzidByOsDatabaseAndVtimezoneFallback() {
        val byOs = Ical.events("BEGIN:VEVENT\nUID:t\nDTSTART;TZID=Europe/Moscow:20260715T120000\nEND:VEVENT", ZoneId.of("UTC")).single()
        assertEquals(1_784_106_000_000L, byOs.dtstart)
        val vcal = "BEGIN:VCALENDAR\nBEGIN:VTIMEZONE\nTZID:Russian Standard Time\nBEGIN:STANDARD\nDTSTART:16010101T000000\nTZOFFSETFROM:+0300\nTZOFFSETTO:+0300\nEND:STANDARD\nEND:VTIMEZONE\n" +
            "BEGIN:VEVENT\nUID:w\nDTSTART;TZID=Russian Standard Time:20260715T120000\nEND:VEVENT\nEND:VCALENDAR"
        assertEquals(1_784_106_000_000L, Ical.events(vcal, ZoneId.of("UTC")).single().dtstart)
        // Плавающее время — пояс телефона, не UTC.
        assertEquals(1_784_106_000_000L, Ical.events("BEGIN:VEVENT\nUID:f\nDTSTART:20260715T120000\nEND:VEVENT", msk).single().dtstart)
    }

    @Test
    fun alarmsAttendeesExdates() {
        val e = Ical.events(
            "BEGIN:VEVENT\nUID:a\nDTSTART:20260715T090000Z\nORGANIZER;CN=Boss:mailto:boss@x.ru\n" +
                "ATTENDEE;CN=\"Me: me\";PARTSTAT=ACCEPTED:mailto:me@x.ru\nEXDATE:20260722T090000Z,20260729T090000Z\n" +
                "BEGIN:VALARM\nTRIGGER:-PT15M\nACTION:DISPLAY\nEND:VALARM\nBEGIN:VALARM\nTRIGGER:-P1D\nEND:VALARM\nEND:VEVENT",
        ).single()
        assertEquals(listOf(1440, 15), e.alarmLeads)
        assertEquals("boss@x.ru", e.organizerEmail)
        assertEquals("Me: me", e.attendees.single().name)
        assertEquals("ACCEPTED", e.attendees.single().partstat)
        assertEquals(2, e.exdates.size)
    }

    @Test
    fun unescapesTextAndUnfolds() {
        val e = Ical.events("BEGIN:VEVENT\nUID:x\nSUMMARY:Line one\\, part\nDESCRIPTION:aaa\n bbb\nDTSTART:20260715T120000Z\nEND:VEVENT").single()
        assertEquals("Line one, part", e.summary)
        assertEquals("aaabbb", e.description)
    }

    @Test
    fun buildsIcalWithEscapedFields() {
        val ics = Ical.build("u1", "Plan, v2", "", "Room; A", 1_700_000_000_000, 1_700_003_600_000, false)
        assertTrue(ics.contains("UID:u1"))
        assertTrue(ics.contains("SUMMARY:Plan\\, v2"))
        assertTrue(ics.contains("LOCATION:Room\\; A"))
        val e = Ical.events(ics).single()
        assertEquals("Plan, v2", e.summary)
        assertEquals("Room; A", e.location)
        assertEquals(1_700_000_000_000L, e.dtstart)
    }

    @Test
    fun mergePreservesRruleAlarmsAndOverrides() {
        val existing = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:keep-me\r\nSUMMARY:Old\r\nRRULE:FREQ=WEEKLY\r\nDTSTART:20260715T090000Z\r\nDTEND:20260715T093000Z\r\n" +
            "BEGIN:VALARM\r\nTRIGGER:-PT5M\r\nDESCRIPTION:alarm\r\nEND:VALARM\r\nEND:VEVENT\r\n" +
            "BEGIN:VEVENT\r\nUID:keep-me\r\nRECURRENCE-ID:20260722T090000Z\r\nSUMMARY:Moved\r\nDTSTART:20260722T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
        val merged = Ical.merge(existing, "New title", "Desc", null, 1_700_000_000_000, null, false, dropEnd = true)
        assertTrue(merged.contains("UID:keep-me"))
        assertTrue(merged.contains("RRULE:FREQ=WEEKLY"))
        assertTrue(merged.contains("SUMMARY:New title"))
        assertFalse(merged.contains("SUMMARY:Old"))
        assertFalse(merged.contains("DTEND:"))
        // Описание будильника не тронуто, а новое описание события добавлено.
        assertTrue(merged.contains("DESCRIPTION:alarm"))
        assertTrue(merged.contains("DESCRIPTION:Desc"))
        // Переопределение повтора — как было.
        assertTrue(merged.contains("SUMMARY:Moved"))
        val evs = Ical.events(merged)
        assertEquals(2, evs.size)
        assertEquals(listOf(5), evs[0].alarmLeads)
    }

    private val multistatus = """<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:CR="urn:ietf:params:xml:ns:carddav">
  <D:response>
    <D:href>/carddav/1/addressbooks/2/a.vcf</D:href>
    <D:propstat>
      <D:prop>
        <D:getetag>"e1"</D:getetag>
        <CR:address-data>BEGIN:VCARD
VERSION:3.0
UID:u-1
FN:Ivan Petrov
EMAIL;TYPE=work:ivan@example.com
ORG:Acme;Sales
END:VCARD</CR:address-data>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
    <D:propstat><D:prop><D:displayname/></D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat>
  </D:response>
</D:multistatus>"""

    @Test
    fun extractsAndParsesOneCard() {
        val r = DavClient.multistatus(multistatus).single()
        assertEquals("\"e1\"", r.text("getetag"))
        val (uid, c) = VCard.parse(r.text("address-data")!!)!!
        assertEquals("u-1", uid)
        assertEquals("Ivan Petrov", c.fullName)
        assertEquals(listOf("ivan@example.com"), c.emails)
        assertEquals("Acme", c.organization)
        // Свойство со статусом 404 — не свойство.
        assertEquals(null, r.prop("displayname"))
    }

    @Test
    fun vcardFoldingAndBuild() {
        val (_, c) = VCard.parse("BEGIN:VCARD\nFN:Long\n  Name\nEMAIL:a@x.ru\nEND:VCARD")!!
        assertEquals("Long Name", c.fullName)
        assertEquals(null, VCard.parse("BEGIN:VCARD\nNOTE:hi\nEND:VCARD"))
        val v = VCard.build("u", "Иван, мл.", listOf("a@x.ru", ""), listOf("+7 900"), "Acme", "")
        assertTrue(v.contains("FN:Иван\\, мл."))
        assertEquals("Иван, мл.", VCard.parse(v)!!.second.fullName)
    }

    @Test
    fun absUrl() {
        assertEquals("https://mail.letotam.ru/principals/lucky/", DavClient.absUrl("https://mail.letotam.ru/caldav/", "/principals/lucky/"))
        assertEquals("https://other.ru/b/", DavClient.absUrl("https://x.ru/a/", "https://other.ru/b/"))
        assertEquals("https://x.ru/a/cal1/", DavClient.absUrl("https://x.ru/a/", "cal1/"))
    }

    @Test
    fun discoversEveryCalendarOfHomeSet() = runBlocking {
        val srv = MockWebServer()
        fun ms(body: String) = MockResponse().setResponseCode(207).setBody("<D:multistatus xmlns:D=\"DAV:\" xmlns:C=\"urn:ietf:params:xml:ns:caldav\" xmlns:A=\"http://apple.com/ns/ical/\">$body</D:multistatus>")
        srv.enqueue(ms("<D:response><D:href>/.well-known/caldav</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype><D:current-user-principal><D:href>/p/lucky/</D:href></D:current-user-principal></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>"))
        srv.enqueue(ms("<D:response><D:href>/p/lucky/</D:href><D:propstat><D:prop><C:calendar-home-set><D:href>/cal/lucky/</D:href></C:calendar-home-set></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>"))
        srv.enqueue(
            ms(
                "<D:response><D:href>/cal/lucky/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>" +
                    "<D:response><D:href>/cal/lucky/work/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/><C:calendar/></D:resourcetype><D:displayname>Работа</D:displayname><A:calendar-color>#ff0000ff</A:calendar-color>" +
                    "<D:current-user-privilege-set><D:privilege><D:read/></D:privilege></D:current-user-privilege-set></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>" +
                    "<D:response><D:href>/cal/lucky/home/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/><C:calendar/></D:resourcetype><D:displayname>Дом</D:displayname></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>",
            ),
        )
        srv.start()
        val c = DavClient(OkHttpClient(), "lucky", "pw")
        val cols = DavPim.collections(c, srv.url("/.well-known/caldav").toString(), "calendar")
        assertEquals(listOf("Работа", "Дом"), cols.map { it.name })
        assertEquals("#ff0000", cols[0].color)
        assertFalse(cols[0].canWrite)
        assertTrue(cols[1].canWrite)
        assertTrue(cols[1].url.endsWith("/cal/lucky/home/"))
        assertTrue(srv.takeRequest().getHeader("Authorization")!!.startsWith("Basic "))
        srv.shutdown()
    }
}
