package ru.letotam.ddmail.contacts

import ru.letotam.ddmail.api.ContactDto
import java.text.CollationKey
import java.text.Collator
import java.text.Normalizer
import java.util.Locale

/** Имя для списка: без имени — первый адрес, затем телефон. */
internal fun ContactDto.display(): String =
    fullName.trim().ifBlank { emails.firstOrNull() ?: phones.firstOrNull() ?: "(без имени)" }

/** Буквенный раздел списка контактов (липкий заголовок + строки). */
data class ContactSection(val letter: String, val items: List<ContactDto>)

/**
 * Книга, приготовленная к показу и поиску (§15): сортировка по имени
 * (`Collator` ru, равные имена — по id, порядок не прыгает между загрузками),
 * свёрнутые строки поиска (без регистра и диакритики, «ё» = «е»), цифры
 * телефонов. Книга в 15–30 тыс. карточек — строится вне главного потока
 * ([ContactsViewModel] на `Dispatchers.Default`); поиск по готовым строкам —
 * единицы миллисекунд, его можно звать на каждое нажатие.
 */
class ContactIndex(list: List<ContactDto>) {
    /** Вся книга по алфавиту; id не повторяются (первый выигрывает). */
    val sorted: List<ContactDto>
    val byId: Map<Long, ContactDto>
    private val sortKeys: List<CollationKey>
    private val keys: Array<String>
    private val digits: Array<String>

    init {
        val unique = LinkedHashMap<Long, ContactDto>(list.size * 2)
        for (c in list) unique.putIfAbsent(c.id, c)
        val keyed = unique.values.map { Keyed(collationKey(it), it) }.sortedWith(KEYED_ORDER)
        sorted = keyed.map { it.c }
        sortKeys = keyed.map { it.key }
        byId = unique
        keys = Array(sorted.size) { key(sorted[it]) }
        digits = Array(sorted.size) { phoneDigits(sorted[it]) }
    }

    /** Строка поиска, свёрнутая один раз на запрос. */
    class Query(raw: String) {
        val text: String = fold(raw.trim())
        /**
         * Цифры — только когда строка похожа на номер (цифры, пробелы, `+-()`):
         * иначе «a1» находил бы всех с единицей в телефоне.
         */
        val digits: String = raw.trim().takeIf { s -> s.any(Char::isDigit) && s.all { it.isDigit() || it in PHONE_CHARS } }
            ?.filter(Char::isDigit).orEmpty()
        val isEmpty: Boolean get() = text.isEmpty()
    }

    /** Подходящие из загруженной книги — в порядке книги. */
    fun search(q: Query): List<ContactDto> = if (q.isEmpty) sorted else hits(q).map { sorted[it] }

    private fun hits(q: Query): List<Int> {
        if (q.isEmpty) return sorted.indices.toList()
        val out = ArrayList<Int>()
        for (i in keys.indices) {
            if (keys[i].contains(q.text) || (q.digits.isNotEmpty() && digits[i].contains(q.digits))) out += i
        }
        return out
    }

    /**
     * Что показывает список: книга по строке [query] плюс [extra] — созданные
     * здесь, но ещё не пришедшие с загрузкой, и найденные сервером за
     * пределами загруженного. Уже известные книге id из [extra] отбрасываются;
     * остальные вливаются по алфавиту без пересортировки всей книги.
     */
    fun view(query: String, extra: Collection<ContactDto>): List<ContactSection> {
        val q = Query(query)
        val add = extra.filter { byId[it.id] == null && matches(it, q) }.distinctBy { it.id }
        if (add.isEmpty()) return sections(search(q))
        val a = hits(q).map { Keyed(sortKeys[it], sorted[it]) }
        val b = add.map { Keyed(collationKey(it), it) }.sortedWith(KEYED_ORDER)
        val merged = ArrayList<ContactDto>(a.size + b.size)
        var i = 0
        var j = 0
        while (i < a.size || j < b.size) {
            merged += if (j >= b.size || (i < a.size && KEYED_ORDER.compare(a[i], b[j]) <= 0)) a[i++].c else b[j++].c
        }
        return sections(merged)
    }

    private class Keyed(val key: CollationKey, val c: ContactDto)

    companion object {
        private const val PHONE_CHARS = " +-()."
        private val DIACRITICS = Regex("\\p{Mn}+")
        private val collator: Collator = Collator.getInstance(Locale("ru")).apply { strength = Collator.SECONDARY }
        private val KEYED_ORDER = Comparator<Keyed> { x, y -> x.key.compareTo(y.key).takeIf { it != 0 } ?: x.c.id.compareTo(y.c.id) }

        /** Без регистра и диакритики; «ё» → «е», «й» → «и» (как `diacriticInsensitive` у iOS). */
        fun fold(s: String): String {
            val lower = s.lowercase(Locale.ROOT)
            if (lower.all { it.code < 0x80 }) return lower
            return DIACRITICS.replace(Normalizer.normalize(lower, Normalizer.Form.NFD), "")
        }

        fun matches(c: ContactDto, q: Query): Boolean =
            q.isEmpty || key(c).contains(q.text) || (q.digits.isNotEmpty() && phoneDigits(c).contains(q.digits))

        private fun key(c: ContactDto): String = fold((listOf(c.fullName, c.organization) + c.emails).joinToString("\n"))

        private fun phoneDigits(c: ContactDto): String = c.phones.joinToString("\n") { p -> p.filter(Char::isDigit) }

        // Collator не потокобезопасен — ключи строятся под его замком.
        private fun collationKey(c: ContactDto): CollationKey = synchronized(collator) { collator.getCollationKey(c.display()) }

        /**
         * Разделы по первой букве имени (в порядке списка); не буква — «#» в
         * конце, буквы — по алфавиту.
         */
        fun sections(list: List<ContactDto>): List<ContactSection> {
            val groups = LinkedHashMap<String, MutableList<ContactDto>>()
            for (c in list) {
                val first = c.display().firstOrNull()
                val letter = if (first != null && first.isLetter()) first.uppercaseChar().toString() else "#"
                groups.getOrPut(letter) { ArrayList() } += c
            }
            val letters = groups.keys.filter { it != "#" }.sortedWith { x, y -> synchronized(collator) { collator.compare(x, y) } }
            return (letters + listOfNotNull("#".takeIf { it in groups })).map { ContactSection(it, groups.getValue(it)) }
        }
    }
}
