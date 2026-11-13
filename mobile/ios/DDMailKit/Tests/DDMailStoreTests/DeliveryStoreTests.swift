import DDMailAPI
import Foundation
import GRDB
import Testing
@testable import DDMailStore

/// Статус доставки в кэше (миграция `v3-delivery`, контракт §5).
struct DeliveryStoreTests {
    private func conv(_ id: String, last: String = "", _ refs: [(Int64, String, String)]) throws -> ConversationDTO {
        let r = refs.map { #"{"uid":\#($0.0),"message_id":"\#($0.1)","delivery_status":"\#($0.2)"}"# }.joined(separator: ",")
        return try JSONDecoder().decode(ConversationDTO.self, from: Data(
            #"{"id":"\#(id)","last_delivery_status":"\#(last)","messages":[\#(r)]}"#.utf8))
    }

    @Test func statusesSurviveTheCache() throws {
        let s = try MailStore.inMemory()
        try s.replaceAll("a", [try conv("bob", last: "pending", [(1, "<in@x>", ""), (2, "<out@a>", "pending")])])
        let snap = try #require(try s.snapshot(["a"])["a"])
        #expect(snap.conversations.first?.lastDeliveryStatus == "pending")
        #expect(snap.refs["bob"]?.sorted { $0.uid < $1.uid }.map(\.ref.deliveryStatus) == ["", "pending"])
    }

    /// Событие `delivery_status`: ссылка, тело и строка беседы — сразу.
    @Test func eventUpdatesRefBodyAndRow() throws {
        let s = try MailStore.inMemory()
        try s.replaceAll("a", [try conv("bob", last: "pending", [(1, "<in@x>", ""), (2, "<out@a>", "pending")])])
        try s.putBodies("a", [MessageBody(uid: 2, text: "привет", isOutgoing: true, messageID: "<out@a>", deliveryStatus: "pending")])
        let touched = try s.applyDelivery("a", DeliveryUpdate(
            uid: 2, messageID: "out@a", status: "failed", reason: "domain_not_found",
            error: "Домен не найден", failed: ["bob@dead.example"]))
        #expect(touched == ["bob"])
        let snap = try #require(try s.snapshot(["a"])["a"])
        #expect(snap.conversations.first?.lastDeliveryStatus == "failed")
        #expect(snap.refs["bob"]?.first { $0.uid == 2 }?.deliveryStatus == "failed")
        let body = try #require(try s.bodies("a", uids: [2])[2])
        #expect(body.delivery == .failed)
        #expect(body.deliveryError == "Домен не найден")
        #expect(body.deliveryFailed == ["bob@dead.example"])
        #expect(body.text == "привет")
    }

    /// Не последнее письмо беседы: строка не трогается; поиск по Message-ID без скобок.
    @Test func olderMessageDoesNotTouchRowAndMatchesByMessageID() throws {
        let s = try MailStore.inMemory()
        try s.replaceAll("a", [try conv("bob", [(2, "<Out@A>", "pending"), (5, "<in@x>", "")])])
        let touched = try s.applyDelivery("a", DeliveryUpdate(uid: 0, messageID: "out@a", status: "delivered"))
        #expect(touched == ["bob"])
        let snap = try #require(try s.snapshot(["a"])["a"])
        #expect(snap.conversations.first?.lastDeliveryStatus == "")
        #expect(snap.refs["bob"]?.first { $0.uid == 2 }?.deliveryStatus == "delivered")
        // Письма в кэше нет — нечего менять, статус принесёт синк.
        #expect(try s.applyDelivery("a", DeliveryUpdate(uid: 77, messageID: "nope@a", status: "failed")).isEmpty)
        #expect(try s.applyDelivery("other", DeliveryUpdate(uid: 2, status: "failed")).isEmpty)
    }

    /// Кэш до миграции: строки получают пустой статус, следующий синк ddmail
    /// полный (иначе статусы старых бесед не пришли бы дельтой).
    @Test func migrationAddsColumnsAndForcesFullSync() throws {
        let q = try DatabaseQueue()
        try MailStore.migrator.migrate(q, upTo: "v2-accounts")
        try q.write { db in
            try db.execute(sql: """
                INSERT INTO conversations (accountKey, id, label, avatarHash, receivedBy, counterpartsJSON, isGroup,
                  lastDateTs, lastSubject, unreadCount, totalCount, updatedAt)
                VALUES ('a', 'bob', 'Bob', '', '', '[]', 0, 1, 's', 0, 1, 1)
                """)
            try db.execute(sql: "INSERT INTO conversation_messages VALUES ('a', 'bob', 'Sent', 3, '<m>', 1)")
            try db.execute(sql: "INSERT INTO meta VALUES ('me@mail.a.ru/conv_full_ts', '123'), ('me@mail.a.ru/conv_since', '456')")
        }
        let s = try MailStore(writer: q)
        let snap = try #require(try s.snapshot(["a"])["a"])
        #expect(snap.conversations.first?.lastDeliveryStatus == "")
        #expect(snap.refs["bob"]?.first?.deliveryStatus == "")
        #expect(try s.meta("me@mail.a.ru/conv_full_ts") == nil)
        #expect(try s.meta("me@mail.a.ru/conv_since") == "456")
    }
}
