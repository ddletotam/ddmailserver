package ru.letotam.ddmail.api

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Test

/** Тело `POST /spam/blacklist-and-purge` — то же, что шлёт десктоп. */
class BlacklistAndPurgeBodyTest {
    @Test
    fun carriesScopeIdsAndFallback() {
        val o = Json.parseToJsonElement(blacklistAndPurgeBody("domain", listOf(7, 9), "a@x.ru")).jsonObject
        assertEquals("domain", o["scope"]!!.jsonPrimitive.content)
        assertEquals("a@x.ru", o["address"]!!.jsonPrimitive.content)
        assertEquals(listOf("7", "9"), o["message_ids"]!!.jsonArray.map { it.jsonPrimitive.content })
    }

    @Test
    fun outcomeParsesServerReply() {
        val r = DdmailApi.json.decodeFromString(
            PurgeOutcome.serializer(),
            """{"deleted":12,"rule_type":"domain","rule_value":"spam.example","rule_count":2}""",
        )
        assertEquals(PurgeOutcome(12, "domain", "spam.example", 2), r)
    }
}
