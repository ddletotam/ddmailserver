package ru.letotam.ddmail.sync

import kotlinx.serialization.json.Json
import org.junit.Assert.assertEquals
import org.junit.Test

class IdentityColorsTest {
    private val doc = Json.parseToJsonElement(
        """
        {"groups":[
          {"id":"g1","name":"Работа","color":"","addresses":[{"email":"me@work.example"}]},
          {"id":"g2","name":"Личное","color":"#B2DFDB","addresses":[{"email":"Me@Home.Example","hidden":true}]},
          {"id":"g3","name":"Битый","color":"зелёный","addresses":[{"email":"x@y.example"}]}
        ],"default_group":"g1"}
        """,
    )
    private val colors = IdentityColors.parse(doc)

    @Test fun emptyColorTakesPaletteByIndex() =
        assertEquals(IdentityColors.PASTEL[0], colors.colorOf("me@work.example"))

    @Test fun ownColorAndCaseInsensitive() =
        assertEquals(0xFFB2DFDB, colors.colorOf(" ME@home.example "))

    @Test fun badColorFallsBackToPalette() =
        assertEquals(IdentityColors.PASTEL[2], colors.colorOf("x@y.example"))

    @Test fun taggedAddressTakesBase() =
        assertEquals(IdentityColors.PASTEL[0], colors.colorOf("me+list@work.example"))

    @Test fun unknownIsGrey() {
        assertEquals(IdentityColors.UNKNOWN, colors.colorOf("other@work.example"))
        assertEquals(IdentityColors.UNKNOWN, IdentityColors.parse(null).colorOf("me@work.example"))
    }
}
