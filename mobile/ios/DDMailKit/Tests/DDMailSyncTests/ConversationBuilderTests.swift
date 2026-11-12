import DDMailAPI
import Testing
@testable import DDMailSync

/// Порт тестов `conversation_key_tests` десктопа (`imap.rs`) и сценариев
/// `groupConversations` сервера: id обязаны совпадать байт в байт.
struct ConversationKeyTests {
    typealias B = ConversationBuilder

    @Test func participantsIgnoreRoles() {
        let a = B.participants(from: "bob@x.ru", to: ["me@a.ru"], cc: ["carol@x.ru"])
        let b = B.participants(from: "me@a.ru", to: ["carol@x.ru"], cc: ["bob@x.ru"])
        #expect(a == b)
        #expect(a == ["bob@x.ru", "carol@x.ru", "me@a.ru"])
    }

    @Test func participantsNormaliseCaseAndDedup() {
        #expect(B.participants(from: "  Bob@X.RU ", to: ["me@a.ru", "bob@x.ru"], cc: [""]) == ["bob@x.ru", "me@a.ru"])
    }

    @Test func differentSetIsADifferentConversation() {
        #expect(B.participants(from: "bob@x.ru", to: ["me@a.ru"], cc: []) != B.participants(from: "bob@x.ru", to: ["me@a.ru", "carol@x.ru"], cc: []))
    }

    @Test func idMatchesTheOldFormatForASingleIdentity() {
        #expect(B.conversationID(mine: ["me@a.ru"], others: ["bob@x.ru"], own: "me@a.ru") == "me@a.ru|bob@x.ru")
        #expect(B.conversationID(mine: ["me@a.ru"], others: ["bob@x.ru", "carol@x.ru"], own: "me@a.ru") == "me@a.ru|group:bob@x.ru,carol@x.ru")
    }

    @Test func idSeparatesConversationsThatDifferOnlyByMySecondIdentity() {
        let one = B.conversationID(mine: ["me@a.ru"], others: ["bob@x.ru"], own: "me@a.ru")
        let two = B.conversationID(mine: ["me@a.ru", "me@b.ru"], others: ["bob@x.ru"], own: "me@a.ru")
        #expect(one != two)
        #expect(two == "me@a.ru+me@b.ru|bob@x.ru")
    }

    @Test func idForMailBetweenMyOwnIdentities() {
        #expect(B.conversationID(mine: ["me@a.ru", "me@b.ru"], others: [], own: "me@a.ru") == "me@a.ru+me@b.ru|self")
    }

    @Test func idWhenIAmNotAmongTheVisibleAddresses() {
        #expect(B.conversationID(mine: [], others: ["bob@x.ru"], own: "me@a.ru") == "me@a.ru|bob@x.ru")
    }

    @Test func idKeepsTheEmptyFormWhenNoIdentityIsKnown() {
        #expect(B.conversationID(mine: [], others: ["bob@x.ru"], own: "") == "|bob@x.ru")
    }

    @Test func gravatar() {
        #expect(B.gravatarHash(" Test@Example.com ") == "55502f40dc8b7c769880b10874abc9d0")
    }
}

struct ConversationBuildTests {
    typealias B = ConversationBuilder
    let ours: Set<String> = ["me@a.ru"]

    func msg(_ uid: Int64, from: String, to: [String], cc: [String] = [], subject: String = "тема", date: Int64,
             folder: String = "INBOX", sent: Bool = false, mid: String = "", seen: Bool = true, draft: Bool = false,
             fromName: String = "", toNames: [String] = []) -> HeaderMessage {
        HeaderMessage(
            folder: folder, isSent: sent, uid: uid, messageID: mid, fromName: fromName, fromAddr: from,
            to: to.enumerated().map { (i, a) in (i < toNames.count ? toNames[i] : "", a) }, cc: cc.map { ("", $0) },
            subject: subject, dateTs: date, seen: seen, draft: draft
        )
    }

    /// Ровно тот случай, ради которого группировка — по id: входящее по bcc
    /// (нас в заголовках нет) и наш ответ — одна беседа, а не две строки с одним id.
    @Test func bccIncomingAndMyReplyAreOneConversation() {
        let convs = B.build([
            msg(1, from: "bob@x.ru", to: ["list@x.ru"], date: 100, mid: "a"),
            msg(2, from: "me@a.ru", to: ["bob@x.ru", "list@x.ru"], date: 200, folder: "Sent", sent: true, mid: "b"),
        ], ours: ours, own: "me@a.ru")
        #expect(convs.count == 1)
        #expect(convs[0].id == "me@a.ru|group:bob@x.ru,list@x.ru")
        #expect(convs[0].receivedBy == "me@a.ru")
        #expect(convs[0].messages.map(\.uid) == [1, 2])
        // В наборе и наш адрес — как у сервера: «тема (3 чел)».
        #expect(convs[0].label == "тема (3 чел)")
    }

    @Test func directConversationRolesNamesAndCounts() {
        let convs = B.build([
            msg(1, from: "bob@x.ru", to: ["me@a.ru"], date: 100, mid: "a", seen: false, fromName: "Боб Петров"),
            msg(2, from: "me@a.ru", to: ["bob@x.ru"], subject: "Re: тема", date: 200, folder: "Sent", sent: true, mid: "b"),
            msg(3, from: "bob@x.ru", to: ["me@a.ru"], subject: "Re: тема", date: 300, mid: "c", seen: false),
        ], ours: ours, own: "me@a.ru")
        #expect(convs.count == 1)
        let c = convs[0]
        #expect(c.id == "me@a.ru|bob@x.ru")
        #expect(c.label == "Боб Петров")
        #expect(c.counterparts == [ContactInfo(name: "Боб Петров", addr: "bob@x.ru")])
        #expect(c.unreadCount == 2)
        #expect(c.totalCount == 3)
        #expect(c.lastDateTs == 300)
        #expect(c.lastSubject == "Re: тема")
        #expect(c.avatarHash == B.gravatarHash("bob@x.ru"))
        #expect(!c.isGroup)
    }

    /// Письмо себе лежит и во «Входящих», и в «Отправленных» — одна копия, из «Отправленных».
    @Test func duplicatesByMessageIDPreferSent() {
        let convs = B.build([
            msg(5, from: "me@a.ru", to: ["me@a.ru"], date: 100, mid: "self1"),
            msg(9, from: "me@a.ru", to: ["me@a.ru"], date: 100, folder: "Sent", sent: true, mid: "self1"),
        ], ours: ours, own: "me@a.ru")
        #expect(convs.count == 1)
        #expect(convs[0].id == "me@a.ru|self")
        #expect(convs[0].messages.map(\.folder) == ["Sent"])
        #expect(convs[0].label == "me@a.ru")
    }

    @Test func draftsAloneDoNotMakeAConversation() {
        let convs = B.build([
            msg(1, from: "me@a.ru", to: ["new@x.ru"], date: 100, folder: "Drafts", draft: true),
            msg(2, from: "bob@x.ru", to: ["me@a.ru"], date: 50),
            msg(3, from: "me@a.ru", to: ["bob@x.ru"], date: 60, folder: "Drafts", draft: true),
        ], ours: ours, own: "me@a.ru")
        #expect(convs.map(\.id) == ["me@a.ru|bob@x.ru"])
        #expect(convs[0].messages.map(\.uid) == [2])
        #expect(convs[0].lastDateTs == 50)
    }

    /// Беседа начата с моего второго адреса: айдентика — та, что в первом письме.
    @Test func myIdentityComesFromTheFirstMessage() {
        let ours: Set<String> = ["me@a.ru", "alias@a.ru"]
        let convs = B.build([
            msg(1, from: "bob@x.ru", to: ["alias@a.ru"], date: 100),
            msg(2, from: "alias@a.ru", to: ["bob@x.ru"], date: 200, folder: "Sent", sent: true),
        ], ours: ours, own: "me@a.ru")
        #expect(convs.map(\.id) == ["alias@a.ru|bob@x.ru"])
        #expect(convs[0].receivedBy == "alias@a.ru")
    }

    @Test func betweenMyOwnIdentities() {
        let ours: Set<String> = ["me@a.ru", "me@b.ru"]
        let convs = B.build([msg(1, from: "me@b.ru", to: ["me@a.ru"], date: 100)], ours: ours, own: "me@a.ru")
        #expect(convs[0].id == "me@a.ru+me@b.ru|self")
        #expect(convs[0].counterparts.map(\.addr) == ["me@b.ru"])
        #expect(convs[0].receivedBy == "me@a.ru")
    }

    @Test func groupWithoutSubjectAndNameFromRecipients() {
        let convs = B.build([
            msg(1, from: "me@a.ru", to: ["bob@x.ru", "ann@y.ru"], subject: " ", date: 100, folder: "Sent", sent: true,
                toNames: ["Bob <bob@x.ru>", "ann@y.ru"]),
        ], ours: ours, own: "me@a.ru")
        #expect(convs[0].label == "(без темы) (3 чел)")
        #expect(convs[0].id == "me@a.ru|group:ann@y.ru,bob@x.ru")
        #expect(convs[0].counterparts == [ContactInfo(name: "", addr: "ann@y.ru"), ContactInfo(name: "Bob", addr: "bob@x.ru")])
        #expect(convs[0].isGroup)
    }

    @Test func spamPrefixAndOrderByDate() {
        let convs = B.build([
            msg(1, from: "a@x.ru", to: ["me@a.ru"], date: 100),
            msg(2, from: "b@x.ru", to: ["me@a.ru"], date: 300),
        ], ours: ours, own: "me@a.ru", idPrefix: "spam:", spam: true)
        #expect(convs.map(\.id) == ["spam:me@a.ru|b@x.ru", "spam:me@a.ru|a@x.ru"])
        #expect(convs.allSatisfy { $0.spam })
    }
}
