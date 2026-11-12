import DDMailNet
import Foundation
import Testing
@testable import DDMailSMTP

final class ScriptedSMTPSocket: MailSocket, @unchecked Sendable {
    private let lock = NSLock()
    private var incoming: [Data]
    private var afterTLS: [Data]
    private(set) var written = Data()
    private(set) var tls = false

    init(_ server: String) {
        let h = server.components(separatedBy: "<<TLS>>")
        incoming = [Data(h[0].utf8)]
        afterTLS = h.count > 1 ? [Data(h[1].utf8)] : []
    }
    func send(_ data: Data) async throws { lock.withLock { written += data } }
    func receive() async throws -> Data { lock.withLock { incoming.isEmpty ? Data() : incoming.removeFirst() } }
    func startTLS() async throws { lock.withLock { tls = true; incoming += afterTLS; afterTLS = [] } }
    func close() {}
    var sent: String { lock.withLock { String(decoding: written, as: UTF8.self) } }
}

struct SMTPConn: MailConnector {
    let s: ScriptedSMTPSocket
    func connect(_ endpoint: MailEndpoint, timeout: Duration) async throws -> MailSocket { s }
}

struct SMTPTests {
    @Test func starttlsAuthPlainAndSendWithDotStuffing() async throws {
        let server = """
        220 mx ESMTP\r
        250-mx\r
        250-STARTTLS\r
        250 SIZE 100\r
        220 go\r
        <<TLS>>250-mx\r
        250-AUTH PLAIN LOGIN\r
        250 8BITMIME\r
        235 ok\r
        250 sender ok\r
        250 rcpt ok\r
        250 rcpt ok\r
        354 go\r
        250 queued\r
        221 bye\r

        """
        let s = ScriptedSMTPSocket(server)
        let c = try await SMTPClient.connect(MailEndpoint(host: "h", port: 587, security: .startTLS),
                                             credentials: SMTPCredentials(user: "me", password: "pw"), connector: SMTPConn(s: s))
        try await c.send(from: "me@example.org", recipients: ["a@example.net", "hidden@example.net"], data: Data("Subject: x\n\n.line\nend".utf8))
        await c.quit()
        #expect(s.tls)
        let sent = s.sent
        #expect(sent.contains("AUTH PLAIN \(Data("\0me\0pw".utf8).base64EncodedString())\r\n"))
        #expect(sent.contains("RCPT TO:<hidden@example.net>\r\n"))
        #expect(sent.contains("Subject: x\r\n\r\n..line\r\nend\r\n.\r\n"))
    }

    @Test func starttlsMissingIsRefused() async {
        let s = ScriptedSMTPSocket("220 mx\r\n250 mx\r\n")
        await #expect(throws: MailNetError.self) {
            _ = try await SMTPClient.connect(MailEndpoint(host: "h", port: 587, security: .startTLS), credentials: nil, connector: SMTPConn(s: s))
        }
    }

    @Test func wrongPasswordIsAuth() async {
        let s = ScriptedSMTPSocket("220 mx\r\n250-mx\r\n250 AUTH LOGIN\r\n334 VXNlcm5hbWU6\r\n334 UGFzc3dvcmQ6\r\n535 5.7.8 bad credentials\r\n")
        await #expect(throws: MailNetError.auth("5.7.8 bad credentials")) {
            _ = try await SMTPClient.connect(MailEndpoint(host: "h", port: 465, security: .tls),
                                             credentials: SMTPCredentials(user: "u", password: "p"), connector: SMTPConn(s: s))
        }
    }

    @Test func noAuthRelaySkipsAuth() async throws {
        let s = ScriptedSMTPSocket("220 mx\r\n250 mx\r\n221 bye\r\n")
        try await SMTPClient.check(MailEndpoint(host: "h", port: 465, security: .tls), credentials: nil, connector: SMTPConn(s: s))
        #expect(!s.sent.contains("AUTH"))
    }
}
