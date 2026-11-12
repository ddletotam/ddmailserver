import Foundation
import Testing
@testable import DDMailMIME

struct EncodingTests {
    @Test func encodedWordsUTF8AndKOI8() {
        #expect(EncodedWord.decode("=?UTF-8?B?0J/RgNC40LLQtdGC?=") == "Привет")
        // «Тест» в koi8-r, Q-кодировка.
        #expect(EncodedWord.decode("=?koi8-r?Q?=F4=C5=D3=D4?=") == "Тест")
        // windows-1251, base64.
        #expect(EncodedWord.decode("=?windows-1251?B?0eLl8g==?=") == "Свет")
    }

    @Test func adjacentWordsDropWhitespaceAndJoinBytes() {
        // «Привет» разрезан посреди символа на два слова: байты склеиваются.
        let a = Data("Привет".utf8)
        let w1 = "=?utf-8?B?\(a.prefix(5).base64EncodedString())?="
        let w2 = "=?utf-8?B?\(a.dropFirst(5).base64EncodedString())?="
        #expect(EncodedWord.decode("\(w1)\r\n \(w2)") == "Привет")
        // Между словом и обычным текстом пробел сохраняется.
        #expect(EncodedWord.decode("Re: =?utf-8?Q?=D0=94=D0=B0?= ok") == "Re: Да ok")
        #expect(EncodedWord.decode("a =?bad") == "a =?bad")
    }

    @Test func qUnderscoreIsSpace() {
        #expect(EncodedWord.decode("=?iso-8859-1?Q?caf=E9_au_lait?=") == "café au lait")
    }

    @Test func encodeRoundTrip() {
        let s = "Длинная тема письма, которая точно не влезет в одно слово RFC 2047 целиком"
        let enc = EncodedWord.encode(s)
        #expect(enc.components(separatedBy: "\r\n").allSatisfy { $0.count <= 76 })
        #expect(EncodedWord.decode(enc) == s)
        #expect(EncodedWord.encode("plain") == "plain")
    }

    @Test func quotedPrintable() {
        let qp = "Caf=C3=A9 =\r\nau lait=3D\r\n"
        #expect(String(decoding: TransferEncoding.quotedPrintableDecode(Array(qp.utf8)), as: UTF8.self) == "Café au lait=\r\n")
        let text = String(repeating: "Ж", count: 60) + "\nстрока "
        let enc = TransferEncoding.quotedPrintableEncode(text)
        #expect(enc.components(separatedBy: "\r\n").allSatisfy { $0.count <= 76 })
        let back = String(decoding: TransferEncoding.quotedPrintableDecode(Array(enc.utf8)), as: UTF8.self)
        #expect(back == text.replacingOccurrences(of: "\n", with: "\r\n"))
    }

    @Test func base64Tolerant() {
        #expect(TransferEncoding.base64Decode(Array("0J/R\r\ngNC4\n".utf8)) == Array("При".utf8))
    }

    @Test func charsets() {
        let cp1251: [UInt8] = [0xCF, 0xF0, 0xE8, 0xE2, 0xE5, 0xF2]
        #expect(Charset.decode(cp1251, charset: "windows-1251") == "Привет")
        #expect(Charset.decode(cp1251, charset: "\"Windows-1251\"") == "Привет")
        let koi: [UInt8] = [0xF0, 0xD2, 0xC9, 0xD7, 0xC5, 0xD4]
        #expect(Charset.decode(koi, charset: "koi8-r") == "Привет")
        // Без кодировки и не UTF-8 — windows-1251.
        #expect(Charset.decode(cp1251, charset: nil) == "Привет")
        #expect(Charset.decode(Array("ok".utf8), charset: "x-unknown") == "ok")
        let iso5: [UInt8] = [0xBF, 0xE0, 0xD8, 0xD2, 0xD5, 0xE2]
        #expect(Charset.decode(iso5, charset: "iso-8859-5") == "Привет")
    }
}

struct HeaderTests {
    @Test func unfoldsAndFindsCaseInsensitive() {
        let h = HeaderBlock(Array("Subject: =?utf-8?B?0KLQtdGB0YI=?=\r\n =?utf-8?B?0LjRgNC+0LLQsNC90LjQtQ==?=\r\nfrom: A <a@x.ru>\r\n\r\nbody".utf8))
        #expect(h.text("SUBJECT") == "Тестирование")
        #expect(h["From"] == "A <a@x.ru>")
    }

    @Test func addresses() {
        let list = MailAddress.parseList(#""Иванов, Иван" <Ivan@Example.ORG>, bob@x.ru (Боб), =?utf-8?B?0JDQvdC90LA=?= <anna@y.ru>, Друзья: c@z.ru, d@z.ru;"#)
        #expect(list.map(\.email) == ["ivan@example.org", "bob@x.ru", "anna@y.ru", "c@z.ru", "d@z.ru"])
        #expect(list[0].name == "Иванов, Иван")
        #expect(list[1].name == "Боб")
        #expect(list[2].name == "Анна")
        #expect(MailAddress.parseList("").isEmpty)
        #expect(MailAddress.parseList("undisclosed-recipients:;").isEmpty)
    }

    @Test func messageIDs() {
        #expect(normalizeMessageID(" <a.b@c> ") == "a.b@c")
        #expect(extractMessageIDs("<a@x>\r\n <b@y> junk <c@z>") == ["a@x", "b@y", "c@z"])
        #expect(cleanDisplayName("John Doe <j@x>") == "John Doe")
        #expect(cleanDisplayName("j@x") == "")
        #expect(cleanDisplayName("John (j@x)") == "John")
    }

    @Test func rfc2231Params() {
        let v = ParameterizedValue("attachment; filename*0*=UTF-8''%D0%9E%D1%82%D1%87; filename*1*=%D1%91%D1%82.pdf")
        #expect(v.value == "attachment")
        #expect(v["filename"] == "Отчёт.pdf")
        let w = ParameterizedValue("text/plain; charset=\"KOI8-R\"; format=flowed")
        #expect(w["charset"] == "KOI8-R")
        let x = ParameterizedValue(#"application/pdf; name="=?utf-8?B?0KHRh9C10YIucGRm?=""#)
        #expect(x["name"] == "Счет.pdf")
    }

    @Test func dates() {
        #expect(MailDate.parse("Thu, 12 Nov 2026 10:00:00 +0300") == 1_794_466_800)
        #expect(MailDate.parse("12 Nov 2026 07:00:00 GMT") == 1_794_466_800)
        #expect(MailDate.parse("Thu, 12 Nov 26 10:00 +0300 (MSK)") == 1_794_466_800)
        #expect(MailDate.parse("12-Nov-2026 10:00:00 +0300") == 1_794_466_800)
        #expect(MailDate.parse("garbage") == nil)
    }
}

struct MIMEParserTests {
    static let mixed = """
    From: =?utf-8?B?0JDQvdC90LA=?= <anna@example.org>\r
    To: me@example.ru\r
    Subject: test\r
    Content-Type: multipart/mixed; boundary="MIX"\r
    \r
    preamble\r
    --MIX\r
    Content-Type: multipart/related; boundary="REL"\r
    \r
    --REL\r
    Content-Type: multipart/alternative; boundary=ALT\r
    \r
    --ALT\r
    Content-Type: text/plain; charset=koi8-r\r
    Content-Transfer-Encoding: quoted-printable\r
    \r
    =F0=D2=C9=D7=C5=D4\r
    --ALT\r
    Content-Type: text/html; charset=utf-8\r
    Content-Transfer-Encoding: base64\r
    \r
    PHA+0J/RgNC40LLQtdGCPGltZyBzcmM9ImNpZDpsb2dvQHgiPjwvcD4=\r
    --ALT--\r
    --REL\r
    Content-Type: image/png\r
    Content-ID: <logo@x>\r
    Content-Transfer-Encoding: base64\r
    \r
    iVBORw0KGgo=\r
    --REL--\r
    --MIX\r
    Content-Type: application/pdf; name="=?utf-8?B?0KHRh9C10YIucGRm?="\r
    Content-Disposition: attachment\r
    Content-Transfer-Encoding: base64\r
    \r
    JVBERi0=\r
    --MIX\r
    Content-Type: image/jpeg\r
    Content-Transfer-Encoding: base64\r
    \r
    /9j/\r
    --MIX--\r
    epilogue\r

    """

    @Test func structureBodiesAttachmentsAndCid() {
        let m = ParsedMessage(raw: Array(Self.mixed.utf8))
        #expect(m.text == "Привет")
        #expect(m.html == "<p>Привет<img src=\"cid:logo@x\"></p>")
        #expect(m.attachments.map(\.filename) == ["Счет.pdf", "attachment_1"])
        #expect(m.attachments.map(\.index) == [0, 1])
        #expect(m.attachments[0].size == 5)
        #expect(m.attachment(0)?.data == Data("%PDF-".utf8))
        #expect(m.part(cid: "<LOGO@x>")?.mimeType == "image/png")
        #expect(m.part(cid: "logo@x")?.data.prefix(4) == Data([0x89, 0x50, 0x4E, 0x47]))
        #expect(m.headers.text("From") == "Анна <anna@example.org>")
    }

    @Test func plainMessageWithoutMIME() {
        let raw = "Subject: x\r\n\r\nHello\r\n"
        let m = ParsedMessage(raw: Array(raw.utf8))
        #expect(m.text == "Hello\r\n")
        #expect(m.html == nil)
        #expect(m.attachments.isEmpty)
    }

    @Test func lfOnlyAndMissingClosingBoundary() {
        let raw = "Content-Type: multipart/alternative; boundary=b\n\n--b\nContent-Type: text/plain\n\nA\n--b\nContent-Type: text/html\n\n<b>B</b>\n"
        let m = ParsedMessage(raw: Array(raw.utf8))
        #expect(m.text == "A")
        #expect(m.html == "<b>B</b>\n")
    }

    @Test func forwardedMessageIsAttachment() {
        let raw = "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nсм.\r\n--b\r\nContent-Type: message/rfc822\r\n\r\nSubject: Отчёт\r\n\r\nтело\r\n--b--\r\n"
        let m = ParsedMessage(raw: Array(raw.utf8))
        #expect(m.attachments.map(\.filename) == ["Отчёт.eml"])
    }
}

struct MIMEBuilderTests {
    @Test func buildsLayersAndParsesBack() {
        let msg = OutgoingMessage(
            from: MailAddress(name: "Иван", email: "ivan@example.org"),
            to: [MailAddress(email: "bob@example.net")], cc: [MailAddress(name: "Ann", email: "ann@example.net")],
            subject: "Отчёт за ноябрь", text: "Привет!\n.точка в начале", html: "<p>Привет!</p><img src=\"cid:pic1\">",
            inReplyTo: "orig@example.net", references: "<a@x> b@y",
            attachments: [
                .init(filename: "картинка.png", mimeType: "image/png", data: Data([1, 2, 3]), contentID: "pic1"),
                .init(filename: "Счёт №5.pdf", mimeType: "application/pdf", data: Data("%PDF".utf8)),
            ],
            date: Date(timeIntervalSince1970: 1_794_466_800)
        )
        let (data, mid) = MIMEBuilder.build(msg, messageID: "<abc.1@example.org>", boundarySeed: "S")
        let raw = String(decoding: data, as: UTF8.self)
        #expect(mid == "abc.1@example.org")
        #expect(!raw.contains("Bcc"))
        #expect(raw.contains("In-Reply-To: <orig@example.net>"))
        #expect(raw.contains("References: <a@x>\r\n <b@y>"))
        #expect(raw.contains("multipart/mixed"))
        #expect(raw.contains("multipart/related"))
        let p = ParsedMessage(raw: Array(data))
        #expect(p.text == "Привет!\r\n.точка в начале")
        #expect(p.html == msg.html)
        #expect(p.attachments.map(\.filename) == ["Счёт №5.pdf"])
        #expect(p.part(cid: "pic1")?.data == Data([1, 2, 3]))
        #expect(p.headers.text("Subject") == "Отчёт за ноябрь")
        #expect(MailAddress.parseList(p.headers["From"] ?? "") == [MailAddress(name: "Иван", email: "ivan@example.org")])
        #expect(MailAddress.parseList(p.headers["Cc"] ?? "").first?.name == "Ann")
    }

    @Test func messageIDFormat() {
        let id = MIMEBuilder.makeMessageID(fromEmail: "me@example.org", now: Date(timeIntervalSince1970: 1), counter: 255)
        #expect(id == "<3b9aca00.ff@example.org>")
    }
}
