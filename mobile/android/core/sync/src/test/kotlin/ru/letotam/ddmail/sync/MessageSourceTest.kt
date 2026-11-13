package ru.letotam.ddmail.sync

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class MessageSourceTest {
    private fun h(s: String) = MessageSource.text(MessageSource.headers(s.toByteArray()))

    @Test
    fun headersEndAtBlankLine() {
        assertEquals("From: a@x\r\nSubject: =?utf-8?B?0KLQtdC80LA=?=", h("From: a@x\r\nSubject: =?utf-8?B?0KLQtdC80LA=?=\r\n\r\nтело\r\n"))
        assertEquals("From: a@x", h("From: a@x\n\nbody"))
        // Без тела — всё письмо и есть заголовки.
        assertEquals("From: a@x\r\n", h("From: a@x\r\n"))
    }

    @Test
    fun eightBitNotUtf8IsLatin1() {
        assertEquals("X: é", MessageSource.text(byteArrayOf(0x58, 0x3A, 0x20, 0xE9.toByte())))
        assertEquals("Тема: привет", MessageSource.text("Тема: привет".toByteArray()))
    }

    @Test
    fun previewCutsHugeText() {
        val big = "a".repeat(300 * 1024)
        val p = MessageSource.preview(big)
        assertTrue(p.length < big.length)
        assertTrue(p.endsWith("«Скопировать» берёт всё)"))
        assertEquals("мало", MessageSource.preview("мало"))
    }
}
