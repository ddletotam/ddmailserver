import DDMailAPI
import Foundation
import Testing
@testable import DDMailSync

/// Те же случаи, что `DialogFoldersTest.kt` Android и `folders.rs` десктопа:
/// смысл документа обязан совпадать.
struct DialogFoldersTests {
    private func conv(_ id: String, _ receivedBy: String, _ cps: [String], group: Bool = false, unread: Int = 0, spam: Bool = false) -> Conversation {
        Conversation(
            accountKey: "", id: id, label: id, receivedBy: receivedBy,
            counterparts: cps.map { ContactInfo(addr: $0) }, isGroup: group,
            unreadCount: unread, totalCount: 1, spam: spam
        )
    }

    private func folder(_ id: String, _ rules: FolderRules = FolderRules(), include: [DialogKey] = [], exclude: [DialogKey] = []) -> DialogFolder {
        DialogFolder(id: id, name: id, rules: rules, include: include, exclude: exclude)
    }

    private func key(_ id: String) -> DialogKey { DialogKey(account: "", id: id) }

    @Test func noRulesNoIncludeMatchesNothing() {
        let doc = FoldersDoc(folders: [folder("a")])
        #expect(doc.folderOf(conv("x", "me@a", ["bob@b"])) == nil)
    }

    @Test func rulesAndBetweenCategoriesOrWithin() {
        let doc = FoldersDoc(folders: [folder("a", FolderRules(identities: ["me@a", "me@c"], domains: ["acme.com"]))])
        #expect(doc.folderOf(conv("1", "me@a", ["bob@mail.acme.com"])) == 0)
        #expect(doc.folderOf(conv("2", "ME@C", ["x@acme.com"])) == 0)
        #expect(doc.folderOf(conv("3", "me@a", ["bob@notacme.com"])) == nil)
        #expect(doc.folderOf(conv("4", "me@z", ["bob@acme.com"])) == nil)
    }

    @Test func kindRule() {
        let doc = FoldersDoc(folders: [folder("g", FolderRules(kind: .group))])
        #expect(doc.folderOf(conv("1", "me", ["a", "b"], group: true)) == 0)
        #expect(doc.folderOf(conv("2", "me", ["a"])) == nil)
    }

    @Test func firstMatchingFolderWinsIncludeBeatsRules() {
        let r = FolderRules(domains: ["acme.com"])
        let c = conv("1", "me", ["bob@acme.com"])
        #expect(FoldersDoc(folders: [folder("a", r), folder("b", r)]).folderOf(c) == 0)
        let doc = FoldersDoc(folders: [folder("a", r), folder("b", r), folder("c", include: [key("1")])])
        #expect(doc.folderOf(c) == 2)
    }

    @Test func excludeDropsToNextMatchingOrInbox() {
        let r = FolderRules(domains: ["acme.com"])
        let c = conv("1", "me", ["bob@acme.com"])
        #expect(FoldersDoc(folders: [folder("a", r, exclude: [key("1")]), folder("b", r)]).folderOf(c) == 1)
        #expect(FoldersDoc(folders: [folder("a", r, exclude: [key("1")])]).folderOf(c) == nil)
    }

    @Test func assignMovesStrictlyAndToInbox() {
        let r = FolderRules(domains: ["acme.com"])
        let c = conv("1", "me", ["bob@acme.com"])
        var doc = FoldersDoc(folders: [folder("a", r), folder("b")])
        #expect(doc.folderOf(c) == 0)
        doc = doc.assign([c], to: 1)
        #expect(doc.folderOf(c) == 1)
        doc = doc.assign([c], to: nil)
        #expect(doc.folderOf(c) == nil, "в инбокс, хотя правила папки a его ловят")
        doc = doc.assign([c], to: 0)
        #expect(doc.folderOf(c) == 0)
        #expect(doc.folders[0].include.count == 1)
        #expect(doc.folders[0].exclude.isEmpty)
    }

    @Test func bulkAssignIsOneEdit() {
        let cs = [conv("1", "me", ["a@x"]), conv("2", "me", ["b@y"])]
        let doc = FoldersDoc(folders: [folder("a")]).assign(cs, to: 0)
        #expect(doc.folders[0].include == [key("1"), key("2")])
    }

    @Test func filterAndCounts() {
        let doc = FoldersDoc(folders: [folder("f", FolderRules(domains: ["acme.com"]))])
        let all = [
            conv("a", "me", ["x@acme.com"], unread: 2),
            conv("b", "me", ["y@acme.com"]),
            conv("g", "me", ["z@other.ru"], unread: 1),
        ]
        #expect(doc.filter(all, active: 0).map(\.id) == ["a", "b"])
        #expect(doc.filter(all, active: nil).map(\.id) == ["g"])
        let counts = doc.unreadCounts(all)
        #expect(counts.folders == [1])
        #expect(counts.inbox == 1)
    }

    @Test func unknownFieldsSurviveRoundtrip() throws {
        let src = #"""
        {"schema":2,"folders":[{"id":"f","name":"F","color":"#fff",
            "rules":{"kind":"group","future":1},"include":[{"account":"u@h","id":"a|b"}]}]}
        """#
        let doc = FoldersDoc.parse(JSONValue.parse(Data(src.utf8)))
        #expect(doc.folders[0].rules.kind == .group)
        #expect(doc.folders[0].include == [DialogKey(account: "u@h", id: "a|b")])
        let back = doc.toJSON()
        #expect(back["schema"] == .int(2))
        let f = try #require(back["folders"]?.array?.first)
        #expect(f["color"] == .string("#fff"))
        #expect(f["rules"]?["future"] == .int(1))
    }

    // Папка «Спам» — те же случаи, что `spam_lives_only_in_system_folder`
    // и `ensure_spam_is_idempotent_and_repairs` десктопа.
    @Test func spamLivesOnlyInSystemFolder() throws {
        let anyA = FolderRules(identities: ["me@a"])
        let bare = FoldersDoc(folders: [folder("a", anyA)])
        let spam = conv("spam:me@a|bad@x", "me@a", ["bad@x"], unread: 1, spam: true)
        let ham = conv("me@a|bob@b", "me@a", ["bob@b"], unread: 1)
        let all = [spam, ham]

        // Без папки «Спам» спама не видно нигде, даже если правила его ловят.
        #expect(bare.folderOf(spam) == nil)
        #expect(bare.filter(all, active: nil).isEmpty)
        #expect(bare.unreadCounts(all).folders == [1])
        #expect(bare.unreadCounts(all).inbox == 0)

        let doc = bare.withSpam()
        let si = try #require(doc.spamIndex)
        #expect(doc.folders[si].hidden)
        #expect(doc.folderOf(spam) == si)
        #expect(doc.folderOf(ham) == 0)
        #expect(doc.filter(all, active: si).map(\.id) == ["spam:me@a|bad@x"])
        #expect(doc.unreadCounts(all).folders == [1, 1])

        // Руками ни внутрь, ни наружу.
        #expect(doc.assign([ham], to: si).folderOf(ham) == 0)
        #expect(doc.assign([spam], to: nil).folderOf(spam) == si)
    }

    @Test func withSpamIsIdempotentRepairsAndRoundtrips() {
        let once = FoldersDoc().withSpam()
        #expect(once.folders == once.withSpam().folders)
        // Другой клиент снял «скрыта» и задублировал — чиним.
        var broken = once
        broken.folders = once.folders.map { var f = $0; f.hidden = false; return f } + once.folders
        let fixed = broken.withSpam()
        #expect(fixed.folders.count == 1)
        #expect(fixed.folders[0].hidden)
        // Через JSON — та же системная папка.
        let back = FoldersDoc.parse(JSONValue.parse(once.toJSON().data()))
        #expect(back.folders[0].system == systemSpam)
        #expect(back.spamIndex == 0)
        // Пользовательская папка с id "spam" не мешает.
        let clash = FoldersDoc(folders: [folder("spam")]).withSpam()
        #expect(clash.folders.count == 2)
        #expect(clash.folders[1].id != "spam")
        #expect(clash.folderOf(conv("x", "me@a", ["x@y"])) == nil)
    }

    // Правка документа из настроек и меню полосы табов (`folders_ui.rs`).
    @Test func addMoveUpdateRemoveKeepSpamLast() throws {
        let t0 = Date(timeIntervalSince1970: 1_700_000_000)
        var (doc, id) = FoldersDoc(folders: [folder("a")]).withSpam().adding(now: t0)
        #expect(id == "f18bcfe568000")
        #expect(doc.folders.map(\.id) == ["a", id, "spam"])
        #expect(doc.folders[1].name == "Папка 2")
        #expect(doc.folders[1].icon == "📁")
        // Второй в ту же миллисекунду — другой id.
        #expect(doc.newID(now: t0) == "f18bcfe568001")

        doc = doc.moving(id, to: 0)
        #expect(doc.folders.map(\.id) == [id, "a", "spam"])
        // Дальше «Спама» не уезжает, сам «Спам» не двигается.
        doc = doc.moving(id, to: 99)
        #expect(doc.folders.map(\.id) == ["a", id, "spam"])
        #expect(doc.moving("spam", to: 0).folders.map(\.id) == ["a", id, "spam"])

        doc = doc.updating(id) {
            $0.name = "Работа"
            $0.rules = FolderRules(identities: ["me@a"], domains: splitFolderList("Acme.com; x.ru"), kind: .direct)
            $0.hidden = true
        }
        #expect(doc.updating("spam") { $0.name = "x" }.folders[2].name == "Спам")
        #expect(doc.hiddenCount == 1)

        // Через JSON — всё, что поправили.
        let back = FoldersDoc.parse(JSONValue.parse(doc.toJSON().data())).withSpam()
        let f = try #require(back.folders.first { $0.id == id })
        #expect(f.name == "Работа")
        #expect(f.rules.domains == ["acme.com", "x.ru"])
        #expect(f.rules.kind == .direct)
        #expect(f.hidden)
        #expect(back.folders[0].toJSON()["system"] == nil)
        #expect(back.spamIndex == 2)

        doc = doc.removing(id).removing("spam")
        #expect(doc.folders.map(\.id) == ["a", "spam"])
        #expect(doc.onlySystem().folders.map(\.id) == ["spam"])
        #expect(clipFolderIcon(" 👨‍👩‍👧‍👦🇷🇺abc ") == "👨‍👩‍👧‍👦🇷🇺ab")
    }
}

struct IdentityColorsTests {
    private let colors = IdentityColors.parse(JSONValue.parse(Data(#"""
    {"groups":[
      {"id":"g1","name":"Работа","color":"","addresses":[{"email":"me@work.example"}]},
      {"id":"g2","name":"Личное","color":"#B2DFDB","addresses":[{"email":"Me@Home.Example","hidden":true}]},
      {"id":"g3","name":"Битый","color":"зелёный","addresses":[{"email":"x@y.example"}]}
    ],"default_group":"g1"}
    """#.utf8)))

    @Test func emptyColorTakesPaletteByIndex() {
        #expect(colors.color(of: "me@work.example") == IdentityColors.pastel[0])
    }

    @Test func ownColorAndCaseInsensitive() {
        #expect(colors.color(of: " ME@home.example ") == 0xFFB2_DFDB)
    }

    @Test func badColorFallsBackToPalette() {
        #expect(colors.color(of: "x@y.example") == IdentityColors.pastel[2])
    }

    @Test func taggedAddressTakesBase() {
        #expect(colors.color(of: "me+list@work.example") == IdentityColors.pastel[0])
    }

    @Test func unknownIsGrey() {
        #expect(colors.color(of: "other@work.example") == IdentityColors.unknown)
        #expect(IdentityColors.parse(nil).color(of: "me@work.example") == IdentityColors.unknown)
    }
}
