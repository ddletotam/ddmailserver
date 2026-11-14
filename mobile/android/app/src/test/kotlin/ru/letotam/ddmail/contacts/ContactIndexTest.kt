package ru.letotam.ddmail.contacts

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import ru.letotam.ddmail.api.ContactDto

/**
 * Сводная книга на экране (0.0.40): вся книга, поиск без регистра и
 * диакритики, буквенные разделы, созданные и найденные сервером — в списке
 * сразу; и на объёме книги владельца (15–30 тыс.).
 */
class ContactIndexTest {
    private fun ids(secs: List<ContactSection>) = secs.flatMap { s -> s.items.map { it.id } }

    @Test
    fun searchFoldsCaseDiacriticsAndPhones() {
        val book = ContactIndex(
            listOf(
                ContactDto(1, "Ёлкин Пётр", emails = listOf("PETR@x.ru"), phones = listOf("+7 (900) 111-22-33")),
                ContactDto(2, "Аудит Контакт", organization = "ООО Ромашка"),
                ContactDto(3, "", emails = listOf("nobody@y.ru")),
                ContactDto(4, "Йога Клуб", phones = listOf("+7 111")),
            ),
        )
        fun find(q: String) = book.search(ContactIndex.Query(q)).map { it.id }
        assertEquals(listOf(1L), find("елкин"))
        assertEquals(listOf(1L), find("petr@"))
        assertEquals(listOf(1L), find("900 111-22"))
        assertEquals(listOf(2L), find("ромашка"))
        assertEquals(listOf(3L), find("NOBODY"))
        assertEquals(listOf(4L), find("йога"))
        // Буквы с цифрой — не номер: «a1» не находит всех с единицей в телефоне.
        assertEquals(emptyList<Long>(), find("а1"))
    }

    @Test
    fun sortedByNameWithLetterSections() {
        val book = ContactIndex(
            listOf(ContactDto(3, "Борис"), ContactDto(2, "Анна"), ContactDto(1, "анна"), ContactDto(4, "1С Бот"), ContactDto(5, "Alice")),
        )
        // Равные имена — по id: порядок не прыгает между загрузками.
        // Кириллица, затем латиница, «#» в конце — как у ICU телефона с
        // локалью ru (у Collator JVM латиница была бы первой).
        assertEquals(listOf(1L, 2L, 3L, 5L, 4L), book.sorted.map { it.id })
        val secs = ContactIndex.sections(book.sorted)
        assertEquals(listOf("А", "Б", "A", "#"), secs.map { it.letter })
        assertEquals(listOf(1L, 2L), secs[0].items.map { it.id })
        // Список и разделы — в одном порядке (прокрутка к созданному считает по разделам).
        assertEquals(book.sorted.map { it.id }, ids(secs))
    }

    @Test
    fun duplicateIdsCollapse() {
        // Повтор id уронил бы LazyColumn (ключи строк).
        val book = ContactIndex(listOf(ContactDto(1, "А"), ContactDto(1, "А"), ContactDto(2, "Б")))
        assertEquals(listOf(1L, 2L), book.sorted.map { it.id })
    }

    /** Созданный здесь виден сразу — в общем списке на своём месте и в поиске. */
    @Test
    fun createdContactShowsBeforeReload() {
        val book = ContactIndex(listOf(ContactDto(1, "Анна"), ContactDto(2, "Борис"), ContactDto(3, "Вера")))
        val created = ContactDto(99, "Белла", emails = listOf("bella@x.ru"))
        assertEquals(listOf(1L, 99L, 2L, 3L), ids(book.view("", listOf(created))))
        assertEquals(listOf(99L), ids(book.view("белл", listOf(created))))
        assertEquals(listOf(99L), ids(book.view("bella@", listOf(created))))
        assertEquals(emptyList<Long>(), ids(book.view("вер0", listOf(created))))
        // Пришёл с загрузкой — не дублируется.
        assertEquals(listOf(1L, 2L, 3L), ids(book.view("", listOf(ContactDto(2, "Борис")))))
    }

    /** Найденное сервером за пределами загруженного вливается в результаты по алфавиту. */
    @Test
    fun serverHitsMergeIntoSearch() {
        val book = ContactIndex(listOf(ContactDto(1, "Аудит Альфа"), ContactDto(3, "Аудит Гамма")))
        val remote = listOf(ContactDto(2, "Аудит Бета"))
        assertEquals(listOf(1L, 2L, 3L), ids(book.view("аудит", remote)))
        // Латинский от сервера — после кириллицы, не в начале.
        assertEquals(listOf(1L, 3L, 7L), ids(book.view("", listOf(ContactDto(7, "Audit Zeta")))))
    }

    /**
     * Книга владельца — 15 000 карточек; держим запас: 30 000. Индекс
     * строится вне главного потока, а поиск идёт на каждое нажатие — он
     * должен быть быстрым и найти последний по алфавиту контакт.
     */
    @Test
    fun largeBookIsWholeAndSearchIsFast() {
        val names = listOf("Абай", "Борис", "Виктор", "Галина", "Яна")
        val list = (0 until 30_000).map { i ->
            ContactDto(i + 1L, "${names[i % names.size]} Фамилия$i", emails = listOf("u$i@corp.kz"), phones = listOf("+7700$i"))
        } + ContactDto(99_999, "Яяя Аудит Контакт", emails = listOf("audit@example.invalid"))
        var t = System.nanoTime()
        val book = ContactIndex(list)
        val buildMs = (System.nanoTime() - t) / 1_000_000
        assertEquals(30_001, book.sorted.size)
        assertEquals(99_999L, book.sorted.last().id)
        // Прогрев, затем замер поиска.
        book.search(ContactIndex.Query("аудит"))
        t = System.nanoTime()
        val hits = book.search(ContactIndex.Query("аудит контакт"))
        val searchMs = (System.nanoTime() - t) / 1_000_000
        assertEquals(listOf(99_999L), hits.map { it.id })
        t = System.nanoTime()
        val secs = book.view("", listOf(ContactDto(100_000, "Ааа Новый")))
        val viewMs = (System.nanoTime() - t) / 1_000_000
        assertEquals(30_002, secs.sumOf { it.items.size })
        assertEquals(100_000L, secs.first().items.first().id)
        assertEquals(99_999L, secs.last { it.letter != "#" }.items.last().id)
        println("ContactIndex 30k: build ${buildMs}ms, search ${searchMs}ms, view ${viewMs}ms")
        assertTrue("поиск по 30 000 — ${searchMs}ms", searchMs < 200)
    }
}
