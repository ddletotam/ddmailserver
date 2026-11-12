package ru.letotam.ddmail.mime

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import ru.letotam.ddmail.api.OutgoingAttachment
import ru.letotam.ddmail.api.SendRequest
import ru.letotam.ddmail.smtp.Smtp
import java.util.Base64

class MimeParseTest {
    private fun b64(b: ByteArray) = Base64.getMimeEncoder().encodeToString(b)

    @Test
    fun windows1251QuotedPrintableAndRfc2047Headers() {
        val text = "Привет, мир".toByteArray(charset("windows-1251"))
        val qp = text.joinToString("") { "=%02X".format(it) }
        val raw = """From: =?utf-8?B?${b64("Иван Петров".toByteArray())}?= <Ivan@X.ru>
To: me@a.ru, "Bob" <bob@x.ru>
Subject: =?koi8-r?B?${b64("Тема".toByteArray(charset("koi8-r")))}?=
Date: Tue, 2 Jan 2024 03:04:05 +0300
Message-ID: <abc@x.ru>
References: <r1@x> <r2@x>
Content-Type: text/plain; charset=windows-1251
Content-Transfer-Encoding: quoted-printable

$qp
""".replace("\n", "\r\n").toByteArray()
        val b = MimeParse.body(raw, 9, "INBOX", listOf("me@a.ru"))
        assertEquals("Привет, мир", b.text?.trim())
        assertEquals("Тема", b.subject)
        assertEquals("Иван Петров", b.from)
        assertEquals("ivan@x.ru", b.fromAddr)
        assertEquals(listOf("me@a.ru", "Bob <bob@x.ru>"), b.to)
        assertEquals(1704153845L, b.dateTs)
        assertEquals("abc@x.ru", b.messageId)
        assertEquals(listOf("r1@x", "r2@x"), b.references)
        assertEquals(false, b.isOutgoing)
    }

    @Test
    fun koi8rBase64HtmlAndNoCharsetHeuristics() {
        val html = "<p>Здравствуйте</p>".toByteArray(charset("koi8-r"))
        val raw = """From: me@a.ru
Content-Type: multipart/alternative; boundary="b"

--b
Content-Type: text/plain

${String("Без кодировки".toByteArray(charset("windows-1251")), Charsets.ISO_8859_1)}
--b
Content-Type: text/html; charset="KOI8-R"
Content-Transfer-Encoding: base64

${b64(html)}
--b--
""".replace("\n", "\r\n").toByteArray(Charsets.ISO_8859_1)
        val b = MimeParse.body(raw, 1, "INBOX", listOf("me@a.ru"))
        assertEquals("<p>Здравствуйте</p>", b.html)
        assertEquals("Без кодировки", b.text?.trim())
        assertTrue(b.isOutgoing)
    }

    @Test
    fun attachmentsIndexesCidAndRfc2231() {
        val png = byteArrayOf(1, 2, 3, 4)
        val raw = """From: bob@x.ru
Content-Type: multipart/mixed; boundary="m"

--m
Content-Type: multipart/related; boundary="r"

--r
Content-Type: text/html; charset=utf-8

<img src="cid:logo@x">
--r
Content-Type: image/png
Content-ID: <logo@x>
Content-Transfer-Encoding: base64

${b64(png)}
--r--
--m
Content-Type: application/pdf
Content-Disposition: attachment; filename*=UTF-8''%D0%9E%D1%82%D1%87%D1%91%D1%82.pdf
Content-Transfer-Encoding: base64

${b64(byteArrayOf(9, 9))}
--m--
""".replace("\n", "\r\n").toByteArray()
        val b = MimeParse.body(raw, 1, "INBOX", emptyList())
        // Картинка по cid: спрятана, документ — виден, с индексом листа.
        assertEquals(1, b.attachments.size)
        val a = b.attachments[0]
        assertEquals("Отчёт.pdf", a.filename)
        assertEquals(1, a.index)
        assertEquals(2L, a.size)
        val (bytes, type) = MimeParse.part(raw, "<logo@x>")!!
        assertEquals("image/png", type)
        assertTrue(bytes.contentEquals(png))
        val (pdf, mime, name) = MimeParse.attachment(raw, 1)!!
        assertEquals("application/pdf", mime)
        assertEquals("Отчёт.pdf", name)
        assertEquals(2, pdf.size)
        assertNull(MimeParse.part(raw, "nope@x"))
    }

    @Test
    fun outgoingStructureLikeDesktop() {
        val req = SendRequest(
            from = "\"Иван\" <me@a.ru>",
            to = listOf("bob@x.ru"),
            cc = listOf("Кэрол <carol@y.ru>"),
            subject = "Re: Тема",
            html = "<div>привет<img src=\"cid:i1\"></div>",
            text = "привет",
            inReplyTo = "orig@x.ru",
            references = "root@x.ru orig@x.ru",
            attachments = listOf(
                OutgoingAttachment("pic.png", "image/png", byteArrayOf(1), contentId = "i1"),
                OutgoingAttachment("Документ.txt", "text/plain", "x".toByteArray()),
            ),
        )
        val out = Smtp.build(req)
        assertTrue(out.messageId.endsWith("@a.ru"))
        val raw = String(out.bytes(), Charsets.UTF_8)
        assertTrue(raw.contains("Message-ID: <${out.messageId}>"))
        assertTrue(raw.contains("In-Reply-To: <orig@x.ru>"))
        assertTrue(raw.contains("References: <root@x.ru> <orig@x.ru>"))
        assertTrue(raw.contains("multipart/mixed"))
        assertTrue(raw.contains("multipart/related"))
        assertTrue(raw.contains("multipart/alternative"))
        assertTrue(!raw.contains("Bcc", ignoreCase = true))
        // Разбирается обратно тем же парсером: тело, вложение, cid.
        val back = MimeParse.body(out.bytes(), 1, "Sent", listOf("me@a.ru"))
        assertEquals("Иван", back.from)
        assertEquals("Re: Тема", back.subject)
        assertTrue(back.isOutgoing)
        assertEquals(listOf("Документ.txt"), back.attachments.map { it.filename })
        assertNotNull(MimeParse.part(out.bytes(), "i1"))
        assertEquals(out.messageId, back.messageId)
        assertEquals(2, out.recipients.size)
    }

    @Test
    fun dates() {
        assertEquals(1704153845L, MailDates.parse("Tue, 2 Jan 2024 03:04:05 +0300 (MSK)"))
        assertEquals(1704153845L, MailDates.parse("2 Jan 24 00:04:05 GMT"))
        assertEquals(1704153840L, MailDates.parse("2 Jan 2024 03:04 MSK"))
        assertEquals(0L, MailDates.parse("вчера"))
        assertEquals(1704153845L, MailDates.parse("02-Jan-2024 00:04:05 +0000"))
    }
}
