import Testing
@testable import DDMailAPI

/// Те же векторы, что `ServerUrlTest.kt` Android (контракт §1: порты — с тестами
/// оригинала), плюс контрольные примеры RFC 3492 для punycode.
struct ServerURLTests {
    @Test func schemeIsAddedAndTailDropped() throws {
        #expect(try normalizeServerURL(" mail.example.ru/ ").get() == "https://mail.example.ru")
        #expect(try normalizeServerURL("HTTPS://Mail.Example.RU/path").get() == "https://mail.example.ru")
        #expect(try normalizeServerURL("mail.example.ru:8443").get() == "https://mail.example.ru:8443")
        #expect(try normalizeServerURL("https://mail.example.ru:443").get() == "https://mail.example.ru")
    }

    @Test func httpIsRefused() {
        #expect(normalizeServerURL("http://mail.example.ru") == .failure(.insecure))
    }

    @Test(arguments: ["", "   ", "localhost", "https://", "mail.example.ru:", "mail.example.ru:99999", "a b.ru", "user@mail.example.ru"])
    func junkIsRefused(_ bad: String) {
        #expect((try? normalizeServerURL(bad).get()) == nil)
    }

    @Test func idnBecomesPunycode() throws {
        #expect(try normalizeServerURL("почта.рф").get() == "https://xn--80a1acny.xn--p1ai")
        #expect(emailDomain("Я@Почта.РФ") == "xn--80a1acny.xn--p1ai")
    }

    @Test func emailDomainNeedsRealAddress() {
        #expect(emailDomain(" Bob@Example.ORG ") == "example.org")
        for bad in ["", "bob", "bob@", "@x.ru", "bob@localhost", "bob@.ru"] {
            #expect(emailDomain(bad) == nil, "\(bad)")
        }
    }

    /// RFC 3492 §7.1: (A) арабский, (B) китайский, (L) смешанный с ASCII.
    @Test(arguments: [
        ("\u{0644}\u{064A}\u{0647}\u{0645}\u{0627}\u{0628}\u{062A}\u{0643}\u{0644}\u{0645}\u{0648}\u{0634}\u{0639}\u{0631}\u{0628}\u{064A}\u{061F}", "egbpdaj6bu4bxfgehfvwxn"),
        ("\u{4ED6}\u{4EEC}\u{4E3A}\u{4EC0}\u{4E48}\u{4E0D}\u{8BF4}\u{4E2D}\u{6587}", "ihqwcrb4cv8a8dqg056pqjye"),
        ("3\u{5E74}B\u{7D44}\u{91D1}\u{516B}\u{5148}\u{751F}", "3B-ww4c5e180e575a65lsy2b"),
    ])
    func punycodeMatchesRFC(_ input: String, _ expected: String) {
        #expect(Punycode.encode(input) == expected)
    }
}
