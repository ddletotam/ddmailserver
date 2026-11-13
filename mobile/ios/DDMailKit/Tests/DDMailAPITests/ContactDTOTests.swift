import Foundation
import Testing
@testable import DDMailAPI

/// Контакт сводной книги: адресов и телефонов — сколько пришло, право правки —
/// из `can_write`, без него — «можно» (решит сервер).
struct ContactDTOTests {
    @Test func decodesAllAddressesAndWriteFlag() throws {
        let json = #"[{"id":1,"full_name":"Анна","emails":["a@x.ru","b@x.ru","c@x.ru","d@x.ru"],"phones":["1","2","3","4"]},"#
            + #"{"id":2,"full_name":"Склад","can_write":false}]"#
        let list = try JSONDecoder().decode([ContactDTO].self, from: Data(json.utf8))
        #expect(list[0].emails.count == 4)
        #expect(list[0].phones.count == 4)
        #expect(list[0].canWrite)
        #expect(list[1].canWrite == false)
        #expect(list[1].emails.isEmpty)
    }
}
