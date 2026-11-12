import DDMailAPI
import Foundation
import GRDB
import Testing
@testable import DDMailStore

/// Кэш нескольких учёток: миграция одноучётного кэша и удаление учётки.
struct AccountsCacheTests {
    /// Кэш прежней версии: служебные ключи ddmail-учётки получают её ключ в имени.
    @Test func migrationScopesLegacyMetaKeys() throws {
        let q = try DatabaseQueue()
        try MailStore.migrator.migrate(q, upTo: "v1")
        try q.write { db in
            try db.execute(sql: "INSERT INTO meta (key, value) VALUES ('account_key', 'lucky@mail.example.org'), ('journal_seq', '42'), ('dialog_folders', '{}')")
        }
        let s = try MailStore(writer: q)
        #expect(try s.meta("lucky@mail.example.org/journal_seq") == "42")
        #expect(try s.meta("lucky@mail.example.org/dialog_folders") == "{}")
        #expect(try s.meta("journal_seq") == nil)
        #expect(try s.meta("account_key") == nil)
    }

    @Test func clearAccountRemovesOnlyThatAccount() throws {
        let s = try MailStore.inMemory()
        let dto = ConversationDTO(id: "c", messages: [MessageRef(folder: "INBOX", uid: 1, messageID: "m")])
        try s.replaceAll("a@x", [dto])
        try s.replaceAll("a@x#spam", [dto])
        try s.replaceAll("b@y", [dto])
        try s.setMeta("a@x/journal_seq", "1")
        try s.setMeta("a_x/journal_seq", "2")
        try s.putImapHeaders([ImapHeaderRecord(accountKey: "a@x", role: "inbox", uid: 1)])
        try s.putImapFolder(ImapFolderRecord(accountKey: "a@x", role: "inbox", name: "INBOX"))
        try s.clearAccount("a@x")
        let snap = try s.snapshot(["a@x", "a@x#spam", "b@y"])
        #expect(snap["a@x"]?.conversations.isEmpty == true)
        #expect(snap["a@x#spam"]?.conversations.isEmpty == true)
        #expect(snap["b@y"]?.conversations.count == 1)
        #expect(try s.meta("a@x/journal_seq") == nil)
        // `_` в LIKE — не любой символ: чужой ключ, похожий по маске, цел.
        #expect(try s.meta("a_x/journal_seq") == "2")
        #expect(try s.imapUIDs("a@x", role: "inbox").isEmpty)
        #expect(try s.imapFolders("a@x").isEmpty)
    }

    @Test func imapHeadersFlagsAndFolderReset() throws {
        let s = try MailStore.inMemory()
        try s.putImapHeaders([
            ImapHeaderRecord(accountKey: "a", role: "inbox", uid: 5, to: [StoredAddress(name: "Я", email: "me@x")], seen: false),
            ImapHeaderRecord(accountKey: "a", role: "inbox", uid: 6),
        ])
        try s.updateImapFlags("a", role: "inbox", [(uid: 5, seen: true, draft: false, flagged: true)])
        let rows = try s.imapHeaders("a", roles: ["inbox"]).sorted { $0.uid < $1.uid }
        #expect(rows.map(\.uid) == [5, 6])
        #expect(rows[0].seen && rows[0].flagged)
        #expect(rows[0].to == [StoredAddress(name: "Я", email: "me@x")])
        try s.putBodies("a", [MessageBody(uid: 100, text: "x"), MessageBody(uid: 900, text: "y")])
        try s.clearImapFolder("a", role: "inbox", bodyUIDs: 0...500)
        #expect(try s.imapUIDs("a", role: "inbox").isEmpty)
        #expect(try s.cachedBodyUIDs("a") == [900])
    }
}
