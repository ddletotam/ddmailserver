import Foundation
import Testing
@testable import DDMailAPI

/// Статус доставки исходящих (контракт §5, десктоп §1): поля тел, ссылок,
/// строк бесед, ответа `/send` и событий WebSocket.
struct DeliveryTests {
    private func decode<T: Decodable>(_ t: T.Type, _ json: String) throws -> T {
        try JSONDecoder().decode(t, from: Data(json.utf8))
    }

    @Test func bodyCarriesDeliveryFields() throws {
        let b = try decode(MessageBody.self, """
            {"uid":42,"is_outgoing":true,"message_id":"<m@a.ru>","delivery_status":"failed",
             "delivery_reason":"domain_not_found","delivery_error":"Домен получателя не найден",
             "delivery_failed":["bob@dead.example"]}
            """)
        #expect(b.delivery == .failed)
        #expect(b.deliveryReason == "domain_not_found")
        #expect(b.deliveryError == "Домен получателя не найден")
        #expect(b.deliveryFailed == ["bob@dead.example"])
        // В кэше тело лежит своим JSON — поля переживают круг.
        let again = try JSONDecoder().decode(MessageBody.self, from: JSONEncoder().encode(b))
        #expect(again == b)
    }

    /// Старый сервер, входящие, null у Go — пусто: считать доставленным.
    @Test func absentOrNullMeansUnknown() throws {
        let b = try decode(MessageBody.self, #"{"uid":1,"delivery_failed":null}"#)
        #expect(b.delivery == .unknown)
        #expect(b.deliveryFailed.isEmpty)
        #expect(DeliveryState("") == .unknown)
        #expect(DeliveryState("bounced-someday") == .unknown)
        #expect(DeliveryState("Pending") == .pending)
        #expect(DeliveryState("delivered") == .delivered)
    }

    @Test func refAndConversationCarryStatus() throws {
        let c = try decode(ConversationDTO.self, """
            {"id":"me|bob","last_delivery_status":"pending",
             "messages":[{"uid":1,"message_id":"<a>"},{"uid":2,"message_id":"<b>","delivery_status":"pending"}]}
            """)
        #expect(c.lastDeliveryStatus == "pending")
        #expect(c.messages.map(\.deliveryStatus) == ["", "pending"])
        let old = try decode(ConversationDTO.self, #"{"id":"x"}"#)
        #expect(old.lastDeliveryStatus == "")
    }

    /// Ссылка уходит в запросы тел и флагов: пустой статус не добавляет поле.
    @Test func refEncodesStatusOnlyWhenKnown() throws {
        let plain = try JSONValue.parse(JSONEncoder().encode(MessageRef(folder: "Sent", uid: 5, messageID: "<m>", seen: true)))
        #expect(plain?["delivery_status"] == nil)
        #expect(plain?["uid"] == .int(5))
        let known = try JSONValue.parse(JSONEncoder().encode(MessageRef(uid: 5, deliveryStatus: "failed")))
        #expect(known?["delivery_status"] == .string("failed"))
    }

    @Test func messageIDsCompareWithoutBrackets() {
        #expect(normalizedMessageID("<Abc@X.ru>") == normalizedMessageID("abc@x.ru"))
        #expect(normalizedMessageID("") == "")
    }

    @Test func deliveryStatusEventParses() throws {
        let e = try #require(ServerEvent.parse(Data("""
            {"type":"delivery_status","user_id":7,"message_id":901,"rfc_message_id":"m1@a.ru",
             "delivery_status":"failed","delivery_reason":"unreachable","delivery_error":"Сервер получателя недоступен",
             "delivery_failed":["x@logdoc.example"]}
            """.utf8)))
        let d = try #require(e.delivery)
        #expect(d == DeliveryUpdate(uid: 901, messageID: "m1@a.ru", status: "failed", reason: "unreachable",
                                    error: "Сервер получателя недоступен", failed: ["x@logdoc.example"]))
    }

    @Test func otherEventsHaveNoDelivery() throws {
        let sent = try #require(ServerEvent.parse(Data(#"{"type":"message_sent","message_id":900,"rfc_message_id":"m0@a.ru"}"#.utf8)))
        #expect(sent.type == ServerEvent.messageSent)
        #expect(sent.messageID == 900)
        #expect(sent.rfcMessageID == "m0@a.ru")
        #expect(sent.delivery == nil)
        #expect(ServerEvent.parse(Data(#"{"type":"calendar_updated","calendar_id":3}"#.utf8))?.type == ServerEvent.calendarUpdated)
        // Событие доставки без адреса письма применить некуда.
        #expect(ServerEvent.parse(Data(#"{"type":"delivery_status","delivery_status":"failed"}"#.utf8))?.delivery == nil)
        #expect(ServerEvent.parse(Data("not json".utf8)) == nil)
        #expect(ServerEvent.parse(Data(#"{"user_id":1}"#.utf8)) == nil)
    }

    @Test func sendReturnsReceipt() async throws {
        let t = FakeTransport { req in
            #expect(req.url?.path() == "/api/desktop/v1/send")
            return (200, #"{"status":"queued","rfc_message_id":"m2@a.ru","delivery_status":"pending"}"#)
        }
        let api = SessionAPI(baseURL: "https://mail.example.ru", transport: t, tokens: MemoryTokens("T"))
        let r = try await api.send(SendRequest(from: "me@a.ru", to: ["bob@x.ru"], subject: "s", html: "", text: "t"))
        #expect(r == SendReceipt(rfcMessageID: "m2@a.ru", deliveryStatus: "pending"))
    }

    /// Старый сервер отвечает без полей (или не JSON) — письмо всё равно принято.
    @Test func sendToleratesBareResponse() async throws {
        let t = FakeTransport { _ in (200, "") }
        let api = SessionAPI(baseURL: "https://mail.example.ru", transport: t, tokens: MemoryTokens("T"))
        let r = try await api.send(SendRequest(from: "me@a.ru", to: ["bob@x.ru"], subject: "s", html: "", text: "t"))
        #expect(r == SendReceipt())
    }
}
