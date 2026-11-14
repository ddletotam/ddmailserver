package ru.letotam.ddmail.mail

import org.junit.Assert.assertEquals
import org.junit.Test
import ru.letotam.ddmail.sync.DialogFolder
import ru.letotam.ddmail.sync.DialogKind
import ru.letotam.ddmail.sync.FolderRules
import ru.letotam.ddmail.sync.FoldersDoc

class FoldersUiTest {
    /** Как `icon_keeps_four_symbols_without_splitting_emoji` десктопа. */
    @Test
    fun iconKeepsFourSymbolsWithoutSplittingEmoji() {
        assertEquals("abcd", clipIcon("abcdef"))
        assertEquals("👨‍👩‍👧", clipIcon("👨‍👩‍👧"))
        assertEquals("🇷🇺🇷🇺a👍🏽", clipIcon("🇷🇺🇷🇺a👍🏽xyz"))
    }

    @Test
    fun summaryNamesRules() {
        val f = DialogFolder(
            "a", "A", "", FolderRules(identities = listOf("me@a.ru"), kind = DialogKind.Group),
            emptyList(), emptyList(), hidden = true, autoRead = false,
        )
        assertEquals("на me@a.ru · только группы · скрыта", folderSummary(f))
        assertEquals("без правил — только руками", folderSummary(f.copy(rules = FolderRules(), hidden = false)))
    }

    /** «Спам» скрыт по умолчанию, виден с «Показать скрытые» (бледнее, без счётчика). */
    @Test
    fun spamHiddenByDefaultShownWithShowHidden() {
        val work = DialogFolder("w", "Работа", "", FolderRules(domains = listOf("x.ru")), emptyList(), emptyList(), hidden = false, autoRead = false)
        val secret = work.copy(id = "h", name = "Скрытая", hidden = true)
        // Другой клиент снял «скрыта» со «Спама» — нормализация чинит.
        val doc = FoldersDoc(listOf(work, secret)).withSpam().let { d ->
            d.copy(folders = d.folders.map { if (it.isSystem) it.copy(hidden = false) else it })
        }.withSpam()
        val counts = listOf(1, 1, 5)

        val inbox = folderTabs(doc, counts, active = null, showHidden = false)
        assertEquals(listOf("w"), inbox.map { it.id })
        assertEquals(2, doc.hiddenCount)

        val shown = folderTabs(doc, counts, active = null, showHidden = true)
        assertEquals(listOf("w", "h", "spam"), shown.map { it.id })
        val spam = shown[2]
        assertEquals(true, spam.hidden)
        assertEquals(true, spam.system)
        assertEquals(0, spam.unread)
        assertEquals(1, shown[1].unread)

        // Привели в «Спам» — его таб виден и без «Показать скрытые».
        val led = folderTabs(doc, counts, active = doc.spamIndex, showHidden = false)
        assertEquals(listOf("w", "spam"), led.map { it.id })
    }
}
