package ru.letotam.ddmail.sync

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertSame
import org.junit.Assert.assertTrue
import org.junit.Test

/** Векторы — те же, что у `identities.rs` десктопа (§1в) и `IdentityGroupsTests` iOS. */
class IdentityGroupsTest {
    private val doc = IdentityGroups.parse(
        Json.parseToJsonElement(
            """
            {"default_group":"g2","sig_extra":1,"groups":[
              {"id":"g1","name":"Работа","color":"#E3F2FD","sender":"boss@work.example","strict":true,
               "addresses":[{"email":"me@work.example"},{"email":"boss@work.example"}]},
              {"id":"g2","name":"","color":"","sender":"","addresses":[{"email":"me@home.example"},{"email":"old@home.example","hidden":true}]},
              {"id":"g3","name":"Рассылки","color":"","sender":"lists@home.example","addresses":[{"email":"lists@home.example"}]}
            ]}
            """,
        ),
    )

    @Test fun replyFromGroupSenderEvenIfReceivedOnOtherAddress() {
        assertEquals("boss@work.example", doc.replySender("me@work.example"))
        assertEquals("boss@work.example", doc.replySender("ME@work.example"))
        // user+tag@ без своей записи — группа user@.
        assertEquals("boss@work.example", doc.replySender("me+news@work.example"))
    }

    @Test fun notMineGoesFromDefaultGroup() {
        // sender пустой — первый адрес группы.
        assertEquals("me@home.example", doc.replySender("stranger@x.example"))
        assertEquals("me@home.example", doc.replySender(""))
        assertEquals("me@home.example", doc.newMailSender())
        assertEquals("me@home.example", doc.autoSender(null))
    }

    @Test fun strictDefaultIsSkippedForForeignMail() {
        val strictDefault = doc.withDefault("g1")
        assertEquals("boss@work.example", strictDefault.newMailSender())
        // Письмо не на мой адрес — первая нестрогая.
        assertEquals("me@home.example", strictDefault.replySender("x@y.example"))
        val allStrict = IdentityGroups.parse(
            Json.parseToJsonElement("""{"groups":[{"id":"g1","strict":true,"addresses":[{"email":"a@b.example"}]}]}"""),
        )
        assertNull(allStrict.replySender("x@y.example"))
    }

    @Test fun strictGroupOnlyForOwnMailOrNew() {
        assertTrue(doc.allowed("boss@work.example", null))
        assertTrue(doc.allowed("boss@work.example", "me@work.example"))
        assertFalse(doc.allowed("boss@work.example", "me@home.example"))
        assertFalse(doc.allowed("boss@work.example", ""))
        assertTrue(doc.allowed("me@home.example", "me@work.example"))
        assertTrue(doc.allowed("unknown@x.example", "me@work.example"))
    }

    @Test fun hiddenNotOffered() {
        assertFalse(doc.offered("old@home.example", null))
        assertTrue(doc.offered("me@home.example", null))
        assertFalse(doc.offered("me@work.example", "me@home.example"))
    }

    @Test fun defaultFallsBackToFirstGroup() {
        assertEquals("g1", doc.withDefault("nope").defaultGroup?.id)
        assertEquals("g2", doc.defaultGroup?.id)
    }

    @Test fun titleAndSender() {
        assertEquals("Работа · boss@work.example", doc.groups[0].title)
        assertEquals("me@home.example", doc.groups[1].title)
    }

    @Test fun withDefaultKeepsOtherFields() {
        val j = doc.withDefault("g3").json
        assertEquals("g3", j["default_group"]!!.jsonPrimitive.content)
        assertEquals("1", j["sig_extra"]!!.jsonPrimitive.content)
        assertEquals(doc.json["groups"], j["groups"])
    }

    @Test fun mergeAddsNewAddressesAsOwnGroups() {
        val merged = doc.merged(
            listOf(
                IdentityGroups.Source("Me@Home.example"),
                IdentityGroups.Source("new@mail.example", "Новый"),
                IdentityGroups.Source("not an email"),
            ),
        )
        assertEquals(4, merged.groups.size)
        val g = merged.groups.last()
        assertEquals("g4", g.id)
        assertEquals("Новый", g.name)
        assertEquals("new@mail.example", g.sender)
        // Первый незанятый цвет палитры (#E3F2FD занят группой g1).
        assertEquals("#FFE4E1", g.color)
        assertEquals("g2", merged.defaultGroupId)
        assertSame(merged, merged.merged(listOf(IdentityGroups.Source("new@mail.example"))))
    }

    @Test fun mergeIntoEmptySetsDefault() {
        val m = IdentityGroups.EMPTY.merged(listOf(IdentityGroups.Source("a@b.example"), IdentityGroups.Source("c@d.example")))
        assertEquals(listOf("g1", "g2"), m.groups.map { it.id })
        assertEquals("g1", m.defaultGroupId)
        assertEquals(listOf("#FFE4E1", "#E8F5E9"), m.groups.map { it.color })
        // Цвета строк — из того же документа.
        assertEquals(IdentityColors.PASTEL[0], m.colors().colorOf("a@b.example"))
    }

    @Test fun emptyOrBrokenDoc() {
        assertTrue(IdentityGroups.parse(null).isEmpty)
        assertTrue(IdentityGroups.parse(JsonObject(emptyMap())).isEmpty)
        assertNull(IdentityGroups.EMPTY.newMailSender())
    }
}
