import Foundation

/// Исходящее письмо для SMTP (порт `smtp::build_body` десктопа):
///
/// - `multipart/alternative { text/plain, text/html }`;
/// - встроенные картинки (с `Content-ID`) → `multipart/related { alternative, картинки }`;
/// - файлы → `multipart/mixed { related-или-alternative, файлы }`.
///
/// `Message-ID <hex.hex@домен-from>`, `In-Reply-To`/`References` в ответах.
/// Bcc в заголовки не пишется никогда.
public struct OutgoingMessage: Sendable {
    public struct Attachment: Sendable {
        public var filename: String
        public var mimeType: String
        public var data: Data
        /// Есть — встроенная картинка (`cid:`), нет — файл.
        public var contentID: String?

        public init(filename: String, mimeType: String, data: Data, contentID: String? = nil) {
            self.filename = filename
            self.mimeType = mimeType
            self.data = data
            self.contentID = contentID
        }
    }

    public var from: MailAddress
    public var to: [MailAddress]
    public var cc: [MailAddress]
    public var subject: String
    public var text: String
    public var html: String
    public var inReplyTo: String
    /// Через пробел, без угловых скобок или с ними.
    public var references: String
    public var attachments: [Attachment]
    public var date: Date

    public init(
        from: MailAddress, to: [MailAddress], cc: [MailAddress] = [], subject: String, text: String, html: String,
        inReplyTo: String = "", references: String = "", attachments: [Attachment] = [], date: Date = Date()
    ) {
        self.from = from; self.to = to; self.cc = cc; self.subject = subject; self.text = text; self.html = html
        self.inReplyTo = inReplyTo; self.references = references; self.attachments = attachments; self.date = date
    }
}

public enum MIMEBuilder {
    /// `<наносекунды-hex.счётчик-hex@домен>` — как `make_message_id` десктопа.
    public static func makeMessageID(fromEmail: String, now: Date = Date(), counter: UInt64 = UInt64.random(in: 0..<0xFFFF)) -> String {
        let domain = fromEmail.split(separator: "@").last.map(String.init) ?? "localhost"
        let nanos = UInt64(max(0, now.timeIntervalSince1970) * 1_000_000_000)
        return "<\(String(nanos, radix: 16)).\(String(counter, radix: 16))@\(domain)>"
    }

    /// Собрать письмо целиком (CRLF). Возвращает байты и Message-ID (без `<>`).
    public static func build(
        _ m: OutgoingMessage, messageID: String? = nil, boundarySeed: String = UUID().uuidString
    ) -> (data: Data, messageID: String) {
        let mid = messageID ?? makeMessageID(fromEmail: m.from.email, now: m.date)
        var h = ""
        func add(_ name: String, _ value: String) { h += "\(name): \(value)\r\n" }
        add("From", address(m.from))
        if !m.to.isEmpty { add("To", m.to.map(address).joined(separator: ",\r\n ")) }
        if !m.cc.isEmpty { add("Cc", m.cc.map(address).joined(separator: ",\r\n ")) }
        add("Subject", EncodedWord.encode(m.subject))
        add("Date", MailDate.format(m.date))
        add("Message-ID", mid.hasPrefix("<") ? mid : "<\(mid)>")
        if !m.inReplyTo.isEmpty { add("In-Reply-To", bracket(m.inReplyTo)) }
        let refs = m.references.split(whereSeparator: { $0 == " " || $0 == "\t" }).map { bracket(String($0)) }
        if !refs.isEmpty { add("References", refs.joined(separator: "\r\n ")) }
        add("MIME-Version", "1.0")

        let seed = boundarySeed.replacingOccurrences(of: "-", with: "")
        let alt = "alt-\(seed)", rel = "rel-\(seed)", mix = "mix-\(seed)"
        let inlines = m.attachments.filter { $0.contentID != nil }
        let files = m.attachments.filter { $0.contentID == nil }

        var altBody = ""
        altBody += "--\(alt)\r\n" + textPart("text/plain", m.text)
        altBody += "--\(alt)\r\n" + textPart("text/html", m.html)
        altBody += "--\(alt)--\r\n"
        let altPart = "Content-Type: multipart/alternative; boundary=\"\(alt)\"\r\n\r\n" + altBody

        var inner = altPart
        if !inlines.isEmpty {
            var b = "--\(rel)\r\n" + altPart
            for a in inlines { b += "--\(rel)\r\n" + binaryPart(a, inline: true) }
            b += "--\(rel)--\r\n"
            inner = "Content-Type: multipart/related; boundary=\"\(rel)\"\r\n\r\n" + b
        }
        var top = inner
        if !files.isEmpty {
            var b = "--\(mix)\r\n" + inner
            for a in files { b += "--\(mix)\r\n" + binaryPart(a, inline: false) }
            b += "--\(mix)--\r\n"
            top = "Content-Type: multipart/mixed; boundary=\"\(mix)\"\r\n\r\n" + b
        }
        return (Data((h + top).utf8), normalizeMessageID(mid))
    }

    static func bracket(_ id: String) -> String {
        let t = id.trimmingCharacters(in: .whitespaces)
        return t.hasPrefix("<") ? t : "<\(t)>"
    }

    /// `"Имя" <адрес>` с RFC 2047 для не-ASCII.
    public static func address(_ a: MailAddress) -> String {
        let name = a.name.trimmingCharacters(in: .whitespaces)
        if name.isEmpty || name.caseInsensitiveCompare(a.email) == .orderedSame { return a.email }
        let enc = EncodedWord.encode(name)
        if enc != name { return "\(enc) <\(a.email)>" }
        let quoted = name.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"")
        return "\"\(quoted)\" <\(a.email)>"
    }

    static func textPart(_ type: String, _ body: String) -> String {
        "Content-Type: \(type); charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n"
            + TransferEncoding.quotedPrintableEncode(body) + "\r\n"
    }

    static func binaryPart(_ a: OutgoingMessage.Attachment, inline: Bool) -> String {
        let name = a.filename.isEmpty ? "attachment" : a.filename
        let mime = a.mimeType.contains("/") ? a.mimeType : "application/octet-stream"
        var s = "Content-Type: \(mime); name=\(paramValue(name))\r\n"
        s += "Content-Transfer-Encoding: base64\r\n"
        if inline, let cid = a.contentID {
            s += "Content-ID: <\(normalizeMessageID(cid))>\r\n"
            s += "Content-Disposition: inline; \(filenameParam(name))\r\n"
        } else {
            s += "Content-Disposition: attachment; \(filenameParam(name))\r\n"
        }
        return s + "\r\n" + TransferEncoding.base64Lines(a.data)
    }

    /// `name=` — ASCII в кавычках или RFC 2047 (так его понимают все почтовики).
    static func paramValue(_ v: String) -> String {
        let enc = EncodedWord.encode(v)
        if enc != v { return "\"\(enc.replacingOccurrences(of: "\r\n ", with: " "))\"" }
        return "\"\(v.replacingOccurrences(of: "\"", with: "'"))\""
    }

    /// `filename` — ASCII в кавычках или RFC 2231 `filename*=utf-8''…`.
    static func filenameParam(_ v: String) -> String {
        if v.unicodeScalars.allSatisfy({ $0.isASCII && $0.value >= 32 }) {
            return "filename=\"\(v.replacingOccurrences(of: "\"", with: "'"))\""
        }
        var allowed = CharacterSet.alphanumerics.intersection(CharacterSet(charactersIn: Unicode.Scalar(0)...Unicode.Scalar(127)))
        allowed.insert(charactersIn: "!#$&+-.^_`|~")
        let pct = v.addingPercentEncoding(withAllowedCharacters: allowed) ?? v
        return "filename*=utf-8''\(pct)"
    }
}
