package ru.letotam.ddmail.sync

import kotlinx.coroutines.runBlocking
import okhttp3.OkHttpClient
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import ru.letotam.ddmail.api.AddressBookDto
import ru.letotam.ddmail.api.CalendarDto
import ru.letotam.ddmail.api.ContactDto
import ru.letotam.ddmail.api.ContactWriteRequest
import ru.letotam.ddmail.api.EventCreateRequest
import ru.letotam.ddmail.api.EventDto
import ru.letotam.ddmail.api.EventPatchRequest
import ru.letotam.ddmail.api.Identity
import ru.letotam.ddmail.api.PimApi

class PimHubTest {
    @Test
    fun externalIdsAreNegativeAndStable() {
        val a = PimHub.negId("me@imap.x.ru\nhttps://dav/cal/\nuid-1\n")
        assertTrue(a < 0)
        assertEquals(a, PimHub.negId("me@imap.x.ru\nhttps://dav/cal/\nuid-1\n"))
        assertNotEquals(a, PimHub.negId("other@imap.x.ru\nhttps://dav/cal/\nuid-1\n"))
        assertTrue(PimHub.negId("") < 0)
    }

    /** Положительные id — серверу ddmail, без внешних учёток всё как было. */
    @Test
    fun serverIdsRouteToDdmail() = runBlocking {
        val calls = mutableListOf<String>()
        val server = object : PimApi {
            override suspend fun identities() = listOf(Identity("me@a.ru"))
            override suspend fun calendars() = listOf(CalendarDto(7, "Мой"))
            override suspend fun calendarEvents(fromMs: Long, toMs: Long) = listOf(EventDto(1, 7))
            override suspend fun createEvent(req: EventCreateRequest) = "".also { calls += "create" }
            override suspend fun patchEvent(id: Long, req: EventPatchRequest) = "".also { calls += "patch $id" }
            override suspend fun deleteEvent(id: Long) { calls += "delete $id" }
            override suspend fun rsvp(id: Long, partstat: String) { calls += "rsvp $id" }
            override suspend fun contacts(limit: Int) = listOf(ContactDto(3, "Иван"))
            override suspend fun addressBooks() = listOf(AddressBookDto(2))
            override suspend fun createContact(req: ContactWriteRequest) { calls += "contact" }
            override suspend fun patchContact(id: Long, req: ContactWriteRequest) { calls += "pc $id" }
            override suspend fun deleteContact(id: Long) { calls += "dc $id" }
        }
        val hub = PimHub(server, emptyList(), { null }, OkHttpClient()) {}
        assertEquals(listOf(7L), hub.calendars().map { it.id })
        assertEquals(1, hub.calendarEvents(0, 1).size)
        hub.patchEvent(1, EventPatchRequest())
        hub.deleteEvent(1)
        hub.rsvp(1, "ACCEPTED")
        assertEquals(listOf("patch 1", "delete 1", "rsvp 1"), calls)
        assertEquals(1, hub.contacts().size)
    }
}
