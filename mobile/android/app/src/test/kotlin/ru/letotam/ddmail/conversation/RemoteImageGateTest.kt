package ru.letotam.ddmail.conversation

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** Пропуск `/remote` перехватчиком WebView: обычная беседа и спам (контракт §5, §8). */
class RemoteImageGateTest {
    private val img = mapOf("Accept" to "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
    private val css = mapOf("Accept" to "text/css,*/*;q=0.1")
    private val font = mapOf("Accept" to "*/*")

    @Test
    fun hamIsUnchanged() {
        val g = RemoteImageGate()
        g.open(spam = false, uids = listOf(1L))
        assertTrue(g.permits(1, img))
        assertTrue(g.permits(null, emptyMap()))
        assertTrue(g.permits(99, font))
    }

    @Test
    fun spamNeedsExplicitAllowPerMessage() {
        val g = RemoteImageGate()
        g.open(spam = true, uids = listOf(1L, 2L))
        assertFalse(g.permits(1, img))
        g.allow(1)
        assertTrue(g.permits(1, img))
        // Другое письмо той же беседы — своё нажатие.
        assertFalse(g.permits(2, img))
        // Без uid и с чужим uid — нет.
        assertFalse(g.permits(null, img))
        g.allow(42)
        assertFalse(g.permits(42, img))
    }

    @Test
    fun spamAllowsOnlyImages() {
        val g = RemoteImageGate()
        g.open(spam = true, uids = listOf(1L))
        g.allow(1)
        assertFalse(g.permits(1, css))
        assertFalse(g.permits(1, font))
        assertFalse(g.permits(1, emptyMap()))
        assertTrue(g.permits(1, mapOf("sec-fetch-dest" to "image", "Accept" to "*/*")))
        assertFalse(g.permits(1, mapOf("Sec-Fetch-Dest" to "font", "Accept" to "image/*")))
        assertFalse(g.permits(1, mapOf("Sec-Fetch-Dest" to "iframe")))
    }

    @Test
    fun allowanceDiesWithTheConversation() {
        val g = RemoteImageGate()
        g.open(spam = true, uids = listOf(1L))
        g.allow(1)
        g.open(spam = false, uids = listOf(5L))
        g.open(spam = true, uids = listOf(1L))
        assertFalse(g.permits(1, img))
    }

    @Test
    fun newMessagesCanBeAllowed() {
        val g = RemoteImageGate()
        g.open(spam = true, uids = listOf(1L))
        g.allow(7)
        assertFalse(g.permits(7, img))
        g.extend(listOf(7L))
        g.allow(7)
        assertTrue(g.permits(7, img))
    }
}
