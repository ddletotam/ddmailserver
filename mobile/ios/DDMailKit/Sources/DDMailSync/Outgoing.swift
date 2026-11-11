import DDMailAPI
import Foundation

/// Файл, приложенный к письму в композере.
public struct PendingAttachment: Equatable, Identifiable, Sendable {
    public let id = UUID()
    public var name: String
    public var mime: String
    public var data: Data

    public init(name: String, mime: String, data: Data) {
        self.name = name
        self.mime = mime
        self.data = data
    }

    public var size: Int { data.count }
}

/// Что набрано в композере. «Кому», «Копия», «Тема» — переопределения:
/// пусто значит «как выведет ветка отправки» (десктопный контракт §1).
public struct Draft: Equatable, Sendable {
    public var text = ""
    public var to = ""
    public var cc = ""
    public var subject = ""
    /// Адрес «От кого»; пусто — выбрать по беседе.
    public var from = ""
    public var attachments: [PendingAttachment] = []

    public init(text: String = "", to: String = "", cc: String = "", subject: String = "", from: String = "", attachments: [PendingAttachment] = []) {
        self.text = text; self.to = to; self.cc = cc; self.subject = subject; self.from = from; self.attachments = attachments
    }

    /// «Есть что отправлять»: текст или хотя бы одно вложение (§1).
    public var canSend: Bool {
        !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || !attachments.isEmpty
    }

    public var isEmpty: Bool { text.isEmpty && attachments.isEmpty }
}

/// Ветка отправки (§1 десктопа), в порядке проверки.
public enum Branch: Sendable {
    /// Пересылка: адресат — только явное «Кому».
    case forward(MessageBody)
    /// Новое письмо (из списка).
    case new
    /// Явный ответ на конкретное письмо; в группе — всем, кроме своих.
    case reply(MessageBody, isGroup: Bool)
    /// Неявный ответ в открытой беседе — её собеседникам.
    case implicit(Conversation, lastIncoming: MessageBody?, last: MessageBody?)
}

/// Результат сборки: запрос или причина, почему отправлять нечего или некому.
public enum Built: Sendable {
    case ok(SendRequest, to: [String])
    case refused(String)
}

public func splitAddresses(_ s: String) -> [String] {
    s.split(whereSeparator: { ",; \n".contains($0) })
        .map { $0.trimmingCharacters(in: .whitespaces) }
        .filter { $0.contains("@") }
}

/// `Имя <адрес>` → адрес в нижнем регистре; голый адрес — он же.
public func bareAddress(_ raw: String) -> String {
    if let lt = raw.firstIndex(of: "<"), let gt = raw[raw.index(after: lt)...].firstIndex(of: ">") {
        return raw[raw.index(after: lt)..<gt].trimmingCharacters(in: .whitespaces).lowercased()
    }
    return raw.trimmingCharacters(in: .whitespaces).lowercased()
}

private func re(_ s: String) -> String {
    if s.trimmingCharacters(in: .whitespaces).isEmpty { return s }
    if s.trimmingCharacters(in: .whitespaces).lowercased().hasPrefix("re:") { return s }
    return "Re: \(s)"
}

func escapeHTML(_ s: String) -> String {
    s.replacingOccurrences(of: "&", with: "&amp;").replacingOccurrences(of: "<", with: "&lt;")
        .replacingOccurrences(of: ">", with: "&gt;").replacingOccurrences(of: "\"", with: "&quot;")
}

/// Набранный текст в HTML: переносы строк — `<br>`, ничего больше не выдумываем.
public func textToHTML(_ text: String) -> String {
    "<div>" + escapeHTML(text).replacingOccurrences(of: "\n", with: "<br>") + "</div>"
}

/// «От кого»: явный выбор; иначе адрес, на который пришла беседа (если он
/// свой); иначе адрес по умолчанию.
public func pickFrom(_ draft: Draft, identities: [Identity], receivedBy: String?) -> Identity? {
    func find(_ a: String) -> Identity? { identities.first { $0.email.caseInsensitiveCompare(a) == .orderedSame } }
    if !draft.from.trimmingCharacters(in: .whitespaces).isEmpty, let i = find(draft.from) { return i }
    if let r = receivedBy, let i = find(r) { return i }
    return identities.first(where: \.isDefault) ?? identities.first
}

public func fromHeader(_ id: Identity) -> String {
    if id.name.trimmingCharacters(in: .whitespaces).isEmpty || id.name.caseInsensitiveCompare(id.email) == .orderedSame {
        return id.email
    }
    return "\"\(id.name.replacingOccurrences(of: "\"", with: "'"))\" <\(id.email)>"
}

private func threading(_ b: MessageBody?) -> (inReplyTo: String, references: String) {
    guard let b else { return ("", "") }
    let refs = b.references + (b.messageID.isEmpty ? [] : [b.messageID])
    return (b.messageID, refs.joined(separator: " "))
}

/// Собрать письмо по ветке (§1, порт `build` Android). Непустое «Кому»
/// побеждает любую авто-деривацию, непустая тема — авто-«Re:»/«Fwd:»; копия
/// уходит всегда.
public func buildOutgoing(
    _ branch: Branch, draft: Draft, identities: [Identity],
    forwardHTML: String = "", forwardText: String = "", extraAttachments: [PendingAttachment] = []
) -> Built {
    if case .forward = branch {} else if !draft.canSend { return .refused("Нечего отправлять.") }
    let own = Set(identities.map { $0.email.lowercased() })
    let toOverride = splitAddresses(draft.to)
    let cc = splitAddresses(draft.cc)
    let subjectOverride = draft.subject.trimmingCharacters(in: .whitespaces).isEmpty ? nil : draft.subject

    var receivedBy: String?
    if case let .implicit(c, _, _) = branch { receivedBy = c.receivedBy }
    guard let from = pickFrom(draft, identities: identities, receivedBy: receivedBy) else {
        return .refused("Нет ни одного своего адреса для отправки.")
    }

    let to: [String], subject: String, irt: String, refs: String
    switch branch {
    case let .forward(o):
        if toOverride.isEmpty { return .refused("Укажите, кому переслать.") }
        (to, subject, irt, refs) = (toOverride, subjectOverride ?? "Fwd: \(o.subject)", "", "")
    case .new:
        if toOverride.isEmpty { return .refused("Укажите адресата.") }
        (to, subject, irt, refs) = (toOverride, draft.subject, "", "")
    case let .reply(o, isGroup):
        var derived = [o.fromAddr.lowercased()]
        if isGroup { derived += (o.to + o.cc).map(bareAddress) }
        var seen = Set<String>()
        derived = derived.filter { !$0.isEmpty && !own.contains($0) && seen.insert($0).inserted }
        let t = toOverride.isEmpty ? derived : toOverride
        if t.isEmpty { return .refused("Не нашлось, кому ответить.") }
        let th = threading(o)
        (to, subject, irt, refs) = (t, subjectOverride ?? re(o.subject), th.inReplyTo, th.references)
    case let .implicit(c, lastIncoming, last):
        // Беседа «все адреса мои» (письмо себе) собеседников не имеет —
        // отвечаем отправителю последнего входящего (§4 «Роли» десктопа).
        var t = toOverride
        if t.isEmpty { t = c.counterparts.map(\.addr).filter { !$0.trimmingCharacters(in: .whitespaces).isEmpty } }
        if t.isEmpty, let a = [lastIncoming?.fromAddr ?? c.receivedBy].first, !a.isEmpty { t = [a] }
        if t.isEmpty { return .refused("У беседы нет адресата.") }
        // Тема и цепочка — от последнего входящего: на него и отвечают,
        // даже если наше письмо было позже (§1 десктопа).
        let base = lastIncoming?.subject ?? c.lastSubject
        let th = threading(lastIncoming ?? last)
        (to, subject, irt, refs) = (t, subjectOverride ?? re(base), th.inReplyTo, th.references)
    }
    let atts = (draft.attachments + extraAttachments).map { OutgoingAttachment(filename: $0.name, mimeType: $0.mime, content: $0.data) }
    let request = SendRequest(
        from: fromHeader(from), to: to, cc: cc, subject: subject,
        html: textToHTML(draft.text) + forwardHTML, text: draft.text + forwardText,
        inReplyTo: irt, references: refs, attachments: atts
    )
    return .ok(request, to: to)
}

/// Блок пересылаемого письма: заголовки и само письмо.
public func forwardBlock(_ o: MessageBody, timeZone: TimeZone = .current) -> (text: String, html: String) {
    let f = DateFormatter()
    f.locale = Locale(identifier: "ru_RU")
    f.timeZone = timeZone
    f.dateFormat = "dd.MM.yyyy HH:mm"
    let date = f.string(from: Date(timeIntervalSince1970: TimeInterval(o.dateTs)))
    let fromName = o.from.trimmingCharacters(in: .whitespaces).isEmpty ? o.fromAddr : o.from
    let fromLine = "От: \(fromName)" + (!o.from.isEmpty && !o.fromAddr.isEmpty ? " <\(o.fromAddr)>" : "")
    let head = [
        "---------- Пересылаемое сообщение ----------",
        fromLine,
        "Дата: \(date)",
        "Тема: \(o.subject)",
        "Кому: \(o.to.joined(separator: ", "))",
    ]
    let text = "\n\n" + head.joined(separator: "\n") + "\n\n" + (o.text ?? "")
    let html = "<br><div>" + head.map(escapeHTML).joined(separator: "<br>") + "</div><br>" + (o.html ?? textToHTML(o.text ?? ""))
    return (text, html)
}

/// Беседа нового письма: набор собеседников совпал с адресатами. Письмо себе
/// сервер отдаёт беседой без собеседников — тогда сверяем по её id.
public func matchesTarget(_ c: Conversation, _ target: Set<String>) -> Bool {
    let cps = Set(c.counterparts.map { $0.addr.lowercased() }.filter { !$0.isEmpty })
    if !cps.isEmpty { return cps == target }
    let id = c.id.lowercased()
    return target.allSatisfy { id.contains($0) }
}

/// Лимит сервера — 30 МБ на запрос в base64; сырых байт — около 22.
public let maxOutgoingAttachmentBytes = 22 << 20
