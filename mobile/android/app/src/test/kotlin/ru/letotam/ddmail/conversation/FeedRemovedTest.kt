package ru.letotam.ddmail.conversation

import org.junit.Assert.assertEquals
import org.junit.Test
import ru.letotam.ddmail.api.MessageRef

class FeedRemovedTest {
    private fun refs(uids: Iterable<Int>) = uids.map { MessageRef(uid = it.toLong()) }

    @Test
    fun removedFromWindowLeaveFeed() {
        // Показаны 25…6 (окно 20, от нового к старому); удалили показанное 20 и непоказанное 3.
        val pages = refs((25 downTo 1))
        val now = refs((1..25).filter { it != 20 && it != 3 })
        val (gone, loaded) = feedRemoved(pages, 20, now)
        assertEquals(listOf(20L), gone)
        assertEquals(19, loaded)
    }

    @Test
    fun emptyConversationIsNotDeletion() {
        // Пустая беседа — пересборка кэша, а не удаление всего.
        val (gone, loaded) = feedRemoved(refs(5 downTo 1), 5, emptyList())
        assertEquals(emptyList<Long>(), gone)
        assertEquals(5, loaded)
    }

    @Test
    fun nothingGone() {
        val pages = refs(3 downTo 1)
        assertEquals(emptyList<Long>() to 3, feedRemoved(pages, 3, refs(1..4)))
    }
}
