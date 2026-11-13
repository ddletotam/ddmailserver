package ru.letotam.ddmail.api

import kotlinx.coroutines.test.runTest
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test

/** Запись задач: POST/PATCH/DELETE /tasks — пути, тела, разбор ответа. */
class TaskApiTest {
    private val server = MockWebServer().apply { start() }

    private object Tokens : TokenSource {
        override suspend fun current() = "t"
        override suspend fun update(token: String) {}
    }

    private val api = SessionApi(server.url("/").toString().trimEnd('/'), OkHttpClient(), Tokens)

    @After
    fun stop() = server.shutdown()

    private val taskJson = """{"id":101,"calendar_id":1,"uid":"x@ddmailserver","summary":"Оплатить","due":1794528000000,
        |"all_day":true,"status":"NEEDS-ACTION","completed":false,"can_write":true,"calendar_name":"Напоминания"}""".trimMargin()

    @Test
    fun createPostsAndParsesTask() = runTest {
        server.enqueue(MockResponse().setResponseCode(201).setBody(taskJson))
        val t = api.createTask(TaskCreateRequest(1, "Оплатить", "до пятницы", 1794528000000, allDay = true))
        assertEquals(101L, t.id)
        assertEquals("Напоминания", t.calendarName)
        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/api/desktop/v1/tasks", req.path)
        val o = Json.parseToJsonElement(req.body.readUtf8()).jsonObject
        assertEquals("1", o["calendar_id"]!!.jsonPrimitive.content)
        assertEquals("Оплатить", o["summary"]!!.jsonPrimitive.content)
        assertEquals("до пятницы", o["description"]!!.jsonPrimitive.content)
        assertEquals("1794528000000", o["due"]!!.jsonPrimitive.content)
        assertEquals("true", o["all_day"]!!.jsonPrimitive.content)
    }

    @Test
    fun createWithoutDueSendsNoDue() = runTest {
        server.enqueue(MockResponse().setResponseCode(201).setBody(taskJson))
        api.createTask(TaskCreateRequest(1, "Без срока"))
        val o = Json.parseToJsonElement(server.takeRequest().body.readUtf8()).jsonObject
        assertFalse("due" in o)
    }

    @Test
    fun patchSendsOnlyGivenFields() = runTest {
        server.enqueue(MockResponse().setBody(taskJson))
        api.patchTask(101, TaskPatchRequest(summary = "Новое", due = 0))
        val req = server.takeRequest()
        assertEquals("PATCH", req.method)
        assertEquals("/api/desktop/v1/tasks/101", req.path)
        val o = Json.parseToJsonElement(req.body.readUtf8()).jsonObject
        assertEquals(setOf("summary", "due"), o.keys)
        assertEquals("0", o["due"]!!.jsonPrimitive.content)
    }

    @Test
    fun deleteUsesDelete() = runTest {
        server.enqueue(MockResponse().setResponseCode(204))
        api.deleteTask(101)
        val req = server.takeRequest()
        assertEquals("DELETE", req.method)
        assertEquals("/api/desktop/v1/tasks/101", req.path)
    }

    @Test
    fun readOnlyIsServerErrorNotLogout() = runTest {
        server.enqueue(MockResponse().setResponseCode(403).setBody("""{"error":"calendar is read-only"}"""))
        try {
            api.createTask(TaskCreateRequest(3, "x"))
            fail("должно было упасть")
        } catch (e: ApiException.Server) {
            assertEquals(403, e.code)
        }
    }

    @Test
    fun calendarsTellTaskLists() = runTest {
        server.enqueue(
            MockResponse().setBody(
                """[{"id":1,"name":"Напоминания","source_type":"caldav","can_write":true,"components":["VTODO"]},
                   {"id":2,"name":"Дом","source_type":"caldav","can_write":true,"components":["VEVENT"]},
                   {"id":3,"name":"Праздники","source_type":"ics_url","can_write":true,"components":["VEVENT","VTODO"]},
                   {"id":4,"name":"Старый сервер","source_type":"caldav","can_write":true}]""",
            ),
        )
        val cals = api.calendars()
        assertEquals(listOf(1L), cals.filter { it.acceptsTasks }.map { it.id })
        assertTrue(cals.last().components.isEmpty())
    }
}
