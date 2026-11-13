import DDMailAPI
import Foundation
import Testing
@testable import DDMailSync

/// Ветки отправки — §1 десктопного контракта; векторы `OutgoingTest.kt` Android.
struct OutgoingTests {
    private let ids = [Identity(email: "me@a.ru", name: "Я", isDefault: true), Identity(email: "work@b.ru", name: "Я")]

    private func conv(group: Bool = false, receivedBy: String = "work@b.ru") -> Conversation {
        Conversation(
            accountKey: "k", id: "c", label: "Bob", receivedBy: receivedBy,
            counterparts: [ContactInfo(name: "Bob", addr: "bob@x.ru")] + (group ? [ContactInfo(name: "Carol", addr: "carol@y.ru")] : []),
            isGroup: group, lastSubject: "Привет", totalCount: 2
        )
    }

    private func body(_ subject: String, _ from: String, out: Bool = false, to: [String] = [], cc: [String] = []) -> MessageBody {
        MessageBody(uid: 1, subject: subject, fromAddr: from, to: to, cc: cc, isOutgoing: out, messageID: "<m1@x>", references: ["<m0@x>"])
    }

    private func ok(_ b: Built) throws -> SendRequest {
        guard case let .ok(r, _) = b else { Issue.record("отказ: \(b)"); throw CancellationError() }
        return r
    }

    @Test func implicitReplyGoesToCounterpartsFromReceivingIdentity() throws {
        let last = body("Вопрос", "bob@x.ru")
        let r = try ok(buildOutgoing(.implicit(conv(), lastIncoming: last, last: last), draft: Draft(text: "ок"), identities: ids))
        #expect(r.to == ["bob@x.ru"])
        #expect(r.subject == "Re: Вопрос")
        #expect(r.inReplyTo == "<m1@x>")
        #expect(r.references == "<m0@x> <m1@x>")
        #expect(r.from == "\"Я\" <work@b.ru>")
    }

    @Test func explicitToAndSubjectBeatDerivation() throws {
        let r = try ok(buildOutgoing(.implicit(conv(), lastIncoming: nil, last: nil), draft: Draft(text: "x", to: "solo@z.ru", subject: "Своя"), identities: ids))
        #expect(r.to == ["solo@z.ru"])
        #expect(r.subject == "Своя")
    }

    @Test func reSubjectIsNotDoubled() throws {
        let last = body("RE: тема", "bob@x.ru")
        let r = try ok(buildOutgoing(.implicit(conv(), lastIncoming: last, last: last), draft: Draft(text: "x"), identities: ids))
        #expect(r.subject == "RE: тема")
    }

    @Test func explicitReplyInGroupIsReplyAllMinusOwn() throws {
        let o = body("Тема", "bob@x.ru", to: ["Я <me@a.ru>", "carol@y.ru"], cc: ["Dan <dan@z.ru>"])
        let r = try ok(buildOutgoing(.reply(o, isGroup: true), draft: Draft(text: "x"), identities: ids))
        #expect(r.to == ["bob@x.ru", "carol@y.ru", "dan@z.ru"])
    }

    @Test func explicitReplyOneToOneGoesToSenderOnly() throws {
        let o = body("Тема", "bob@x.ru", to: ["me@a.ru", "carol@y.ru"])
        let r = try ok(buildOutgoing(.reply(o, isGroup: false), draft: Draft(text: "x"), identities: ids))
        #expect(r.to == ["bob@x.ru"])
    }

    @Test func forwardNeedsExplicitTo() throws {
        let o = body("Отчёт", "bob@x.ru")
        if case .ok = buildOutgoing(.forward(o), draft: Draft(), identities: ids) { Issue.record("без адресата ушло") }
        let r = try ok(buildOutgoing(.forward(o), draft: Draft(to: "x@y.ru"), identities: ids))
        #expect(r.subject == "Fwd: Отчёт")
        #expect(r.inReplyTo == "")
    }

    @Test func attachmentOnlyIsValidEmptyIsNot() {
        let c = Branch.implicit(conv(), lastIncoming: nil, last: nil)
        if case .ok = buildOutgoing(c, draft: Draft(), identities: ids) { Issue.record("пустое ушло") }
        let withFile = Draft(attachments: [PendingAttachment(name: "a.pdf", mime: "application/pdf", data: Data([1]))])
        guard case .ok = buildOutgoing(c, draft: withFile, identities: ids) else {
            Issue.record("письмо из одного вложения не ушло")
            return
        }
    }

    @Test func selfChatRepliesToSender() throws {
        var c = conv(receivedBy: "me@a.ru")
        c.counterparts = []
        let last = body("Себе", "me@a.ru")
        let r = try ok(buildOutgoing(.implicit(c, lastIncoming: last, last: last), draft: Draft(text: "x"), identities: ids))
        #expect(r.to == ["me@a.ru"])
    }

    @Test func textBecomesEscapedHtml() {
        #expect(textToHTML("a <b>\nc") == "<div>a &lt;b&gt;<br>c</div>")
        #expect(bareAddress("Bob <Bob@X.ru>") == "bob@x.ru")
    }

    @Test func newMailFindsItsConversationByID() {
        // Новому адресату: id «мой|его», как у сервера и ConversationBuilder.
        let t = SentTarget(from: "\"Я\" <me@a.ru>", to: ["Bob@X.ru"], cc: [], own: ids)
        #expect(t.id == "me@a.ru|bob@x.ru")
        var c = conv()
        c.id = "me@a.ru|bob@x.ru"
        #expect(t.matches(c))
        // Тот же собеседник, но другой набор (группа) — не она.
        var group = conv(group: true)
        group.id = "me@a.ru|group:bob@x.ru,carol@y.ru"
        #expect(!t.matches(group))
        // Спам с тем же набором — не она.
        c.spam = true
        #expect(!t.matches(c))
    }

    @Test func newMailToOwnOtherAddressIsSelfConversation() {
        // Письмо с одного своего адреса на другой: собеседник у сервера —
        // отправитель, набор собеседников адресатам не равен; ловим по id.
        let t = SentTarget(from: "me@a.ru", to: ["work@b.ru"], cc: [], own: ids)
        #expect(t.id == "me@a.ru+work@b.ru|self")
        let selfChat = Conversation(accountKey: "k", id: "me@a.ru+work@b.ru|self", counterparts: [ContactInfo(addr: "me@a.ru")])
        #expect(t.matches(selfChat))
        let same = SentTarget(from: "me@a.ru", to: ["me@a.ru"], cc: [], own: ids)
        #expect(same.id == "me@a.ru|self")
    }

    @Test func newMailFallsBackToCounterparts() {
        // id иной (сервер приписал другой свой адрес), собеседник тот же.
        let t = SentTarget(from: "me@a.ru", to: ["bob@x.ru"], cc: [], own: ids)
        var c = conv()
        c.id = "work@b.ru|bob@x.ru"
        #expect(t.matches(c))
    }

    @Test func richBodyAndInlineImages() throws {
        let img = PendingAttachment(name: "a.png", mime: "image/png", data: Data([1, 2]), contentID: "img1@ddmail")
        let d = Draft(text: "жирно", html: "<div><b>жирно</b></div>", images: [img])
        #expect(d.canSend)
        #expect(d.bodyHTML == "<div><b>жирно</b></div><div><img src=\"cid:img1@ddmail\" alt=\"a.png\"></div>")
        let r = try ok(buildOutgoing(.implicit(conv(), lastIncoming: nil, last: nil), draft: d, identities: ids))
        #expect(r.text == "жирно")
        #expect(r.html.hasPrefix("<div><b>жирно</b></div>"))
        #expect(r.attachments.first?.contentID == "img1@ddmail")
        // Одна картинка без текста — тоже письмо.
        #expect(Draft(images: [img]).canSend)
        // Без форматирования — HTML из текста.
        #expect(Draft(text: "a\nb").bodyHTML == "<div>a<br>b</div>")
    }

    @Test func forwardBlockHasHeaders() {
        let o = MessageBody(uid: 1, subject: "Отчёт", from: "Bob", fromAddr: "bob@x.ru", to: ["me@a.ru"], dateTs: 0, text: "тело")
        let f = forwardBlock(o, timeZone: TimeZone(identifier: "UTC")!)
        #expect(f.text.contains("От: Bob <bob@x.ru>"))
        #expect(f.text.contains("Дата: 01.01.1970 00:00"))
        #expect(f.text.hasSuffix("тело"))
        #expect(f.html.contains("Пересылаемое сообщение"))
    }
}
