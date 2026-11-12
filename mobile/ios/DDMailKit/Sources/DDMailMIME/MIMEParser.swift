import Foundation

/// Часть письма (RFC 2045/2046): заголовки, тип, тело как в письме и вложенные части.
public struct MIMEPart: Sendable {
    public let headers: HeaderBlock
    public let contentType: ParameterizedValue
    public let disposition: ParameterizedValue?
    /// Тело без раскодирования Content-Transfer-Encoding.
    public let rawBody: ArraySlice<UInt8>
    public let children: [MIMEPart]

    public var mimeType: String { contentType.value.isEmpty ? "text/plain" : contentType.value }
    public var isMultipart: Bool { mimeType.hasPrefix("multipart/") }

    /// `Content-ID` без `<>`.
    public var contentID: String? {
        headers["Content-ID"].map(normalizeMessageID).flatMap { $0.isEmpty ? nil : $0 }
    }

    /// Имя файла: `filename` из Content-Disposition, иначе `name` из Content-Type.
    public var filename: String? {
        let n = disposition?["filename"] ?? contentType["name"]
        return n.flatMap { $0.trimmingCharacters(in: .whitespaces).isEmpty ? nil : $0 }
    }

    /// Раскодированные байты тела.
    public var decodedBody: [UInt8] {
        TransferEncoding.decode(rawBody, encoding: headers["Content-Transfer-Encoding"])
    }

    /// Текст текстовой части по её `charset`.
    public var text: String {
        Charset.decode(decodedBody, charset: contentType["charset"])
    }

    /// Разобрать письмо или часть целиком.
    public init(raw: some Collection<UInt8>) {
        self.init(bytes: Array(raw)[...], depth: 0)
    }

    init(bytes: ArraySlice<UInt8>, depth: Int) {
        let (h, b) = splitHeaderBody(Array(bytes))
        let headers = HeaderBlock(h)
        self.headers = headers
        contentType = ParameterizedValue(headers["Content-Type"] ?? "text/plain")
        disposition = headers["Content-Disposition"].map(ParameterizedValue.init)
        rawBody = b
        var kids: [MIMEPart] = []
        // Глубина ограничена: злонамеренная вложенность не уронит разбор.
        if contentType.value.hasPrefix("multipart/"), let boundary = contentType["boundary"], !boundary.isEmpty, depth < 32 {
            kids = Self.split(b, boundary: boundary).map { MIMEPart(bytes: $0, depth: depth + 1) }
        }
        children = kids
    }

    /// Тело multipart → тела частей между `--boundary` и `--boundary--`.
    static func split(_ body: ArraySlice<UInt8>, boundary: String) -> [ArraySlice<UInt8>] {
        let delim = Array("--\(boundary)".utf8)
        let a = Array(body)
        var parts: [ArraySlice<UInt8>] = []
        var lineStart = 0
        var partStart: Int?
        var i = 0
        func atLineStart(_ p: Int) -> Bool { p == 0 || a[p - 1] == 10 }
        while i <= a.count {
            if i == a.count || a[i] == 10 {
                let line = a[lineStart..<i]
                var trimmed = line
                while let last = trimmed.last, last == 13 || last == 32 || last == 9 { trimmed = trimmed.dropLast() }
                if trimmed.starts(with: delim), atLineStart(lineStart) {
                    let rest = trimmed.dropFirst(delim.count)
                    if rest.isEmpty || rest.elementsEqual(Array("--".utf8)) {
                        if let s = partStart {
                            // Перенос перед разделителем принадлежит разделителю.
                            var end = lineStart
                            if end > s, a[end - 1] == 10 { end -= 1 }
                            if end > s, a[end - 1] == 13 { end -= 1 }
                            parts.append(a[s..<max(s, end)])
                        }
                        if !rest.isEmpty { return parts }
                        partStart = i + 1 <= a.count ? i + 1 : a.count
                    }
                }
                lineStart = i + 1
            }
            i += 1
        }
        // Нет закрывающего разделителя — хвост тоже часть.
        if let s = partStart, s < a.count { parts.append(a[s...]) }
        return parts
    }

    /// Листья по порядку обхода в глубину.
    public var leaves: [MIMEPart] {
        isMultipart ? children.flatMap(\.leaves) : [self]
    }
}

/// Вложение письма: имя, тип, размер раскодированных байт и номер.
public struct MIMEAttachment: Equatable, Sendable {
    public var filename: String
    public var mimeType: String
    public var size: Int
    public var index: Int
}

/// Письмо, разобранное в модель тела ленты (та же, что отдаёт сервер ddmail):
/// HTML, текст, вложения с номерами и встроенные части по `cid:`.
public struct ParsedMessage: Sendable {
    public let root: MIMEPart
    public let html: String?
    public let text: String?
    public let attachments: [MIMEAttachment]
    /// Листья-вложения по номеру (для выдачи байт).
    private let attachmentParts: [MIMEPart]
    /// Встроенные части по `Content-ID` (нижний регистр).
    private let inline: [String: MIMEPart]

    public var headers: HeaderBlock { root.headers }

    public init(raw: some Collection<UInt8>) {
        root = MIMEPart(raw: raw)
        var html: String?
        var text: String?
        var atts: [MIMEAttachment] = []
        var attParts: [MIMEPart] = []
        var inline: [String: MIMEPart] = [:]
        Self.walk(root, inAlternative: false, html: &html, text: &text, atts: &atts, attParts: &attParts, inline: &inline)
        self.html = html
        self.text = text
        attachments = atts
        attachmentParts = attParts
        self.inline = inline
    }

    /// Порядок выбора тела: первая HTML-часть и первая текстовая вне
    /// вложений. Вложением считается: `attachment`; лист не-текстового
    /// типа без `Content-ID`; текст с именем файла вне `alternative`;
    /// вложенное письмо (`message/rfc822`). Картинки с `Content-ID` —
    /// встроенные части (`cid:`); у которых есть имя и `attachment` —
    /// и то и другое.
    private static func walk(
        _ p: MIMEPart, inAlternative: Bool,
        html: inout String?, text: inout String?,
        atts: inout [MIMEAttachment], attParts: inout [MIMEPart], inline: inout [String: MIMEPart]
    ) {
        if p.isMultipart {
            let alt = p.mimeType == "multipart/alternative"
            for c in p.children {
                walk(c, inAlternative: alt, html: &html, text: &text, atts: &atts, attParts: &attParts, inline: &inline)
            }
            return
        }
        let type = p.mimeType
        let disp = p.disposition?.value ?? ""
        let cid = p.contentID
        if let cid { inline[cid.lowercased()] = p }
        let isText = type == "text/plain" || type == "text/html"
        var isAttachment: Bool
        if disp == "attachment" {
            isAttachment = true
        } else if isText {
            isAttachment = p.filename != nil && !inAlternative && disp != "inline"
        } else if type == "message/rfc822" {
            isAttachment = true
        } else {
            isAttachment = cid == nil || p.filename != nil && disp != "inline"
        }
        if isText && !isAttachment {
            if type == "text/html", html == nil { html = p.text }
            if type == "text/plain", text == nil { text = p.text }
            return
        }
        // Пустой «лист» без имени (например, text/plain с пустым телом в
        // конце mixed) — не вложение.
        if isAttachment && !(isText && p.filename == nil) {
            let body = p.decodedBody
            var name = p.filename ?? ""
            if name.isEmpty {
                if type == "message/rfc822" {
                    let inner = MIMEPart(raw: body)
                    let subj = inner.headers.text("Subject") ?? ""
                    name = (subj.isEmpty ? "письмо" : subj) + ".eml"
                } else {
                    name = "attachment_\(atts.count)"
                }
            }
            atts.append(MIMEAttachment(filename: name, mimeType: type, size: body.count, index: atts.count))
            attParts.append(p)
        }
    }

    /// Байты вложения по номеру и его тип.
    public func attachment(_ index: Int) -> (data: Data, mimeType: String)? {
        guard attachmentParts.indices.contains(index) else { return nil }
        let p = attachmentParts[index]
        return (Data(p.decodedBody), p.mimeType)
    }

    /// Встроенная часть по `cid:` (без учёта регистра).
    public func part(cid: String) -> (data: Data, mimeType: String)? {
        guard let p = inline[normalizeMessageID(cid).lowercased()] else { return nil }
        return (Data(p.decodedBody), p.mimeType)
    }
}
