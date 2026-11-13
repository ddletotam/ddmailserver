package ru.letotam.ddmail.mail

import org.junit.Assert.assertEquals
import org.junit.Test
import ru.letotam.ddmail.sync.DialogFolder
import ru.letotam.ddmail.sync.DialogKind
import ru.letotam.ddmail.sync.FolderRules

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
}
