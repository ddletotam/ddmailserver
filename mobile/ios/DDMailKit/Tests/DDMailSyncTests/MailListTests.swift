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

    /// «Спам» скрыт по умолчанию, виден с «Показать скрытые» (бледнее, без
    /// счётчика) или когда в него привели — как прочие скрытые (десктоп §4).
    @Test func spamHiddenByDefaultShownWithShowHidden() {
        var doc = FoldersDoc(folders: [
            DialogFolder(id: "w", name: "Работа", rules: FolderRules(domains: ["x.ru"])),
            DialogFolder(id: "h", name: "Скрытая", rules: FolderRules(domains: ["y.ru"]), hidden: true),
        ]).withSpam()
        // Другой клиент снял «скрыта» со «Спама» — нормализация чинит.
        doc.folders = doc.folders.map { var f = $0; if f.isSystem { f.hidden = false }; return f }
        doc = doc.withSpam()
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
        #expect(doc.hiddenCount == 2)

        // «Показать скрытые» — табы скрытых видны, бледнее; у «Спама» счётчика нет.
        let shown = buildMailList(convs, doc: doc, activeID: nil, query: nil, showHidden: true)
        #expect(shown.tabs.map(\.id) == ["w", "h", "spam"])
        #expect(shown.tabs[1].hidden)
        #expect(shown.tabs[2].hidden && shown.tabs[2].system)
        #expect(shown.tabs[2].unread == 0)

        // Привели в «Спам» — таб виден и без «Показать скрытые».
        let spam = buildMailList(convs, doc: doc, activeID: "spam", query: nil)
        #expect(spam.inSpam)
        #expect(spam.tabs.map(\.id) == ["w", "spam"])
        #expect(spam.conversations.map(\.id) == ["s"])

        // Папки выключены — списка как без папок: спама нет нигде.
        let off = buildMailList(convs, doc: FoldersDoc(), activeID: "spam", query: nil, showHidden: true)
        #expect(off.tabs.isEmpty)
        #expect(off.active == nil)
        #expect(off.conversations.map(\.id) == ["1", "2", "3"])

        // Удалённая папка — снова инбокс.
        #expect(buildMailList(convs, doc: doc, activeID: "gone", query: nil).active == nil)
    }

    @Test func hitsSayWhatMatched() {
        let hits = searchConversationHits(all, "отчёт")
        #expect(hits.map(\.conversation.id) == ["1", "3"])
        #expect(hits.map(\.matched) == ["bot@x.ru", "Квартальный ОТЧЁТ"])
        #expect(searchConversationHits(all, "otchet").map(\.matched) == ["otchet@y.ru"])
        #expect(searchConversationHits(all, "  ").isEmpty)
        let bare = c("9", "Bob@X.ru", "bob@x.ru", "")
        #expect(searchConversationHits([bare], "bob").map(\.matched) == [""])
    }

    /// Векторы `parse_email_like` десктопа.
    @Test func emailLike() {
        #expect(parseEmailLike(" Ivan@Mail.RU ") == "ivan@mail.ru")
        #expect(parseEmailLike("ivan@mail") == nil)
        #expect(parseEmailLike("ivan@.ru") == nil)
        #expect(parseEmailLike("ivan@mail.") == nil)
        #expect(parseEmailLike("@mail.ru") == nil)
        #expect(parseEmailLike("ivan petrov@mail.ru") == nil)
        #expect(parseEmailLike("<ivan@mail.ru>") == nil)
        #expect(parseEmailLike("иван") == nil)
    }
}
