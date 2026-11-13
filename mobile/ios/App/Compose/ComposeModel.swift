import DDMailAPI
import DDMailSync
import Foundation
import Observation
import SwiftUI
import UniformTypeIdentifiers

/// Отправка (Android §5, десктоп §1–2): ветки, мгновенный пузырь-заглушка в
/// ленте, «✓ Отправлено» без тостов, при ошибке — заглушка убирается, текст
/// возвращается в композер.
///
/// Отправка асинхронная: сервер ставит письмо в очередь и сразу кладёт копию
/// в «Отправленные» (беседа есть сразу, статус доставки — на письме, §5);
/// синк — сразу и ещё несколько раз, на случай, если WebSocket нет.
@MainActor @Observable
final class ComposeModel {
    /// Отдельное окно: новое письмо или пересылка.
    struct Sheet: Identifiable {
        let id = UUID()
        var branch: Branch
        var draft: Draft
        var error: String?
        var busy = false

        var isForward: Bool { if case .forward = branch { true } else { false } }
        var original: MessageBody? { if case let .forward(o) = branch { o } else { nil } }
    }

    @ObservationIgnored private unowned let workspace: Workspace
    private let conversation: ConversationModel
    /// Синк учётки, через которую ушло письмо.
    private let syncSoon: (String) -> Void
    private let onAuthDead: @MainActor (String) -> Void

    /// Черновик ответа в открытой беседе. Его `text` — зеркало `rich`.
    var draft = Draft()
    /// Тело с форматированием (десктоп §3а) — источник истины строки ввода.
    var rich = AttributedString() {
        didSet { draft.text = RichText.plain(rich) }
    }
    /// Выделение в строке ввода: на него действуют ⌘B/⌘I/⌘U/⌘K.
    var selection = AttributedTextSelection()
    /// Явный ответ на конкретное письмо («Ответ на …»).
    var replyTo: MessageBody?
    /// Показаны поля «Кому», «Копия», «Тема», «От».
    var expanded = false
    /// «✓ Отправлено» ~2 с.
    private(set) var sentFlash = false
    var error: String?
    var sheet: Sheet?

    /// После нового письма — открыть его беседу, как только синк её принесёт
    /// (десктоп `compose_sent_target`).
    private(set) var sentTarget: SentTarget?
    /// Заглушки нового письма: переезжают в беседу, на которую сменится
    /// «Новая беседа», пока настоящее письмо не пришло (десктоп §2).
    private(set) var carriedStubs: [String] = []
    private var stubSeq = 0

    init(workspace: Workspace, conversation: ConversationModel, syncSoon: @escaping (String) -> Void, onAuthDead: @escaping @MainActor (String) -> Void) {
        self.workspace = workspace
        self.conversation = conversation
        self.syncSoon = syncSoon
        self.onAuthDead = onAuthDead
    }

    /// Свои адреса всех учёток; учётка открытой беседы — первой, чтобы при
    /// одинаковом адресе в двух учётках ответ ушёл через свою (`owner_for`).
    var identities: [Identity] {
        let prefer = conversation.conversation?.accountKey
        let ordered = workspace.links.sorted { ($0.account.key == prefer ? 0 : 1) < ($1.account.key == prefer ? 0 : 1) }
        var seen = Set<String>()
        return ordered.flatMap { $0.provider.identities() }.filter { seen.insert($0.email.lowercased()).inserted }
    }

    /// Учётка для отправки — владелец адреса From (как `owner_for` десктопа):
    /// учётка открытой беседы, если адрес её; иначе первая владеющая.
    func owner(of request: SendRequest) -> AccountLink? {
        let from = bareAddress(request.from)
        let owners = workspace.links.filter { l in l.provider.identities().contains { $0.email.lowercased() == from } }
        if let prefer = conversation.conversation?.accountKey, let l = owners.first(where: { $0.account.key == prefer }) { return l }
        return owners.first
    }

    /// Отправить через учётку-владельца адреса From; нет владельца — отказ.
    /// Ответ — учётка и итог отправки (Message-ID письма).
    private func send(_ request: SendRequest) async throws -> (account: String, receipt: SendReceipt) {
        guard let link = owner(of: request) else { throw SendRefused() }
        do {
            let receipt = try await link.provider.send(request)
            return (link.account.key, receipt)
        } catch {
            if isAuthFailure(error) { onAuthDead(link.account.key) }
            // IMAP-учётка отправляет сама (SMTP провайдера): отказ SMTP —
            // письмо не доставлено, а не «сервер не принял в очередь».
            if link.imap != nil, !isAuthFailure(error) { throw NotDelivered(underlying: error) }
            throw error
        }
    }

    struct SendRefused: Error, LocalizedError {
        var errorDescription: String? { "Не через что отправить: адрес «От» не принадлежит ни одной учётке" }
    }

    /// Ошибка SMTP IMAP-учётки — «Не доставлено» с причиной.
    struct NotDelivered: Error {
        let underlying: Error
    }

    /// Сменилась беседа — явный ответ и переопределения не переезжают в другую.
    func reset() {
        rich = AttributedString()
        selection = AttributedTextSelection()
        draft = Draft()
        replyTo = nil
        expanded = false
        error = nil
    }

    func openForward(_ b: MessageBody) { sheet = Sheet(branch: .forward(b), draft: Draft()) }
    /// Беседа нашлась: цель снята, заглушки — ей.
    func consumeTarget() -> [String] {
        sentTarget = nil
        defer { carriedStubs = [] }
        return carriedStubs
    }

    // MARK: Вложения

    /// Файлы из системного выбора; общий размер — в пределах лимита сервера.
    func attach(_ urls: [URL], toSheet: Bool) {
        Task {
            let picked = await Task.detached { urls.compactMap(Self.read) }.value
            let current = toSheet ? (sheet?.draft.attachments ?? []) : draft.attachments
            let total = (current + picked).reduce(0) { $0 + $1.size }
            if total > maxOutgoingAttachmentBytes {
                let msg = "Вложения больше \(maxOutgoingAttachmentBytes >> 20) МБ — сервер такое не примет."
                if toSheet { sheet?.error = msg } else { error = msg }
                return
            }
            if toSheet { sheet?.draft.attachments += picked } else { draft.attachments += picked }
        }
    }

    func attach(data: Data, name: String, type: UTType?, toSheet: Bool) {
        let a = PendingAttachment(name: name, mime: type?.preferredMIMEType ?? "application/octet-stream", data: data)
        if toSheet { sheet?.draft.attachments.append(a) } else { draft.attachments.append(a) }
    }

    /// Картинка в тело письма — своим блоком после текста, уходит inline
    /// (`cid:`, десктоп §3а). Из буфера обмена и из «Фото».
    func addImage(data: Data, name: String, mime: String) {
        let total = (draft.attachments + draft.images).reduce(data.count) { $0 + $1.size }
        if total > maxOutgoingAttachmentBytes {
            error = "Вложения больше \(maxOutgoingAttachmentBytes >> 20) МБ — сервер такое не примет."
            return
        }
        let cid = "img\(UUID().uuidString.lowercased().replacingOccurrences(of: "-", with: ""))@ddmail"
        draft.images.append(PendingAttachment(name: name, mime: mime, data: data, contentID: cid))
        error = nil
    }

    /// «Картинка из буфера».
    func pasteImage() {
        guard let png = Pasteboard.imagePNG else {
            error = "В буфере обмена нет картинки."
            return
        }
        addImage(data: png, name: "image-\(draft.images.count + 1).png", mime: "image/png")
    }

    /// ⌘K / «Ссылка из буфера»: адрес из буфера обмена — на выделение.
    func linkFromPasteboard() {
        if !RichText.link(text: &rich, selection: &selection, url: Pasteboard.url) {
            error = "В буфере обмена нет адреса ссылки (http, https или mailto)."
        }
    }

    func toggle(_ t: RichText.Toggle, _ ctx: Font.Context) {
        RichText.toggle(t, text: &rich, selection: &selection, ctx)
    }

    /// Черновик к отправке: HTML-версия тела — из форматированного текста.
    private func prepared(_ ctx: Font.Context) -> Draft {
        var d = draft
        d.text = RichText.plain(rich)
        d.html = RichText.html(rich, ctx)
        return d
    }

    /// Вернуть неотправленное в композер, если тот пуст.
    private func restore(_ d: Draft, _ r: AttributedString) {
        guard draft.isEmpty else { return }
        rich = r
        draft = d
        draft.html = ""
    }

    nonisolated private static func read(_ url: URL) -> PendingAttachment? {
        let scoped = url.startAccessingSecurityScopedResource()
        defer { if scoped { url.stopAccessingSecurityScopedResource() } }
        guard let data = try? Data(contentsOf: url) else { return nil }
        let mime = UTType(filenameExtension: url.pathExtension)?.preferredMIMEType ?? "application/octet-stream"
        return PendingAttachment(name: url.lastPathComponent, mime: mime, data: data)
    }

    // MARK: Отправка

    /// Ответ в открытой беседе (ветки 2–3).
    func sendReply(_ c: Conversation, _ ctx: Font.Context) {
        let branch: Branch = replyTo.map { .reply($0, isGroup: c.isGroup) }
            ?? .implicit(c, lastIncoming: conversation.lastIncoming, last: conversation.last)
        let d = prepared(ctx)
        switch buildOutgoing(branch, draft: d, identities: identities) {
        case let .refused(reason):
            error = reason
        case let .ok(request, _):
            let sent = d
            let sentRich = rich
            let sentReply = replyTo
            stubSeq += 1
            let key = "s\(stubSeq)"
            clearFailedStubs()
            conversation.web.run("ddm.stub(\(Self.stubJSON(key, sent)))")
            reset()
            Task {
                do {
                    let via = try await send(request)
                    flash()
                    afterSend(via.account)
                } catch {
                    // Текст возвращается, если композер пуст (не затираем набранное заново).
                    restore(sent, sentRich)
                    if replyTo == nil { replyTo = sentReply }
                    await failed(key, error)
                }
            }
        }
    }

    /// Первое письмо новой беседы из поиска (десктоп `enter_compose_mode`):
    /// строка ввода под пустой лентой, ветка «новое письмо», адресат — адрес
    /// беседы (явное «Кому» его побеждает, §1). Пузырь-заглушка — сразу;
    /// беседа откроется, как только письмо придёт синком (`sentTarget`).
    func sendNew(to address: String, _ ctx: Font.Context) {
        var d = prepared(ctx)
        if d.to.trimmingCharacters(in: .whitespaces).isEmpty { d.to = address }
        switch buildOutgoing(.new, draft: d, identities: identities) {
        case let .refused(reason):
            error = reason
        case let .ok(request, _):
            var sent = d
            sent.to = draft.to
            let sentRich = rich
            stubSeq += 1
            let key = "s\(stubSeq)"
            let stub = "ddm.stub(\(Self.stubJSON(key, sent)))"
            clearFailedStubs()
            conversation.web.run(stub)
            reset()
            let target = target(of: request)
            sentTarget = target
            carriedStubs = [stub]
            Task {
                do {
                    let via = try await send(request)
                    // Копия уже в «Отправленных»: беседа узнается и по Message-ID.
                    noteSent(target, via.receipt)
                    flash()
                    afterSend(via.account)
                } catch {
                    if sentTarget == target { sentTarget = nil }
                    carriedStubs = []
                    restore(sent, sentRich)
                    await failed(key, error)
                }
            }
        }
    }

    /// Заглушки писем, которые SMTP IMAP-учётки не принял: висят красным до
    /// следующей отправки (как Android §5).
    private var failedStubs: [String] = []

    private func clearFailedStubs() {
        for k in failedStubs { conversation.web.run("ddm.unstub(\"\(k)\")") }
        failedStubs.removeAll()
    }

    /// Отправка не удалась. Отказ SMTP IMAP-учётки — письмо не доставлено:
    /// заглушка остаётся с красной плашкой «Не доставлено: причина»
    /// (`ddm.stubDelivery`), строки над композером нет. Документ без
    /// `stubDelivery` или отказ сервера ddmail (письмо не принято) — заглушка
    /// убирается, причина — красной строкой над композером.
    private func failed(_ key: String, _ e: Error) async {
        let text = Self.sendError(e)
        if let nd = e as? NotDelivered {
            let d = FeedBatch.Delivery(status: DeliveryState.failed.rawValue, error: Self.reason(nd), failed: [])
            let js = "(typeof ddm.stubDelivery==='function')?(ddm.stubDelivery(\"\(key)\",\(d.javaScriptLiteral())),true):false"
            if await conversation.web.ask(js) {
                failedStubs.append(key)
                return
            }
        }
        conversation.web.run("ddm.unstub(\"\(key)\")")
        error = text
    }

    /// Беседа, в которую ляжет новое письмо: id по набору адресов и своим
    /// адресам учётки, через которую оно уйдёт.
    private func target(of r: SendRequest) -> SentTarget {
        let own = owner(of: r)?.provider.identities() ?? identities
        return SentTarget(from: r.from, to: r.to, cc: r.cc, own: own)
    }

    /// Письмо принято: ждём беседу ещё и по его Message-ID — если ждём всё
    /// ту же (за это время могли отправить другое или уйти).
    private func noteSent(_ target: SentTarget, _ r: SendReceipt) {
        guard !r.rfcMessageID.isEmpty, var t = sentTarget, t == target else { return }
        t.messageID = r.rfcMessageID
        sentTarget = t
    }

    /// Новое письмо или пересылка (ветки 0–1) — из отдельного окна.
    func sendSheet() {
        guard var s = sheet, !s.busy else { return }
        s.busy = true
        s.error = nil
        sheet = s
        let snapshot = s
        Task {
            var fwdText = "", fwdHTML = ""
            var extra: [PendingAttachment] = []
            if let o = snapshot.original {
                (fwdText, fwdHTML) = forwardBlock(o)
                // Пересылка несёт вложения исходного письма.
                do {
                    guard let p = conversation.conversation.flatMap({ workspace.link($0.accountKey)?.provider }) else {
                        fail("Беседа пересылаемого письма закрыта.")
                        return
                    }
                    for a in o.attachments {
                        let bin = try await p.attachment(uid: o.uid, index: a.index)
                        extra.append(PendingAttachment(name: a.filename, mime: a.mimeType.isEmpty ? bin.contentType : a.mimeType, data: bin.data))
                    }
                } catch {
                    fail("Не удалось забрать вложения пересылаемого письма: \(Self.sendError(error))")
                    return
                }
            }
            switch buildOutgoing(snapshot.branch, draft: snapshot.draft, identities: identities,
                                 forwardHTML: fwdHTML, forwardText: fwdText, extraAttachments: extra) {
            case let .refused(reason):
                fail(reason)
            case let .ok(request, _):
                do {
                    let via = try await send(request)
                    if case .new = snapshot.branch {
                        var t = target(of: request)
                        t.messageID = via.receipt.rfcMessageID
                        sentTarget = t
                    }
                    sheet = nil
                    flash()
                    afterSend(via.account)
                } catch {
                    fail(Self.sendError(error))
                }
            }
        }

        func fail(_ message: String) {
            sheet?.busy = false
            sheet?.error = message
        }
    }

    /// «✓ Отправлено» над композером ~2 с (десктоп §2). Повторная отправка
    /// продлевает плашку, а не гасит её раньше времени.
    private var flashSeq = 0
    private func flash() {
        flashSeq += 1
        let mine = flashSeq
        sentFlash = true
        Task {
            try? await Task.sleep(for: .seconds(2))
            if flashSeq == mine { sentFlash = false }
        }
    }

    /// Подтянуть копию в «Отправленных» и статус доставки: сразу и затем с
    /// затухающими интервалами (как `schedule_post_send_refetch` десктопа:
    /// 2.5 → 7 → 15 → 30 → 60 с) — подстраховка, если WebSocket нет. У
    /// ddmail копия ложится при приёме, у IMAP-учётки — когда клиент найдёт
    /// или положит её сам (`ensureSentCopy`, 5–20 с).
    private func afterSend(_ key: String) {
        syncSoon(key)
        Task {
            for d in [2.5, 4.5, 8, 15, 30] {
                try? await Task.sleep(for: .seconds(d))
                syncSoon(key)
            }
        }
    }

    /// Почему SMTP IMAP-учётки не принял письмо — словами.
    static func reason(_ nd: NotDelivered) -> String {
        switch nd.underlying {
        case APIError.network: "нет соединения с SMTP-сервером"
        case let APIError.server(_, message) where !message.isEmpty: message
        default: nd.underlying.localizedDescription
        }
    }

    static func sendError(_ e: Error) -> String {
        if let nd = e as? NotDelivered { return "Не доставлено: \(reason(nd))." }
        let why: String
        switch e {
        case APIError.unauthorized: why = "нужен вход в учётку"
        case is SendRefused: return e.localizedDescription + "."
        case APIError.network: why = "нет соединения с сервером"
        case let APIError.server(code, _): why = code == 413 ? "вложения больше лимита сервера" : "сервер ответил ошибкой \(code)"
        default: why = e.localizedDescription
        }
        return "Не отправлено: \(why)."
    }

    /// Заглушка для документа ленты: `{key, text, html, attachments:[{filename,size}]}`.
    /// Заглушка выглядит как письмо (десктоп §2): тот же HTML, картинки тела
    /// вшиты `data:`-URI — `cid:` разрешается по частям, которых сервер ещё не видел.
    static func stubJSON(_ key: String, _ d: Draft) -> String {
        struct A: Encodable { let filename: String; let size: Int }
        struct S: Encodable { let key: String; let text: String; let html: String; let attachments: [A] }
        var html = d.bodyHTML
        for i in d.images {
            guard let cid = i.contentID else { continue }
            html = html.replacingOccurrences(of: "cid:\(cid)", with: "data:\(i.mime);base64,\(i.data.base64EncodedString())")
        }
        let s = S(key: key, text: d.text, html: html, attachments: d.attachments.map { A(filename: $0.name, size: $0.size) })
        let json = String(decoding: (try? JSONEncoder().encode(s)) ?? Data("{}".utf8), as: UTF8.self)
        return json.replacingOccurrences(of: "\u{2028}", with: "\\u2028").replacingOccurrences(of: "\u{2029}", with: "\\u2029")
    }
}
