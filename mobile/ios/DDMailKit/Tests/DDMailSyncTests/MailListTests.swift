import DDMailAPI
import Testing
@testable import DDMailSync

struct MailListTests {
    private func c(_ id: String, _ label: String, _ addr: String, _ subj: String, unread: Int = 0, spam: Bool = false) -> Conversation {
        Conversation(
            accountKey: "", id: id, label: label, receivedBy: "me@a.ru",
            counterparts: [ContactInfo(name: label, addr: addr)],
            lastSubject: subj, unreadCount: unread, totalCount: 1, spam: spam
        )
    }

    private var all: [Conversation] {
        [
            c("1", "Отчёт бот", "bot@x.ru", "Привет"),
            c("2", "Иван", "otchet@y.ru", "Тема"),
            c("3", "Пётр", "p@z.ru", "Квартальный ОТЧЁТ"),
        ]
    }

    /// Векторы `SearchTest.kt` Android.
    @Test func nameThenAddressThenSubjectCaseFolded() {
        #expect(searchConversations(all, "отчёт").map(\.id) == ["1", "3"])
        #expect(searchConversations(all, "OTCHET").map(\.id) == ["2"])
    }

    @Test func yoMatchesYe() {
        #expect(searchConversations(all, "отчет").map(\.id) == ["1", "3"])
    }

    @Test func searchSkipsSpamAndIgnoresFolders() {
        let doc = FoldersDoc(folders: [DialogFolder(id: "f", name: "F", rules: FolderRules(domains: ["z.ru"]))]).withSpam()
        let convs = all + [c("s", "Отчёт спам", "s@s.ru", "", spam: true)]
        let s = buildMailList(convs, doc: doc, activeID: nil, query: "отчёт")
        #expect(s.searching)
        #expect(s.conversations.map(\.id) == ["1", "3"])
    }

    @Test func tabsHideHiddenFoldersAndSpamHasNoCount() {
        let doc = FoldersDoc(folders: [
            DialogFolder(id: "w", name: "Работа", rules: FolderRules(domains: ["x.ru"])),
            DialogFolder(id: "h", name: "Скрытая", rules: FolderRules(domains: ["y.ru"]), hidden: true),
        ]).withSpam()
        let convs = [
            c("1", "a", "a@x.ru", "", unread: 1),
            c("2", "b", "b@y.ru", "", unread: 1),
            c("3", "c", "c@q.ru", "", unread: 1),
            c("s", "s", "s@s.ru", "", unread: 1, spam: true),
        ]
        let inbox = buildMailList(convs, doc: doc, activeID: nil, query: nil)
        #expect(inbox.tabs.map(\.id) == ["w"])
        #expect(inbox.tabs[0].unread == 1)
        #expect(inbox.inboxUnread == 1)
        #expect(inbox.conversations.map(\.id) == ["3"])

        // В «Спам» привели из меню — его таб виден, но без счётчика.
        let spam = buildMailList(convs, doc: doc, activeID: "spam", query: nil)
        #expect(spam.inSpam)
        #expect(spam.tabs.map(\.id) == ["w", "spam"])
        #expect(spam.tabs[1].unread == 0)
        #expect(spam.conversations.map(\.id) == ["s"])

        // Удалённая папка — снова инбокс.
        #expect(buildMailList(convs, doc: doc, activeID: "gone", query: nil).active == nil)
    }
}
