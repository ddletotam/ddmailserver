import DDMailAPI
import Testing
@testable import DDMailSync

struct NotificationPlanTests {
    private func c(_ id: String, _ uids: [Int64], unseen: Set<Int64> = [], domain: String = "x.ru", spam: Bool = false) -> Conversation {
        Conversation(
            accountKey: "a", id: id, label: id, receivedBy: "me@a.ru",
            counterparts: [ContactInfo(addr: "\(id)@\(domain)")], lastSubject: "тема",
            unreadCount: unseen.count, totalCount: uids.count,
            messages: uids.map { MessageRef(uid: $0, seen: !unseen.contains($0)) }, spam: spam
        )
    }

    private let doc = FoldersDoc(folders: [
        DialogFolder(id: "h", name: "Скрытая", rules: FolderRules(domains: ["hidden.ru"]), hidden: true),
    ]).withSpam()

    /// Первый проход только запоминает рубеж: после входа не сыплем уведомлениями.
    @Test func firstPassOnlyRemembers() {
        let p = NotificationPlan(conversations: [c("bob", [1, 2], unseen: [2])], folders: doc, openRef: nil, previous: [:])
        #expect(p.items.isEmpty)
        #expect(p.watermarks == ["a": 2])
        #expect(p.badge == 1)
    }

    @Test func onlyNewUnseenOutsideOpenAndHidden() {
        let convs = [
            c("bob", [1, 5, 6], unseen: [5, 6]),
            c("ann", [7], unseen: [7]),
            c("old", [3], unseen: [3]),
            c("sec", [8], unseen: [8], domain: "hidden.ru"),
            c("spm", [9], unseen: [9], spam: true),
        ]
        let p = NotificationPlan(conversations: convs, folders: doc, openRef: Conversation.ref("a", "ann"), previous: ["a": 4])
        #expect(p.items.map(\.conversation.id) == ["bob"])
        #expect(p.items.first?.count == 2)
        // Скрытая папка и спам в бейдж не идут; открытая беседа — идёт.
        #expect(p.badge == 3)
        // Рубеж — по известным письмам вне спама.
        #expect(p.watermarks == ["a": 8])
        #expect(NotificationPlan.text(p.items[0]).body == "тема · новых: 2")
    }

    @Test func emptyCacheKeepsWatermark() {
        let p = NotificationPlan(conversations: [], folders: doc, openRef: nil, previous: ["a": 40])
        #expect(p.watermarks == ["a": 40])
        #expect(p.items.isEmpty)
    }

    /// У каждой учётки своя нумерация: рубеж одной не глушит другую, а
    /// новая учётка в первом проходе только запоминает рубеж.
    @Test func watermarksArePerAccount() {
        var big = c("x", [1000], unseen: [1000])
        big.accountKey = "ddmail"
        var small = c("y", [5, 6], unseen: [6])
        small.accountKey = "imap"
        var fresh = c("z", [3], unseen: [3])
        fresh.accountKey = "new"
        let p = NotificationPlan(conversations: [big, small, fresh], folders: doc, openRef: nil, previous: ["ddmail": 1000, "imap": 5])
        #expect(p.items.map(\.conversation.ref) == [small.ref])
        #expect(p.watermarks == ["ddmail": 1000, "imap": 6, "new": 3])
    }
}
