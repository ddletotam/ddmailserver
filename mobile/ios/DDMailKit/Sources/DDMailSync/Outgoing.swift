import DDMailAPI
import Foundation

/// Файл, приложенный к письму в композере.
/// Картинка в теле письма (`contentID`) уходит inline-частью, HTML ссылается
/// на неё через `cid:` (десктоп §3а).
public struct PendingAttachment: Equatable, Identifiable, Sendable {
    public let id = UUID()
    public var name: String
    public var mime: String
    public var data: Data
    /// Есть — картинка в теле (`cid:`), нет — файл.
    public var contentID: String?

    public init(name: String, mime: String, data: Data, contentID: String? = nil) {
        self.name = name
        self.mime = mime
        self.data = data
        self.contentID = contentID
    }

    public var size: Int { data.count }
}

/// Что набрано в композере. «Кому», «Копия», «Тема» — переопределения:
/// пусто значит «как выведет ветка отправки» (десктопный контракт §1).
public struct Draft: Equatable, Sendable {
    /// Тело текстом (`text/plain`); с форматированием — ещё и `html`.
    public var text = ""
    /// Тело HTML (`text/html`, десктоп §3а: `<div>`-абзацы, `<b>/<i>/<u>/<a>`,
    /// картинки `cid:`); пусто — из `text`.
    public var html = ""
    public var to = ""
    public var cc = ""
    public var subject = ""
    /// Адрес «От кого»; пусто — выбрать по беседе.
    public var from = ""
    public var attachments: [PendingAttachment] = []
    /// Картинки в теле — отдельными блоками после текста (у каждой `contentID`).
    public var images: [PendingAttachment] = []

    public init(
        text: String = "", html: String = "", to: String = "", cc: String = "", subject: String = "", from: String = "",
        attachments: [PendingAttachment] = [], images: [PendingAttachment] = []
    ) {
        self.text = text; self.html = html; self.to = to; self.cc = cc; self.subject = subject; self.from = from
        self.attachments = attachments; self.images = images
    }

    /// «Есть что отправлять»: текст, картинка или хотя бы одно вложение (§1).
    public var canSend: Bool {
        !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || !attachments.isEmpty || !images.isEmpty
    }

    public var isEmpty: Bool { text.isEmpty && attachments.isEmpty && images.isEmpty }

    /// Тело HTML: набранное с форматированием или текст, затем картинки
    /// блоками — каждая своим `<div>`, как у редактора десктопа.
    public var bodyHTML: String {
        let base = html.isEmpty ? (text.isEmpty ? "" : textToHTML(text)) : html
        return base + images.compactMap { i in i.contentID.map { "<div><img src=\"cid:\(escapeHTML($0))\" alt=\"\(escapeHTML(i.name))\"></div>" } }.joined()
    }
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

/// Контекст отправки для групп айдентик (десктоп `send_context`, §1в):
/// `nil` — новое письмо; иначе адрес, на который пришла беседа ответа или
/// пересылки (пусто — «не мой адрес»). `receivedBy` — беседы, откуда
/// отвечают или пересылают; у неявного ответа он берётся из самой беседы.
public func sendContext(_ branch: Branch, receivedBy: String? = nil) -> String? {
    switch branch {
    case .new: return nil
    case let .implicit(c, _, _): return (receivedBy ?? c.receivedBy).lowercased()
    case .reply, .forward: return (receivedBy ?? "").lowercased()
    }
}

/// «От кого» (десктоп §1в): явный выбор; иначе отправитель по документу
/// групп — у ответа и пересылки отправитель группы адреса, на который пришло
/// письмо (не мой адрес — группы по умолчанию ★, строгие пропускаются), у
/// нового письма — группы по умолчанию. Групп ещё нет (документ не пришёл) —
/// адрес беседы, если он свой, иначе адрес по умолчанию учётки. `nil` —
/// выбрать некого: ответ ждёт ручного выбора. Отправитель группы без живой
/// учётки — всё равно он: отправка скажет «Не через что отправить».
public func pickFrom(_ draft: Draft, identities: [Identity], groups: IdentityGroups = .empty, context: String?) -> Identity? {
    func find(_ a: String) -> Identity? { identities.first { $0.email.caseInsensitiveCompare(a) == .orderedSame } }
    let explicit = draft.from.trimmingCharacters(in: .whitespaces)
    if !explicit.isEmpty { return find(explicit) ?? Identity(email: explicit) }
    if !groups.isEmpty { return groups.autoSender(context).map { find($0) ?? Identity(email: $0) } }
    if let r = context, let i = find(r) { return i }
    return identities.first(where: \.isDefault) ?? identities.first
}

/// Выпадашка «От кого», разложенная по учёткам (аудит 2026-11-14 №4): у
/// ddmail-учётки адресов десятки, и адрес внешней учётки (Mail.ru) уходил в
/// хвост длинного меню — выглядел отсутствующим. Сверху — по адресу на
/// учётку (её адрес по умолчанию, в порядке учёток), ниже — остальные адреса.
/// Адрес, повторённый в двух учётках, — один раз, у первой.
public struct FromChoices: Equatable, Sendable {
    /// По адресу на учётку.
    public var accounts: [Identity] = []
    /// Остальные адреса учёток.
    public var others: [Identity] = []

    /// `keep` — какие адреса предлагать (десктоп `offered`: не скрытые и
    /// разрешённые строгой группой в этом контексте; выбранный — всегда).
    public init(_ perAccount: [[Identity]], keep: (String) -> Bool = { _ in true }) {
        let perAccount = perAccount.map { $0.filter { keep($0.email) } }
        var seen = Set<String>()
        for list in perAccount {
            // Адрес по умолчанию; занят другой учёткой — следующий свой.
            let ordered = list.filter(\.isDefault) + list.filter { !$0.isDefault }
            guard let main = ordered.first(where: { !seen.contains($0.email.lowercased()) }) else { continue }
            seen.insert(main.email.lowercased())
            accounts.append(main)
        }
        for list in perAccount {
            for i in list where seen.insert(i.email.lowercased()).inserted { others.append(i) }
        }
    }

    public var all: [Identity] { accounts + others }
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

/// Что ветка выводит сама, без переопределений «Кому» и «Тема» (десктоп
/// `refresh_composer_hints`, §3: подсказки полей показывают ровно это).
/// Новое письмо — ни адресатов, ни темы: адресат — у новой беседы, автотемы
/// у нового письма нет (аудит 2026-11-14 №10). Пересылка — «Fwd: …», «Кому»
/// обязательно.
public func derivedFields(_ branch: Branch, identities: [Identity], groups: IdentityGroups = .empty) -> (to: [String], subject: String) {
    let own = Set(identities.map { $0.email.lowercased() } + groups.groups.flatMap { $0.addresses.map(\.email) })
    switch branch {
    case let .forward(o):
        return ([], "Fwd: \(o.subject)")
    case .new:
        return ([], "")
    case let .reply(o, isGroup):
        var derived = [o.fromAddr.lowercased()]
        if isGroup { derived += (o.to + o.cc).map(bareAddress) }
        var seen = Set<String>()
        derived = derived.filter { !$0.isEmpty && !own.contains($0) && seen.insert($0).inserted }
        return (derived, re(o.subject))
    case let .implicit(c, lastIncoming, _):
        // Беседа «все адреса мои» (письмо себе) собеседников не имеет —
        // отвечаем отправителю последнего входящего (§4 «Роли» десктопа).
        var t = c.counterparts.map(\.addr).filter { !$0.trimmingCharacters(in: .whitespaces).isEmpty }
        if t.isEmpty, let a = [lastIncoming?.fromAddr ?? c.receivedBy].first, !a.isEmpty { t = [a] }
        // Тема — от последнего входящего: на него и отвечают, даже если наше
        // письмо было позже (§1 десктопа).
        return (t, re(lastIncoming?.subject ?? c.lastSubject))
    }
}

/// Собрать письмо по ветке (§1, порт `build` Android). Непустое «Кому»
/// побеждает любую авто-деривацию, непустая тема — авто-«Re:»/«Fwd:»; копия
/// уходит всегда.
public func buildOutgoing(
    _ branch: Branch, draft: Draft, identities: [Identity],
    forwardHTML: String = "", forwardText: String = "", extraAttachments: [PendingAttachment] = [],
    groups: IdentityGroups = .empty, receivedBy: String? = nil
) -> Built {
    if case .forward = branch {} else if !draft.canSend { return .refused("Нечего отправлять.") }
    let toOverride = splitAddresses(draft.to)
    let cc = splitAddresses(draft.cc)
    let subjectOverride = draft.subject.trimmingCharacters(in: .whitespaces).isEmpty ? nil : draft.subject

    let context = sendContext(branch, receivedBy: receivedBy)
    guard let from = pickFrom(draft, identities: identities, groups: groups, context: context) else {
        return .refused(groups.isEmpty
            ? "Нет ни одного своего адреса для отправки."
            : "Ни один адрес не подходит для ответа на это письмо автоматически — выберите его в «От кого».")
    }
    // Строгая группа (§1в) отвечает только на письма своих адресов.
    if !groups.allowed(from.email, context: context) {
        return .refused("Адрес \(from.email) отвечает только на письма, пришедшие на адреса своей группы. Выберите другой адрес в «От кого».")
    }

    let auto = derivedFields(branch, identities: identities, groups: groups)
    let to = toOverride.isEmpty ? auto.to : toOverride
    let subject = subjectOverride ?? auto.subject
    let irt: String, refs: String
    switch branch {
    case .forward:
        if to.isEmpty { return .refused("Укажите, кому переслать.") }
        (irt, refs) = ("", "")
    case .new:
        if to.isEmpty { return .refused("Укажите адресата.") }
        (irt, refs) = ("", "")
    case let .reply(o, _):
        if to.isEmpty { return .refused("Не нашлось, кому ответить.") }
        (irt, refs) = threading(o)
    case let .implicit(_, lastIncoming, last):
        if to.isEmpty { return .refused("У беседы нет адресата.") }
        (irt, refs) = threading(lastIncoming ?? last)
    }
    let atts = (draft.images + draft.attachments + extraAttachments).map {
        OutgoingAttachment(filename: $0.name, mimeType: $0.mime, content: $0.data, contentID: $0.contentID)
    }
    let request = SendRequest(
        from: fromHeader(from), to: to, cc: cc, subject: subject,
        html: draft.bodyHTML + forwardHTML, text: draft.text + forwardText,
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

/// Беседа, которую ждём после отправки нового письма (десктоп
/// `compose_sent_target` / `target_conversation_id`): её id считается по
/// тому же правилу, что сервер и `ConversationBuilder` (набор From+To+Cc, мои
/// адреса — слева от `|`). Надёжнее всего — Message-ID из ответа отправки
/// (сервер кладёт копию в «Отправленные» при приёме и отдаёт его): беседа,
/// в которой лежит письмо, — та самая. Набор собеседников — запасной
/// признак: на случай, если сервер добавит к набору свой адрес иначе, чем мы
/// ожидали.
public struct SentTarget: Equatable, Sendable {
    /// Ожидаемый id беседы.
    public let id: String
    /// Адресаты (To+Cc) в нижнем регистре.
    public let recipients: Set<String>
    /// Message-ID отправленного (из `SendReceipt`); пусто — ещё не знаем.
    public var messageID: String = ""

    public init(id: String, recipients: Set<String>, messageID: String = "") {
        self.id = id
        self.recipients = recipients
        self.messageID = messageID
    }

    /// По письму: отправитель, адресаты, свои адреса учётки-отправителя.
    public init(from: String, to: [String], cc: [String], own: [Identity]) {
        let mineSet = Set(own.map { $0.email.lowercased() })
        let sender = bareAddress(from)
        let p = ConversationBuilder.participants(from: sender, to: to.map(bareAddress), cc: cc.map(bareAddress))
        let mine = p.filter { mineSet.contains($0) || $0 == sender }
        let others = p.filter { !mineSet.contains($0) && $0 != sender }
        self.id = ConversationBuilder.conversationID(mine: mine, others: others, own: sender)
        self.recipients = Set((to + cc).map(bareAddress).filter { !$0.isEmpty })
    }

    /// Пришла ли это беседа: id совпал (у IMAP-учётки и ddmail он один и
    /// тот же), иначе набор собеседников равен адресатам (не спам).
    public func matches(_ c: Conversation) -> Bool {
        if c.spam { return false }
        if !messageID.isEmpty {
            let mid = normalizedMessageID(messageID)
            if c.messages.contains(where: { normalizedMessageID($0.messageID) == mid }) { return true }
        }
        if c.id.lowercased() == id { return true }
        let cps = Set(c.counterparts.map { $0.addr.lowercased() }.filter { !$0.isEmpty })
        let others = recipients.subtracting(own)
        return !others.isEmpty && cps == others
    }

    /// Свои адреса из id (`мои|…`): в наборе собеседников их нет.
    private var own: Set<String> {
        let left = id.split(separator: "|", maxSplits: 1).first.map(String.init) ?? ""
        return Set(left.split(separator: "+").map(String.init))
    }
}

/// Лимит сервера — 30 МБ на запрос в base64; сырых байт — около 22.
public let maxOutgoingAttachmentBytes = 22 << 20
