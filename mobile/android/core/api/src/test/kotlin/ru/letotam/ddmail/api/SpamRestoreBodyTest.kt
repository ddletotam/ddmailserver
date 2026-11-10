package ru.letotam.ddmail.api

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Test

/** Тело `POST /spam/restore`: «Не спам» и «Не спам и доверять отправителю». */
class SpamRestoreBodyTest {
    private val refs = listOf(MessageRef("INBOX", 7, "<a@x>"), MessageRef("INBOX", 9, "<b@x>"))

    @Test
    fun plainRestoreHasNoTrust() {
        val o = Json.parseToJsonElement(restoreSpamBody(refs, trust = false)).jsonObject
        assertFalse("trust" in o)
        val msgs = o["messages"]!!.jsonArray.map { it as JsonObject }
        assertEquals(listOf("7", "9"), msgs.map { it["uid"]!!.jsonPrimitive.content })
        assertEquals(listOf("<a@x>", "<b@x>"), msgs.map { it["message_id"]!!.jsonPrimitive.content })
    }

    @Test
    fun trustRestoreSendsTrueFlag() {
        val o = Json.parseToJsonElement(restoreSpamBody(refs, trust = true)).jsonObject
        assertEquals("true", o["trust"]!!.jsonPrimitive.content)
        assertEquals(2, o["messages"]!!.jsonArray.size)
    }
}
