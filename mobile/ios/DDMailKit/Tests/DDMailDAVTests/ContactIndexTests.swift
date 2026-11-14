import DDMailAPI
import Foundation
import Testing
@testable import DDMailDAV

/// Сводная книга на экране (аудит 2026-11-14 №5): вся книга, поиск без
/// регистра и диакритики, буквенные разделы — и на объёме книги владельца.
struct ContactIndexTests {
    @Test func searchFoldsCaseAndDiacriticsAndPhones() {
        let book = ContactIndex([
            ContactDTO(id: 1, fullName: "Ёлкин Пётр", emails: ["PETR@x.ru"], phones: ["+7 (900) 111-22-33"]),
            ContactDTO(id: 2, fullName: "Аудит Контакт", organization: "ООО Ромашка"),
            ContactDTO(id: 3, fullName: "", emails: ["nobody@y.ru"]),
        ])
        #expect(book.search(.init("елкин")).map(\.id) == [1])
        #expect(book.search(.init("petr@")).map(\.id) == [1])
        #expect(book.search(.init("9001112")).map(\.id) == [1])
        #expect(book.search(.init("ромашка")).map(\.id) == [2])
        #expect(book.search(.init("NOBODY")).map(\.id) == [3])
    }

    @Test func sortedByNameWithLetterSections() {
        let book = ContactIndex([
            ContactDTO(id: 3, fullName: "Борис"), ContactDTO(id: 1, fullName: "анна"),
            ContactDTO(id: 2, fullName: "Анна"), ContactDTO(id: 4, fullName: "1С Бот"),
        ])
        // Одинаковые имена — по id: порядок не прыгает между загрузками.
        #expect(book.sorted.map(\.id) == [4, 1, 2, 3])
        let sections = ContactIndex.sections(book.sorted)
        #expect(sections.map(\.letter) == ["А", "Б", "#"])
        #expect(sections[0].items.map(\.id) == [1, 2])
    }

    /// Книга владельца — 15 000 карточек; держим запас: 30 000. Индекс
    /// строится вне главного потока, а поиск идёт на каждое нажатие — он
    /// должен быть быстрым, и найти последний по алфавиту контакт.
    @Test func largeBookIsWholeAndSearchIsFast() {
        let names = ["Абай", "Борис", "Виктор", "Галина", "Яна"]
        var list = (0..<30_000).map { i in
            ContactDTO(id: Int64(i + 1), fullName: "\(names[i % names.count]) Фамилия\(i)", emails: ["u\(i)@corp.kz"], phones: ["+7700\(i)"])
        }
        list.append(ContactDTO(id: 99_999, fullName: "Яяя Аудит Контакт", emails: ["audit@example.invalid"]))
        let clock = ContinuousClock()
        var book = ContactIndex()
        let build = clock.measure { book = ContactIndex(list) }
        #expect(book.sorted.count == 30_001)
        #expect(book.sorted.last?.id == 99_999)
        var hits: [ContactDTO] = []
        let search = clock.measure { hits = book.search(.init("аудит контакт")) }
        #expect(hits.map(\.id) == [99_999])
        #expect(ContactIndex.sections(book.sorted).last?.items.last?.id == 99_999)
        print("ContactIndex 30k: build \(build), search \(search)")
        #expect(search < .milliseconds(500), "поиск по 30 000 — \(search)")
    }
}
