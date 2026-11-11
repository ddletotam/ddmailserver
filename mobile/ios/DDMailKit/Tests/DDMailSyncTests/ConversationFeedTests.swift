import DDMailAPI
import Foundation
import Testing
@testable import DDMailSync

struct RemoteImageGateTests {
    private let image = ["Accept": "image/webp,image/avif,*/*"]

    @Test func hamPassesEverything() {
        let g = RemoteImageGate()
        g.open(spam: false, uids: [1])
        #expect(g.permits(uid: nil, headers: [:]))
    }

    /// Спам: только письмо, которому нажали «Показать картинки», и только картинки.
    @Test func spamOnlyAllowedImages() {
        let g = RemoteImageGate()
        g.open(spam: true, uids: [1, 2])
        #expect(!g.permits(uid: 1, headers: image))
        g.allow(1)
        #expect(g.permits(uid: 1, headers: image))
        #expect(!g.permits(uid: 2, headers: image))
        #expect(!g.permits(uid: 1, headers: ["Accept": "text/css,*/*;q=0.1"]))
        #expect(!g.permits(uid: 1, headers: ["Sec-Fetch-Dest": "font", "Accept": "image/png"]))
        // Чужой uid разрешить нельзя; новая беседа забывает разрешения.
        g.allow(99)
        #expect(!g.permits(uid: 99, headers: image))
        g.open(spam: true, uids: [1])
        #expect(!g.permits(uid: 1, headers: image))
    }
}

struct FeedTests {
    private func refs(_ uids: [Int64], unseen: Set<Int64> = []) -> [MessageRef] {
        uids.map { MessageRef(uid: $0, seen: !unseen.contains($0)) }
    }

    @Test func windowShowsLatestFirstThenOlder() {
        var w = FeedWindow(refs(Array(1...45), unseen: [30, 41]))
        #expect(w.first.map(\.uid) == Array((26...45).reversed()))
        #expect(w.hasOlder)
        #expect(w.focus == 30)
        #expect(w.older().map(\.uid) == Array((6...25).reversed()))
        #expect(w.older().map(\.uid) == Array((1...5).reversed()))
        #expect(!w.hasOlder)
        #expect(w.older().isEmpty)
    }

    @Test func freshAddsOnlyNew() {
        var w = FeedWindow(refs([1, 2, 3]))
        #expect(w.fresh(refs([1, 2, 3, 4])).map(\.uid) == [4])
        #expect(w.fresh(refs([1, 2, 3, 4])).isEmpty)
        #expect(!w.hasOlder)
    }

    @Test func batchRespectsSpamAndSenders() throws {
        let ham = Conversation(accountKey: "a", id: "c", isGroup: true)
        var spam = ham
        spam.spam = true
        let body = MessageBody(uid: 7, fromAddr: "Bob@X.ru", html: "  ", text: "hi\u{2028}there")
        let a = FeedBatch(conversation: ham, bodies: [body], hasOlder: true, focusUid: 7, imageSenders: ["bob@x.ru"])
        let b = FeedBatch(conversation: spam, bodies: [body], hasOlder: false, focusUid: nil, imageSenders: ["bob@x.ru"])
        let ja = try #require(JSONValue.parse(Data(a.javaScriptLiteral().utf8)))
        let jb = try #require(JSONValue.parse(Data(b.javaScriptLiteral().utf8)))
        #expect(ja["messages"]?.array?.first?["imagesAllowed"] == .bool(true))
        #expect(ja["messages"]?.array?.first?["showFrom"] == .bool(true))
        // Пустой HTML не отдаём — документ возьмёт текст.
        #expect(ja["messages"]?.array?.first?["html"] == nil)
        #expect(jb["messages"]?.array?.first?["imagesAllowed"] == .bool(false))
        #expect(a.javaScriptLiteral().contains("\\u2028"))
        #expect(!a.javaScriptLiteral().contains("\u{2028}"))
    }

    @Test func fileNamesAreSafe() {
        #expect(safeFileName("../../etc/passwd") == ".._.._etc_passwd")
        #expect(safeFileName("отчёт: Q3?.pdf") == "отчёт_ Q3_.pdf")
        #expect(safeFileName(" . ") == "attachment")
        #expect(safeFileName("a\u{0}b.") == "a_b")
    }
}
