import DDMailMIME
import DDMailNet
import Foundation
import Testing
@testable import DDMailIMAP

/// Записанная сессия: всё, что скажет сервер, — заранее; что сказал клиент — копится.
final class ScriptedSocket: MailSocket, @unchecked Sendable {
    private let lock = NSLock()
    private var incoming: [Data]
    /// Что сервер скажет уже под TLS (после `<<TLS>>` в сценарии): до
    /// рукопожатия эти байты клиенту недоступны — как в жизни.
    private var afterTLS: [Data]
    private(set) var written = Data()
    private(set) var tlsStarted = false

    init(_ server: String, chunk: Int = 37) {
        let halves = server.components(separatedBy: "<<TLS>>")
        // Порции нарочно мелкие и неровные: разбор не должен рассчитывать на границы строк.
        func chunks(_ s: String) -> [Data] {
            let d = Data(s.utf8)
            return stride(from: 0, to: d.count, by: chunk).map { d.subdata(in: $0..<min($0 + chunk, d.count)) }
        }
        incoming = chunks(halves[0])
        afterTLS = halves.count > 1 ? chunks(halves[1]) : []
    }

    func send(_ data: Data) async throws { lock.withLock { written += data } }

    func receive() async throws -> Data {
        lock.withLock { incoming.isEmpty ? Data() : incoming.removeFirst() }
    }

    func startTLS() async throws {
        lock.withLock {
            tlsStarted = true
            incoming += afterTLS
            afterTLS = []
        }
    }
    func close() {}

    var sent: String { lock.withLock { String(decoding: written, as: UTF8.self) } }
}

struct ScriptedConnector: MailConnector {
    let socket: ScriptedSocket
    func connect(_ endpoint: MailEndpoint, timeout: Duration) async throws -> MailSocket { socket }
}

struct IMAPParseTests {
    @Test func fetchWithEnvelopeLiteralAndSections() throws {
        let header = "From: =?utf-8?B?0JDQvdC90LA=?= <Anna@Example.org>\r\nSubject: hi\r\n\r\n"
        let raw = "* 12 FETCH (UID 345 FLAGS (\\Seen $Forwarded) MODSEQ (77) INTERNALDATE \"12-Nov-2026 10:00:00 +0300\" "
            + "ENVELOPE (\"Thu, 12 Nov 2026 10:00:00 +0300\" \"=?utf-8?B?0J/RgNC40LLQtdGC?=\" ((\"Anna\" NIL \"anna\" \"example.org\")) NIL NIL "
            + "((NIL NIL \"me\" \"example.ru\")(\"Group\" NIL \"team\" NIL)(NIL NIL \"bob\" \"example.net\")(NIL NIL NIL NIL)) NIL NIL \"<p@x>\" \"<m1@example.org>\") "
            + "BODY[HEADER.FIELDS (FROM SUBJECT)] {\(header.utf8.count)}\r\n\(header))"
        let r = try IMAPResponse.parse(Array(raw.utf8))
        guard case let .untagged(.fetch(seq, f)) = r else { Issue.record("не FETCH"); return }
        #expect(seq == 12)
        #expect(f.uid == 345)
        #expect(f.has(flag: "\\seen"))
        #expect(f.modseq == 77)
        #expect(f.internalDate == "12-Nov-2026 10:00:00 +0300")
        #expect(f.envelope?.messageID == "<m1@example.org>")
        #expect(f.envelope?.inReplyTo == "<p@x>")
        #expect(f.envelope?.to.map(\.email) == ["me@example.ru", "bob@example.net"])
        #expect(f.envelope?.from.first?.name == "Anna")
        #expect(f.header.map { String(decoding: $0, as: UTF8.self) } == header)
    }

    @Test func statusCodesSearchVanishedList() throws {
        if case let .untagged(.status(st, code, text)) = try IMAPResponse.parse(Array("* OK [UIDVALIDITY 3857529045] UIDs valid".utf8)) {
            #expect(st == .ok)
            #expect(code?.name == "UIDVALIDITY")
            #expect(code?.number == 3_857_529_045)
            #expect(text == "UIDs valid")
        } else { Issue.record("не статус") }
        #expect(try IMAPResponse.parse(Array("* SEARCH 2 84 882 (MODSEQ 9)".utf8)) == .untagged(.search([2, 84, 882])))
        #expect(try IMAPResponse.parse(Array("* SEARCH".utf8)) == .untagged(.search([])))
        if case let .untagged(.vanished(earlier, set)) = try IMAPResponse.parse(Array("* VANISHED (EARLIER) 41,43:45".utf8)) {
            #expect(earlier)
            #expect(set.all == [41, 43, 44, 45])
        } else { Issue.record("не VANISHED") }
        if case let .untagged(.list(l)) = try IMAPResponse.parse(Array(#"* LIST (\HasNoChildren \Sent) "/" "&BB4EQgQ,BEAEMAQyBDsENQQ9BD0ESwQ1-""#.utf8)) {
            #expect(l.has("\\sent"))
            #expect(l.name == "Отправленные")
            #expect(l.delimiter == "/")
        } else { Issue.record("не LIST") }
        if case let .tagged(tag, st, code, _) = try IMAPResponse.parse(Array("D3 NO [AUTHENTICATIONFAILED] Invalid credentials".utf8)) {
            #expect(tag == "D3")
            #expect(st == .no)
            #expect(code?.name == "AUTHENTICATIONFAILED")
        } else { Issue.record("не ответ") }
    }

    @Test func modifiedUTF7() {
        #expect(ModifiedUTF7.decode("&BB4EQgQ,BEAEMAQyBDsENQQ9BD0ESwQ1-") == "Отправленные")
        #expect(ModifiedUTF7.encode("Отправленные") == "&BB4EQgQ,BEAEMAQyBDsENQQ9BD0ESwQ1-")
        #expect(ModifiedUTF7.decode("A&-B") == "A&B")
        #expect(ModifiedUTF7.encode("A&B") == "A&-B")
        #expect(ModifiedUTF7.decode(ModifiedUTF7.encode("Спам/Junk 😀")) == "Спам/Junk 😀")
        #expect(ModifiedUTF7.decode("INBOX") == "INBOX")
    }

    @Test func uidSets() {
        #expect(UIDSet([5, 1, 2, 3, 9, 10]).string == "1:3,5,9:10")
        #expect(UIDSet(parse: "1:3,7")?.count == 4)
        #expect(UIDSet(parse: "x") == nil)
    }
}

struct IMAPClientTests {
    @Test func loginQuotesOrUsesLiteralAndEnablesQresync() async throws {
        let server = """
        * OK [CAPABILITY IMAP4rev1 LITERAL+ STARTTLS] ready\r
        D1 OK begin TLS\r
        <<TLS>>* CAPABILITY IMAP4rev1 LITERAL+ AUTH=PLAIN\r
        D2 OK caps\r
        D3 OK [CAPABILITY IMAP4rev1 LITERAL+ QRESYNC CONDSTORE UIDPLUS MOVE IDLE] logged in\r
        * ENABLED QRESYNC\r
        D4 OK enabled\r

        """
        let sock = ScriptedSocket(server)
        let c = try await IMAPClient.connect(MailEndpoint(host: "h", port: 143, security: .startTLS), connector: ScriptedConnector(socket: sock))
        try await c.login(user: "me@example.org", password: "пароль\"1")
        #expect(sock.tlsStarted)
        #expect(await c.qresyncEnabled)
        #expect(await c.has("MOVE"))
        let sent = sock.sent
        #expect(sent.contains("D1 STARTTLS\r\n"))
        // Не-ASCII пароль — литералом (LITERAL+ без ожидания «+»).
        #expect(sent.contains("D3 LOGIN \"me@example.org\" {14+}\r\nпароль\"1\r\n"))
        #expect(sent.contains("D4 ENABLE QRESYNC\r\n"))
    }

    @Test func wrongPasswordIsAuthError() async throws {
        let server = "* OK hi\r\nD1 OK [CAPABILITY IMAP4rev1] x\r\n* CAPABILITY IMAP4rev1\r\nD1 OK\r\nD2 NO [AUTHENTICATIONFAILED] Invalid credentials\r\n"
        let sock = ScriptedSocket("* OK [CAPABILITY IMAP4rev1] hi\r\nD1 NO [AUTHENTICATIONFAILED] Invalid credentials\r\n")
        _ = server
        let c = try await IMAPClient.connect(MailEndpoint(host: "h", port: 993, security: .tls), connector: ScriptedConnector(socket: sock))
        await #expect(throws: MailNetError.auth("Invalid credentials")) {
            try await c.login(user: "u", password: "p")
        }
    }

    @Test func selectWithQresyncReportsVanishedAndChanges() async throws {
        let server = """
        * OK [CAPABILITY IMAP4rev1 QRESYNC CONDSTORE] hi\r
        * ENABLED QRESYNC\r
        D1 OK\r
        * 172 EXISTS\r
        * OK [UIDVALIDITY 67890007] ok\r
        * OK [UIDNEXT 600] ok\r
        * OK [HIGHESTMODSEQ 90060128194045007] ok\r
        * VANISHED (EARLIER) 41,43:45\r
        * 49 FETCH (UID 117 FLAGS (\\Seen) MODSEQ (90060115205545359))\r
        D2 OK [READ-WRITE] selected\r

        """
        let sock = ScriptedSocket(server)
        let c = try await IMAPClient.connect(MailEndpoint(host: "h", port: 993, security: .tls), connector: ScriptedConnector(socket: sock))
        _ = try await c.run("ENABLE QRESYNC")
        // Флаг QRESYNC выставляет login; здесь — вручную через ENABLE выше не выставляется,
        // поэтому проверяем запасной путь CONDSTORE и разбор ответа.
        let s = try await c.select("INBOX", qresync: (67_890_007, 90_060_128_194_044_000))
        #expect(s.exists == 172)
        #expect(s.uidValidity == 67_890_007)
        #expect(s.uidNext == 600)
        #expect(s.highestModSeq == 90_060_128_194_045_007)
        #expect(s.vanished.first?.all == [41, 43, 44, 45])
        #expect(s.changed.first?.uid == 117)
        #expect(sock.sent.contains("D2 SELECT \"INBOX\" (CONDSTORE)\r\n"))
    }

    @Test func appendWaitsForContinuationWithoutLiteralPlus() async throws {
        let server = "* OK [CAPABILITY IMAP4rev1] hi\r\n+ go ahead\r\nD1 OK [APPENDUID 1 9] done\r\n"
        let sock = ScriptedSocket(server)
        let c = try await IMAPClient.connect(MailEndpoint(host: "h", port: 993, security: .tls), connector: ScriptedConnector(socket: sock))
        try await c.append("Sent", flags: ["\\Seen"], date: Date(timeIntervalSince1970: 0), message: Data("Subject: x\r\n\r\nhi\r\n".utf8))
        let sent = sock.sent
        #expect(sent.hasPrefix("D1 APPEND \"Sent\" (\\Seen) \""))
        #expect(sent.contains("{18}\r\nSubject: x\r\n\r\nhi\r\n\r\n"))
    }

    @Test func idleReturnsOnNewMail() async throws {
        let server = "* OK [CAPABILITY IMAP4rev1 IDLE] hi\r\n+ idling\r\n* 5 EXISTS\r\nD1 OK IDLE done\r\n"
        let sock = ScriptedSocket(server)
        let c = try await IMAPClient.connect(MailEndpoint(host: "h", port: 993, security: .tls), connector: ScriptedConnector(socket: sock))
        let got = try await c.idle(timeout: .seconds(60))
        #expect(got)
        #expect(sock.sent == "D1 IDLE\r\nDONE\r\n")
    }

    @Test func moveFallsBackToCopyAndExpunge() async throws {
        let server = "* OK [CAPABILITY IMAP4rev1 UIDPLUS] hi\r\nD1 OK copied\r\nD2 OK stored\r\nD3 OK expunged\r\n"
        let sock = ScriptedSocket(server)
        let c = try await IMAPClient.connect(MailEndpoint(host: "h", port: 993, security: .tls), connector: ScriptedConnector(socket: sock))
        try await c.move(UIDSet([3, 4, 5]), to: "Junk")
        #expect(sock.sent == "D1 UID COPY 3:5 \"Junk\"\r\nD2 UID STORE 3:5 +FLAGS.SILENT (\\Deleted)\r\nD3 UID EXPUNGE 3:5\r\n")
    }
}
