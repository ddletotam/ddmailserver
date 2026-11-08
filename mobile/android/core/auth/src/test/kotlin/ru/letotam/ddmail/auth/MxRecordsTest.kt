package ru.letotam.ddmail.auth

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class MxRecordsTest {
    private fun name(vararg labels: String): ByteArray =
        labels.flatMap { listOf(it.length.toByte()) + it.toByteArray().toList() }.toByteArray() + 0

    private fun u16(v: Int) = byteArrayOf((v shr 8).toByte(), v.toByte())

    /** Ответ на MX example.org: две записи, вторая — со сжатием имени. */
    private fun answer(): ByteArray {
        val header = u16(0x1234) + u16(0x8180) + u16(1) + u16(2) + u16(0) + u16(0)
        val qname = name("example", "org") // смещение 12
        val question = qname + u16(15) + u16(1)
        val rr1data = u16(20) + name("mx2", "example", "org")
        val rr1 = u16(0xc00c) + u16(15) + u16(1) + byteArrayOf(0, 0, 0x0e, 0x10) + u16(rr1data.size) + rr1data
        // «mx1» + указатель на example.org в вопросе
        val rr2data = u16(10) + byteArrayOf(3) + "mx1".toByteArray() + u16(0xc00c)
        val rr2 = u16(0xc00c) + u16(15) + u16(1) + byteArrayOf(0, 0, 0x0e, 0x10) + u16(rr2data.size) + rr2data
        return header + question + rr1 + rr2
    }

    @Test
    fun parsesSortedWithCompression() {
        assertEquals(listOf(10 to "mx1.example.org", 20 to "mx2.example.org"), parseMxAnswer(answer()))
    }

    @Test
    fun brokenOrErrorAnswerIsEmpty() {
        assertTrue(parseMxAnswer(byteArrayOf(1, 2, 3)).isEmpty())
        assertTrue(parseMxAnswer(answer().copyOf(40)).isEmpty())
        val nx = answer().also { it[3] = 0x83.toByte() } // NXDOMAIN
        assertTrue(parseMxAnswer(nx).isEmpty())
    }
}
