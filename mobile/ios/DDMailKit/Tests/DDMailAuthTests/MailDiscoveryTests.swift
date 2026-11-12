import DDMailAPI
import Foundation
import Testing
@testable import DDMailAuth

/// Порт тестов `discover.rs` десктопа.
struct MailDiscoveryTests {
    static let xml = """
    <?xml version="1.0"?>
    <clientConfig version="1.1">
      <emailProvider id="example.org">
        <domain>example.org</domain>
        <displayName>Example Mail</displayName>
        <incomingServer type="pop3">
          <hostname>pop.example.org</hostname><port>995</port><socketType>SSL</socketType>
        </incomingServer>
        <incomingServer type="imap">
          <hostname>plain.example.org</hostname><port>143</port><socketType>plain</socketType>
        </incomingServer>
        <incomingServer type="imap">
          <hostname>imap.%EMAILDOMAIN%</hostname><port>993</port><socketType>SSL</socketType>
          <username>%EMAILLOCALPART%</username>
        </incomingServer>
        <outgoingServer type="smtp">
          <hostname>smtp.example.org</hostname><port>587</port><socketType>STARTTLS</socketType>
          <username>%EMAILADDRESS%</username>
        </outgoingServer>
      </emailProvider>
    </clientConfig>
    """

    @Test func autoconfigTakesEncryptedImapAndExpandsPlaceholders() throws {
        let a = try #require(MailDiscovery.parseAutoconfig(Self.xml, email: "bob@example.org"))
        #expect(a.imap == Endpoint("imap.example.org", 993))
        #expect(a.smtp == Endpoint("smtp.example.org", 587))
        #expect(a.login == "bob")
        #expect(a.name == "Example Mail")
    }

    @Test func autoconfigRejectsSecurityThatPortWouldNotUse() {
        let xml = Self.xml.replacingOccurrences(of: "<port>587</port><socketType>STARTTLS", with: "<port>587</port><socketType>SSL")
        #expect(MailDiscovery.parseAutoconfig(xml, email: "bob@example.org")?.smtp == nil)
        #expect(MailDiscovery.parseAutoconfig("<not xml", email: "a@b.c") == nil)
    }

    @Test func imapCandidatesPutKnownFirstAndCap() {
        let auto = MailDiscovery.Autoconfig(imap: Endpoint("in.example.org", 993))
        let v = MailDiscovery.imapCandidates(domain: "example.org", hint: nil, auto: auto, mx: ["mx.yandex.net", "mx2.example.org"])
        #expect(v[0] == Endpoint("in.example.org", 993))
        #expect(v[1] == Endpoint("imap.yandex.ru", 993))
        #expect(v[2] == Endpoint("imap.yandex.ru", 143))
        #expect(v[3] == Endpoint("imap.example.org", 993))
        #expect(v.contains(Endpoint("mx2.example.org", 143)))
        #expect(v.count <= MailDiscovery.maxIMAP)
    }

    @Test func smtpPort25ComesLastAndNeverOnMX() {
        let v = MailDiscovery.smtpCandidates(domain: "example.org", hint: nil, auto: nil, mx: ["mail.example.org"], imap: nil)
        let first25 = v.firstIndex { $0.port == 25 }!
        #expect(v[..<first25].allSatisfy { $0.port != 25 })
        #expect(v[first25...].allSatisfy { $0.port == 25 })
        #expect(!v.contains(Endpoint("mail.example.org", 25)))
        #expect(v.contains(Endpoint("mail.example.org", 465)))
        #expect(v.count <= MailDiscovery.maxSMTP)
    }

    @Test func worstCaseCandidatesFitTheBudget() {
        let hint = MailPreset(imapHost: "i.x.ru", smtpHost: "s.x.ru", smtpPort: 465, name: "X", passwordHint: "")
        let auto = MailDiscovery.Autoconfig(imap: Endpoint("a.x.ru", 993), smtp: Endpoint("b.x.ru", 587))
        let mx = ["mx.yandex.net", "aspmx.l.google.com"]
        let imap = MailDiscovery.imapCandidates(domain: "x.ru", hint: hint, auto: auto, mx: mx)
        let smtp = MailDiscovery.smtpCandidates(domain: "x.ru", hint: hint, auto: auto, mx: mx, imap: Endpoint("q.x.ru", 993))
        #expect(imap.count <= MailDiscovery.maxIMAP && smtp.count <= MailDiscovery.maxSMTP)
        #expect(imap[0].host == "a.x.ru")
        #expect(smtp[0].host == "b.x.ru")
        #expect(smtp[1].host == "s.x.ru")
    }

    @Test func presets() {
        let s = MailPreset.known("someone@bk.ru")
        #expect(s?.imapHost == "imap.mail.ru" && s?.smtpHost == "smtp.mail.ru" && s?.smtpPort == 465)
        #expect(s?.passwordHint.isEmpty == false)
        #expect(MailPreset.known("  Someone@GMX.NET ")?.smtpHost == "mail.gmx.net")
        #expect(MailPreset.known("a@icloud.com")?.smtpPort == 587)
        #expect(MailPreset.guess("a@example.org")?.imapHost == "imap.example.org")
        for e in ["", "user", "user@", "user@mail", "user@mail.r", "user@.ru"] {
            #expect(MailPreset.guess(e) == nil, "\(e)")
        }
    }

    /// Весь поиск на подменённых проверках: ddmail нет, IMAP — второй по
    /// порядку (первый молчит), SMTP — 465 без AUTH; прогресс доходит до конца.
    @Test func fullRunWinsByTrustOrder() async {
        let d = MailDiscovery(
            ddmail: { _ in false },
            mx: { _ in ["mx.example.org"] },
            fetch: { _ in nil },
            probeIMAP: { e in
                if e == Endpoint("imap.example.org", 993) {
                    try? await Task.sleep(for: .milliseconds(50))
                    return false
                }
                return e == Endpoint("imap.example.org", 143) || e == Endpoint("mail.example.org", 993)
            },
            probeSMTP: { e in e.host == "smtp.example.org" && e.port == 465 ? false : nil }
        )
        let last = Last()
        let r = await d.find(email: "bob@example.org") { last.set($0) }
        #expect(r.ddmailURL == nil)
        #expect(r.imap == Endpoint("imap.example.org", 143))
        #expect(r.smtp == Endpoint("smtp.example.org", 465))
        #expect(r.smtpAuth == false)
        #expect(last.value?.done == MailDiscovery.total)
    }

    @Test func ddmailWinsEarly() async {
        let d = MailDiscovery(ddmail: { $0 == "example.org" }, mx: { _ in [] }, fetch: { _ in nil },
                              probeIMAP: { _ in true }, probeSMTP: { _ in true })
        let r = await d.find(email: "bob@example.org") { _ in }
        #expect(r.ddmailURL == "https://example.org")
        #expect(r.imap == nil)
    }

    final class Last: @unchecked Sendable {
        private let lock = NSLock()
        private var v: MailDiscoveryProgress?
        func set(_ p: MailDiscoveryProgress) { lock.withLock { v = p } }
        var value: MailDiscoveryProgress? { lock.withLock { v } }
    }
}

/// Живой поиск — адрес из окружения (`DDMAIL_LIVE_DISCOVER`).
struct LiveDiscoveryTests {
    @Test(.enabled(if: ProcessInfo.processInfo.environment["DDMAIL_LIVE_DISCOVER"] != nil))
    func live() async {
        let email = ProcessInfo.processInfo.environment["DDMAIL_LIVE_DISCOVER"]!
        let t = Date()
        var d = MailDiscovery.live(transport: URLSession.shared)
        let (ddm, mx, fetch, pi, ps) = (d.ddmail, d.mx, d.fetch, d.probeIMAP, d.probeSMTP)
        func trace<A: Sendable, R: Sendable>(_ name: String, _ f: @escaping @Sendable (A) async -> R) -> @Sendable (A) async -> R {
            { a in
                let t0 = Date()
                print("→ \(name) \(a)")
                let r = await f(a)
                print("← \(name) \(a) = \(r) \(String(format: "%.1f", Date().timeIntervalSince(t0))) с")
                return r
            }
        }
        d.ddmail = trace("ddmail", ddm); d.mx = trace("mx", mx); d.fetch = trace("fetch", fetch)
        d.probeIMAP = trace("imap", pi); d.probeSMTP = trace("smtp", ps)
        let r = await d.find(email: email) { _ in }
        print("discover \(email): ddmail=\(r.ddmailURL ?? "-") imap=\(String(describing: r.imap)) smtp=\(String(describing: r.smtp)) auth=\(r.smtpAuth) provider=\(r.provider ?? "-") за \(Date().timeIntervalSince(t)) с")
    }
}
