package ru.letotam.ddmail.conversation

import org.junit.Assert.assertEquals
import org.junit.Test

/** Аудит 2026-11-14 №15: у «Открыть» вложение есть запасной путь. */
class OpenTypesTest {
    @Test
    fun textFallsBackToPlainThenAny() {
        assertEquals(listOf("text/markdown", "text/plain", "*/*"), openTypes("text/markdown", "notes.md"))
        // Сервер не знал тип — текст узнаётся по расширению.
        assertEquals(listOf("application/octet-stream", "text/plain", "*/*"), openTypes("application/octet-stream", "README.MD"))
        assertEquals(listOf("text/plain", "*/*"), openTypes("text/plain", "a.txt"))
    }

    @Test
    fun binaryFallsBackToAnyOnly() {
        assertEquals(listOf("application/pdf", "*/*"), openTypes("application/pdf", "a.pdf"))
        assertEquals(listOf("application/x-foo", "*/*"), openTypes("Application/X-Foo", null))
    }
}
