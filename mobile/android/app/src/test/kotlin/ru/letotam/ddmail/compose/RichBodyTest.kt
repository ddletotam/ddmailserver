package ru.letotam.ddmail.compose

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import ru.letotam.ddmail.api.Identity

/** Тело письма — как `Editor::html` / `plain_text` десктопа (§3а). */
class RichBodyTest {
    private fun t(s: String, b: Boolean = false, i: Boolean = false, u: Boolean = false, link: String? = null) =
        RichSeg.Text(s, b, i, u, link)

    private val ids = listOf(Identity("me@a.ru", "Я", isDefault = true))

    @Test
    fun paragraphsAndEmptyLinesAsAuthorPut() {
        val segs = listOf(t("раз\n\n\nдва"))
        assertEquals("<div>раз</div><div><br></div><div><br></div><div>два</div>", RichBody.html(segs))
        assertEquals("раз\n\n\nдва", RichBody.plain(segs))
    }

    @Test
    fun stylesNestLinkOutside() {
        val segs = listOf(t("a "), t("жир", b = true), t(" "), t("всё", b = true, i = true, u = true), t(" "), t("тут", u = true, link = "example.com"))
        assertEquals(
            "<div>a <b>жир</b> <b><i><u>всё</u></i></b> <a href=\"http://example.com\"><u>тут</u></a></div>",
            RichBody.html(segs),
        )
    }

    @Test
    fun imageIsOwnBlockWithoutExtraEmptyParagraphs() {
        val segs = listOf(t("до\n"), RichSeg.Image("img1.5@ddmail"), t("\nпосле"))
        assertEquals(
            "<div>до</div><div><img src=\"cid:img1.5@ddmail\" style=\"max-width:100%\"></div><div>после</div>",
            RichBody.html(segs),
        )
        assertEquals("до\n\nпосле", RichBody.plain(segs))
        // Письмо из одной картинки.
        assertEquals("<div><img src=\"cid:x@d\" style=\"max-width:100%\"></div>", RichBody.html(listOf(RichSeg.Image("x@d"), t("\n"))))
    }

    @Test
    fun autolinksEscapingAndTrailingPunctuation() {
        val html = RichBody.html(listOf(t("см. https://a.ru/x?y=1&z=2, и www.b.ru. <тег>")))
        assertEquals(
            "<div>см. <a href=\"https://a.ru/x?y=1&amp;z=2\">https://a.ru/x?y=1&amp;z=2</a>, и " +
                "<a href=\"http://www.b.ru\">www.b.ru</a>. &lt;тег&gt;</div>",
            html,
        )
        // С середины слова — не ссылка.
        assertEquals("<div>foohttp://x.ru</div>", RichBody.html(listOf(t("foohttp://x.ru"))))
    }

    @Test
    fun unsafeSchemesDoNotReachTheLetter() {
        assertEquals("https://a.ru", RichBody.safeHref("https://a.ru"))
        assertEquals("mailto:me@a.ru", RichBody.safeHref("me@a.ru"))
        assertTrue(!RichBody.safeHref("javascript:alert(1)").startsWith("javascript"))
    }

    @Test
    fun draftWithInlineImageSendsHtmlAndCidAttachment() {
        val img = PendingAttachment("img1.png", "image/png", byteArrayOf(1, 2), cid = "img1.1@ddmail")
        val segs = listOf(t("смотри", b = true), t("\n"), RichSeg.Image("img1.1@ddmail"))
        val d = Draft(text = RichBody.plain(segs), html = RichBody.html(segs), inline = listOf(img), to = "x@b.ru")
        val r = build(Branch.New, d, ids) as Built.Ok
        assertTrue(r.request.html.startsWith("<div><b>смотри</b></div><div><img src=\"cid:img1.1@ddmail\""))
        assertEquals("смотри\n", r.request.text)
        assertEquals("img1.1@ddmail", r.request.attachments.single().contentId)
        // Одна картинка без слов — тоже письмо.
        assertTrue(Draft(inline = listOf(img)).canSend)
        // Заглушка — с data:-URI вместо cid:.
        assertTrue(stubHtml(r.request.html, d.inline).contains("src=\"data:image/png;base64,AQI=\""))
    }

    @Test
    fun plainDraftGetsDesktopHtml() {
        val r = build(Branch.New, Draft(text = "a <b>\nc", to = "x@b.ru"), ids) as Built.Ok
        assertEquals("<div>a &lt;b&gt;</div><div>c</div>", r.request.html)
    }

    @Test
    fun draftEpochSurvivesEditsButNotResets() {
        val d = Draft(text = "x")
        assertEquals(d.epoch, d.copy(text = "xy").epoch)
        assertTrue(d.reloaded().epoch != d.epoch)
        val c = d.copy(to = "a@b.ru").bodyCleared()
        assertEquals("a@b.ru", c.to)
        assertTrue(c.isBlank() && c.epoch != d.epoch)
    }

    @Test
    fun newMessageTargetIsToPlusCc() {
        val r = build(Branch.New, Draft(text = "x", to = "Bob <Bob@X.ru>", cc = "c@y.ru"), ids) as Built.Ok
        assertEquals(setOf("bob@x.ru", "c@y.ru"), targetOf(r.request))
    }
}
