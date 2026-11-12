import DDMailAPI
import Foundation
import Testing
@testable import DDMailStore

struct MailStoreTests {
    private func dto(_ id: String, _ msgs: [(Int64, String)]) throws -> ConversationDTO {
        let refs = msgs.map { #"{"uid":\#($0.0),"message_id":"\#($0.1)"}"# }.joined(separator: ",")
        return try JSONDecoder().decode(ConversationDTO.self, from: Data(#"{"id":"\#(id)","messages":[\#(refs)]}"#.utf8))
    }

    /// Надгробия журнала: письма уходят из бесед, беседы без писем — целиком
    /// (`apply_deletions` десктопа); тела удалённых писем — тоже.
    @Test func deletionsDropEmptyConversations() throws {
        let s = try MailStore.inMemory()
        try s.replaceAll("a", [try dto("one", [(1, "<1>"), (2, "<2>")]), try dto("two", [(3, "<3>")])])
        try s.putBodies("a", [MessageBody(uid: 3, text: "x", messageID: "<3>")])
        let n = try s.applyDeletions("a", messageIDs: ["<2>", "<3>", ""])
        #expect(n == 2)
        let snap = try #require(try s.snapshot(["a"])["a"])
        #expect(snap.conversations.map(\.id) == ["one"])
        #expect(snap.refs["one"]?.map(\.uid) == [1])
        #expect(try s.bodies("a", uids: [3]).isEmpty)
    }

    /// Пространства учёток не смешиваются: спам живёт в своём.
    @Test func accountSpacesAreSeparate() throws {
        let s = try MailStore.inMemory()
        try s.replaceAll("a", [try dto("x", [(1, "<1>")])])
        try s.replaceAll("a#spam", [try dto("x", [(9, "<9>")])])
        try s.applyDeletions("a", messageIDs: ["<1>"])
        let snap = try s.snapshot(["a", "a#spam"])
        #expect(snap["a"]?.conversations.isEmpty == true)
        #expect(snap["a#spam"]?.conversations.map(\.id) == ["x"])
    }

    /// Пустое тело в кэше непригодно: его не отдают, и докачка его не считает.
    @Test func blankBodiesAreNotUsable() throws {
        let s = try MailStore.inMemory()
        try s.putBodies("a", [MessageBody(uid: 1, messageID: "<1>"), MessageBody(uid: 2, html: "<p>hi</p>", messageID: "<2>")])
        #expect(try s.bodies("a", uids: [1, 2]).keys.sorted() == [2])
        #expect(try s.cachedBodyUIDs("a") == [2])
    }

    /// Перенос кэша в App Group (контракт §7): данные — все, включая ещё не
    /// перенесённые из журнала WAL; старые файлы удалены; повтор безвреден.
    @Test func moveDatabaseKeepsData() throws {
        let fm = FileManager.default
        let root = fm.temporaryDirectory.appending(path: "ddmail-move-\(UUID().uuidString)")
        let old = root.appending(path: "app/ddmail/cache.sqlite")
        let new = root.appending(path: "group/ddmail/cache.sqlite")
        do {
            let s = try MailStore(url: old)
            try s.replaceAll("a", [try dto("x", [(1, "<1>"), (2, "<2>")])])
            try s.setMeta("local/dialog_folders", #"{"folders":[]}"#)
        }
        #expect(try MailStore.moveDatabase(from: old, to: new))
        for suffix in ["", "-wal", "-shm"] { #expect(!fm.fileExists(atPath: old.path + suffix)) }
        #expect(!fm.fileExists(atPath: new.path + ".migrating"))
        let moved = try MailStore(url: new)
        #expect(try moved.snapshot(["a"])["a"]?.refs["x"]?.count == 2)
        #expect(try moved.meta("local/dialog_folders") == #"{"folders":[]}"#)
        #expect(try MailStore.moveDatabase(from: old, to: new) == false)
    }
}
