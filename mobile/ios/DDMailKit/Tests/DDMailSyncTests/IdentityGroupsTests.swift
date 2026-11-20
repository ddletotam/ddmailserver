import DDMailAPI
import Foundation
import Testing
@testable import DDMailSync

/// Векторы — те же, что у `identities.rs` десктопа (§1в) и `IdentityGroupsTest.kt` Android.
struct IdentityGroupsTests {
    private let doc = IdentityGroups.parse(#"""
    {"default_group":"g2","sig_extra":1,"groups":[
      {"id":"g1","name":"Работа","color":"#E3F2FD","sender":"boss@work.example","strict":true,
       "addresses":[{"email":"me@work.example"},{"email":"boss@work.example"}]},
      {"id":"g2","name":"","color":"","sender":"","addresses":[{"email":"me@home.example"},{"email":"old@home.example","hidden":true}]},
      {"id":"g3","name":"Рассылки","color":"","sender":"lists@home.example","addresses":[{"email":"lists@home.example"}]}
    ]}
    """# as String?)

    @Test func replyFromGroupSenderEvenIfReceivedOnOtherAddress() {
        #expect(doc.replySender("me@work.example") == "boss@work.example")
        #expect(doc.replySender("ME@work.example") == "boss@work.example")
        #expect(doc.replySender("me+news@work.example") == "boss@work.example")
    }

    @Test func notMineGoesFromDefaultGroup() {
        #expect(doc.replySender("stranger@x.example") == "me@home.example")
        #expect(doc.replySender("") == "me@home.example")
        #expect(doc.newMailSender() == "me@home.example")
        #expect(doc.autoSender(nil) == "me@home.example")
    }

    @Test func strictDefaultIsSkippedForForeignMail() {
        let strictDefault = doc.withDefault("g1")
        #expect(strictDefault.newMailSender() == "boss@work.example")
        #expect(strictDefault.replySender("x@y.example") == "me@home.example")
        let allStrict = IdentityGroups.parse(#"{"groups":[{"id":"g1","strict":true,"addresses":[{"email":"a@b.example"}]}]}"# as String?)
        #expect(allStrict.replySender("x@y.example") == nil)
    }

    @Test func strictGroupOnlyForOwnMailOrNew() {
        #expect(doc.allowed("boss@work.example", context: nil))
        #expect(doc.allowed("boss@work.example", context: "me@work.example"))
        #expect(!doc.allowed("boss@work.example", context: "me@home.example"))
        #expect(!doc.allowed("boss@work.example", context: ""))
        #expect(doc.allowed("me@home.example", context: "me@work.example"))
        #expect(doc.allowed("unknown@x.example", context: "me@work.example"))
    }

    @Test func hiddenNotOffered() {
        #expect(!doc.offered("old@home.example", context: nil))
        #expect(doc.offered("me@home.example", context: nil))
        #expect(!doc.offered("me@work.example", context: "me@home.example"))
    }

    @Test func defaultFallsBackToFirstGroup() {
        #expect(doc.withDefault("nope").defaultGroup?.id == "g1")
        #expect(doc.defaultGroup?.id == "g2")
    }

    @Test func titleAndSender() {
        #expect(doc.groups[0].title == "Работа · boss@work.example")
        #expect(doc.groups[1].title == "me@home.example")
    }

    @Test func withDefaultKeepsOtherFields() {
        let j = doc.withDefault("g3").json
        #expect(j["default_group"]?.string == "g3")
        #expect(j["sig_extra"] == .int(1))
        #expect(j["groups"] == doc.json["groups"])
    }

    @Test func mergeAddsNewAddressesAsOwnGroups() {
        let merged = doc.merged([
            .init(email: "Me@Home.example"),
            .init(email: "new@mail.example", name: "Новый"),
            .init(email: "not an email"),
        ])
        #expect(merged.groups.count == 4)
        let g = merged.groups.last!
        #expect(g.id == "g4")
        #expect(g.name == "Новый")
        #expect(g.sender == "new@mail.example")
        #expect(g.color == "#FFE4E1")
        #expect(merged.defaultGroupID == "g2")
        #expect(merged.merged([.init(email: "new@mail.example")]) == merged)
    }

    @Test func mergeIntoEmptySetsDefault() {
        let m = IdentityGroups.empty.merged([.init(email: "a@b.example"), .init(email: "c@d.example")])
        #expect(m.groups.map(\.id) == ["g1", "g2"])
        #expect(m.defaultGroupID == "g1")
        #expect(m.groups.map(\.color) == ["#FFE4E1", "#E8F5E9"])
        #expect(m.colors.color(of: "a@b.example") == IdentityColors.pastel[0])
    }

    @Test func emptyOrBrokenDoc() {
        #expect(IdentityGroups.parse(nil as String?).isEmpty)
        #expect(IdentityGroups.parse("[]" as String?).isEmpty)
        #expect(IdentityGroups.empty.newMailSender() == nil)
    }
}

/// Выбор отправителя и тема — векторы `OutgoingTest.kt` Android.
struct OutgoingSenderTests {
    private let ids = [
        Identity(email: "me@a.ru", name: "Я", isDefault: true), Identity(email: "work@b.ru", name: "Я"),
        Identity(email: "boss@b.ru", name: "Шеф"),
    ]
    private let groups = IdentityGroups.parse(#"""
    {"default_group":"g2","groups":[
      {"id":"g1","name":"Работа","sender":"boss@b.ru","strict":true,"addresses":[{"email":"work@b.ru"},{"email":"boss@b.ru"}]},
      {"id":"g2","name":"Я","sender":"me@a.ru","addresses":[{"email":"me@a.ru"}]}]}
    """# as String?)

    private func conv(receivedBy: String = "work@b.ru", subject: String = "Привет") -> Conversation {
        Conversation(
            accountKey: "k", id: "c", label: "Bob", receivedBy: receivedBy,
            counterparts: [ContactInfo(name: "Bob", addr: "bob@x.ru")], isGroup: false, lastSubject: subject, totalCount: 2
        )
    }

    private func body(_ subject: String, _ from: String) -> MessageBody {
        MessageBody(uid: 1, subject: subject, fromAddr: from, messageID: "<m1@x>", references: [])
    }

    private func ok(_ b: Built) throws -> SendRequest {
        guard case let .ok(r, _) = b else { Issue.record("отказ: \(b)"); throw CancellationError() }
        return r
    }

    @Test func replyGoesFromGroupSenderOfReceivingAddress() throws {
        let last = body("Вопрос", "bob@x.ru")
        let r = try ok(buildOutgoing(.implicit(conv(), lastIncoming: last, last: last), draft: Draft(text: "ок"), identities: ids, groups: groups))
        #expect(r.from == "\"Шеф\" <boss@b.ru>")
        let e = try ok(buildOutgoing(.reply(last, isGroup: false), draft: Draft(text: "ок"), identities: ids, groups: groups, receivedBy: "work@b.ru"))
        #expect(e.from == "\"Шеф\" <boss@b.ru>")
    }

    @Test func newMailAndForeignMailGoFromDefaultGroup() throws {
        let n = try ok(buildOutgoing(.new, draft: Draft(text: "x", to: "z@z.ru"), identities: ids, groups: groups))
        #expect(n.from == "\"Я\" <me@a.ru>")
        let last = body("Рассылка", "list@x.ru")
        let r = try ok(buildOutgoing(.implicit(conv(receivedBy: "list@x.ru"), lastIncoming: last, last: last), draft: Draft(text: "x"), identities: ids, groups: groups))
        #expect(r.from == "\"Я\" <me@a.ru>")
        let w = try ok(buildOutgoing(.new, draft: Draft(text: "x", to: "z@z.ru"), identities: ids, groups: groups.withDefault("g1")))
        #expect(w.from == "\"Шеф\" <boss@b.ru>")
    }

    @Test func strictGroupRefusesForeignContext() throws {
        let last = body("Личное", "bob@x.ru")
        if case .ok = buildOutgoing(.implicit(conv(receivedBy: "me@a.ru"), lastIncoming: last, last: last),
                                    draft: Draft(text: "x", from: "work@b.ru"), identities: ids, groups: groups) {
            Issue.record("строгая группа ответила на чужое письмо")
        }
        let f = try ok(buildOutgoing(.implicit(conv(receivedBy: "list@x.ru"), lastIncoming: last, last: last),
                                     draft: Draft(text: "x"), identities: ids, groups: groups.withDefault("g1")))
        #expect(f.from == "\"Я\" <me@a.ru>")
        let choices = FromChoices([ids]) { groups.offered($0, context: "me@a.ru") }
        #expect(choices.all.map(\.email) == ["me@a.ru"])
    }

    @Test func subjectStaysEmptyWithoutBaseSubject() throws {
        let n = try ok(buildOutgoing(.new, draft: Draft(text: "x", to: "z@z.ru"), identities: ids))
        #expect(n.subject == "")
        let last = body("", "bob@x.ru")
        let r = try ok(buildOutgoing(.implicit(conv(subject: ""), lastIncoming: last, last: last), draft: Draft(text: "x"), identities: ids))
        #expect(r.subject == "")
    }
}
