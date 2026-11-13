import DDMailAPI
import DDMailStore
import Foundation
import Testing
@testable import DDMailSync

/// Статус доставки на пути «сервер → кэш → лента и строка» (контракт §5).
struct DeliverySyncTests {
    let server = FakeServer()
    let store: MailStore
    let engine: SyncEngine

    init() throws {
        store = try MailStore.inMemory()
        let api = SessionAPI(baseURL: "https://mail.a.ru", transport: server, tokens: FixedTokens())
        engine = SyncEngine(accountKey: "me@mail.a.ru", api: api, store: store, now: { 1_000_000 })
        server.on("/conversations?spam=1&limit=2000", "[]")
        server.on("/identities", #"[{"email":"me@a.ru","is_default":true}]"#)
        server.on("/identity-groups", #"{"version":1,"doc":{"groups":[]}}"#)
        server.on("/dialog-folders", #"{"version":1,"doc":{"folders":[]}}"#)
    }

    private func delta(_ status: String, now: Int64) -> String {
        """
        {"server_now_ms":\(now),"conversations":[{"id":"me@a.ru|bob@logdoc.example","label":"bob","last_date_ts":10,
         "last_delivery_status":"\(status)","updated_at":\(now),
         "messages":[{"folder":"Sent","uid":7,"message_id":"<m7@a.ru>","seen":true,"delivery_status":"\(status)"}]}]}
        """
    }

    private func body(_ status: String, error: String = "") -> String {
        #"[{"uid":7,"is_outgoing":true,"text":"привет","message_id":"<m7@a.ru>","delivery_status":"\#(status)","delivery_error":"\#(error)","delivery_failed":\#(status == "failed" ? #"["bob@logdoc.example"]"# : "null")}]"#
    }

    /// Статус приходит дельтой: строка видит его, а тело в кэше со старым
    /// статусом перезапрашивается — без сокета пузырь тоже обновится.
    @Test func deltaStatusRefreshesStaleBody() async throws {
        server.on("/changes?since=0", #"{"entries":[],"latest_seq":1}"#)
        server.on("/conversations?limit=5000&since=0", delta("pending", now: 100))
        try await engine.sync()
        var c = try #require(engine.currentConversations().first)
        #expect(c.lastDelivery == .pending)
        server.on("/conversations/messages", body("pending"))
        #expect(try await engine.bodies(c.messages).first?.delivery == .pending)

        // Ещё раз — из кэша, без запроса.
        server.log.withLock { $0.removeAll() }
        _ = try await engine.bodies(c.messages)
        #expect(server.requests.isEmpty)

        server.on("/changes?since=1", #"{"entries":[],"latest_seq":2}"#)
        server.on("/conversations?limit=5000&since=100", delta("failed", now: 200))
        try await engine.sync()
        c = try #require(engine.currentConversations().first)
        #expect(c.lastDelivery == .failed)
        #expect(c.messages.first?.deliveryStatus == "failed")
        server.on("/conversations/messages", body("failed", error: "Домен получателя не найден"))
        server.log.withLock { $0.removeAll() }
        let b = try #require(try await engine.bodies(c.messages).first)
        #expect(server.requests == ["POST /conversations/messages"])
        #expect(b.delivery == .failed)
        #expect(b.deliveryError == "Домен получателя не найден")
    }

    /// Нет связи — тело со старым статусом лучше пустого места.
    @Test func staleBodyIsShownOffline() async throws {
        try store.putBodies(engine.accountKey, [MessageBody(uid: 7, text: "привет", isOutgoing: true, messageID: "<m7@a.ru>", deliveryStatus: "pending")])
        server.on("/conversations/messages", 503, "down")
        let got = try await engine.bodies([MessageRef(uid: 7, messageID: "<m7@a.ru>", deliveryStatus: "failed")])
        #expect(got.first?.delivery == .pending)
    }

    /// Событие WS: статус — в кэш сразу, строка и ссылка видят его без синка.
    @Test func eventAppliesWithoutSync() async throws {
        server.on("/changes?since=0", #"{"entries":[],"latest_seq":1}"#)
        server.on("/conversations?limit=5000&since=0", delta("pending", now: 100))
        try await engine.sync()
        let e = try #require(ServerEvent.parse(Data(#"{"type":"delivery_status","message_id":7,"rfc_message_id":"m7@a.ru","delivery_status":"failed","delivery_error":"нет домена","delivery_failed":["bob@logdoc.example"]}"#.utf8)))
        let touched = try await engine.applyDelivery(try #require(e.delivery))
        #expect(touched == ["me@a.ru|bob@logdoc.example"])
        let c = try #require(engine.currentConversations().first)
        #expect(c.lastDelivery == .failed)
        #expect(c.messages.first?.deliveryStatus == "failed")
    }

    @Test func staleRule() {
        let pending = MessageBody(uid: 1, deliveryStatus: "pending")
        #expect(SyncEngine.deliveryStale(MessageRef(uid: 1, deliveryStatus: "failed"), pending))
        #expect(!SyncEngine.deliveryStale(MessageRef(uid: 1, deliveryStatus: "pending"), pending))
        // Пустой статус ссылки — неизвестно (кэш до миграции): тело не трогаем.
        #expect(!SyncEngine.deliveryStale(MessageRef(uid: 1), pending))
        #expect(!SyncEngine.deliveryStale(MessageRef(uid: 1, deliveryStatus: "failed"), nil))
    }

    /// Поле `delivery` письма документа ленты (договорённость с Android):
    /// `{status, error, failed}` из тела.
    @Test func feedMessageCarriesDelivery() throws {
        let c = Conversation(accountKey: "a", id: "c")
        let failed = MessageBody(uid: 7, text: "привет", isOutgoing: true, deliveryStatus: "failed",
                                 deliveryReason: "domain_not_found", deliveryError: "Домен не найден",
                                 deliveryFailed: ["bob@logdoc.example"])
        let incoming = MessageBody(uid: 8, text: "ответ")
        let batch = FeedBatch(conversation: c, bodies: [failed, incoming], hasOlder: false, focusUid: nil, imageSenders: [])
        let j = try #require(JSONValue.parse(Data(batch.javaScriptLiteral().utf8)))
        let msgs = try #require(j["messages"]?.array)
        #expect(msgs[0]["delivery"] == .object([
            "status": .string("failed"), "error": .string("Домен не найден"),
            "failed": .array([.string("bob@logdoc.example")]),
        ]))
        // Входящее и неизвестное — пустой статус (= доставлено), поле всё равно есть.
        #expect(msgs[1]["delivery"]?["status"] == .string(""))
        #expect(msgs[1]["delivery"]?["failed"] == .array([]))
    }

    @Test func updateScriptPrefersInPlaceUpdate() {
        let c = Conversation(accountKey: "a", id: "c")
        let one = FeedBatch(conversation: c, bodies: [MessageBody(uid: 7, deliveryStatus: "failed")], hasOlder: false, focusUid: nil, imageSenders: [])
        let js = FeedBatch.updateScript(one, orSet: one)
        #expect(js.contains("typeof ddm.update==='function'"))
        #expect(js.contains("ddm.set(f)"))
        #expect(!js.contains("\n"))
        #expect(js.hasSuffix(")"))
    }

    /// Окно ленты: перерисовать только показанные письма со сменившимся статусом.
    @Test func windowReportsRedelivered() {
        var w = FeedWindow([MessageRef(uid: 1), MessageRef(uid: 2, deliveryStatus: "pending")])
        #expect(w.redelivered([MessageRef(uid: 1), MessageRef(uid: 2, deliveryStatus: "pending")]).isEmpty)
        #expect(w.redelivered([MessageRef(uid: 1), MessageRef(uid: 2, deliveryStatus: "failed")]).map(\.uid) == [2])
        // Повтор того же — уже не смена.
        #expect(w.redelivered([MessageRef(uid: 2, deliveryStatus: "failed")]).isEmpty)
        // Пустой статус (кэш до миграции) — не смена.
        #expect(w.redelivered([MessageRef(uid: 2)]).isEmpty)
        // Новое письмо — не «перерисовка», а `fresh`.
        #expect(w.redelivered([MessageRef(uid: 3, deliveryStatus: "pending")]).isEmpty)
    }

    /// Беседа нового письма — по Message-ID из ответа `/send`, даже когда id
    /// беседы посчитан иначе, чем у сервера.
    @Test func sentTargetMatchesByMessageID() {
        var t = SentTarget(from: "me@a.ru", to: ["bob@logdoc.example"], cc: [], own: [Identity(email: "me@a.ru")])
        let c = Conversation(accountKey: "k", id: "other@a.ru+me@a.ru|bob@logdoc.example",
                             counterparts: [ContactInfo(addr: "bob@logdoc.example"), ContactInfo(addr: "ann@x.ru")],
                             messages: [MessageRef(uid: 9, messageID: "<M9@a.ru>")])
        #expect(!t.matches(c))
        t.messageID = "m9@a.ru"
        #expect(t.matches(c))
        var spam = c
        spam.spam = true
        #expect(!t.matches(spam))
    }
}
