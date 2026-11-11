import DDMailAPI
import DDMailSync
import Foundation
import Observation
import UniformTypeIdentifiers

/// Отправка (Android §5, десктоп §1–2): ветки, мгновенный пузырь-заглушка в
/// ленте, «✓ Отправлено» без тостов, при ошибке — заглушка убирается, текст
/// возвращается в композер.
///
/// Отправка асинхронная: сервер ставит письмо в очередь, копия в
/// «Отправленных» (и сама беседа) появляется после доставки — поэтому после
/// отправки синхронизируемся ещё несколько раз, пока нет WebSocket.
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

    private let engine: SyncEngine
    private let conversation: ConversationModel
    private let syncSoon: () -> Void
    private let onAuthDead: () -> Void

    /// Черновик ответа в открытой беседе.
    var draft = Draft()
    /// Явный ответ на конкретное письмо («Ответ на …»).
    var replyTo: MessageBody?
    /// Показаны поля «Кому», «Копия», «Тема», «От».
    var expanded = false
    /// «✓ Отправлено» ~2 с.
    private(set) var sentFlash = false
    var error: String?
    var sheet: Sheet?

    /// После нового письма — открыть беседу с этими адресатами, когда появится.
    private(set) var sentTarget: Set<String>?
    private var stubSeq = 0

    init(engine: SyncEngine, conversation: ConversationModel, syncSoon: @escaping () -> Void, onAuthDead: @escaping () -> Void) {
        self.engine = engine
        self.conversation = conversation
        self.syncSoon = syncSoon
        self.onAuthDead = onAuthDead
    }

    var identities: [Identity] { engine.identities() }

    /// Сменилась беседа — явный ответ и переопределения не переезжают в другую.
    func reset() {
        draft = Draft()
        replyTo = nil
        expanded = false
        error = nil
    }

    func openNew(to: String = "") { sheet = Sheet(branch: .new, draft: Draft(to: to)) }
    func openForward(_ b: MessageBody) { sheet = Sheet(branch: .forward(b), draft: Draft()) }
    func consumeTarget() { sentTarget = nil }

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

    nonisolated private static func read(_ url: URL) -> PendingAttachment? {
        let scoped = url.startAccessingSecurityScopedResource()
        defer { if scoped { url.stopAccessingSecurityScopedResource() } }
        guard let data = try? Data(contentsOf: url) else { return nil }
        let mime = UTType(filenameExtension: url.pathExtension)?.preferredMIMEType ?? "application/octet-stream"
        return PendingAttachment(name: url.lastPathComponent, mime: mime, data: data)
    }

    // MARK: Отправка

    /// Ответ в открытой беседе (ветки 2–3).
    func sendReply(_ c: Conversation) {
        let branch: Branch = replyTo.map { .reply($0, isGroup: c.isGroup) }
            ?? .implicit(c, lastIncoming: conversation.lastIncoming, last: conversation.last)
        switch buildOutgoing(branch, draft: draft, identities: identities) {
        case let .refused(reason):
            error = reason
        case let .ok(request, _):
            let sent = draft
            let sentReply = replyTo
            stubSeq += 1
            let key = "s\(stubSeq)"
            conversation.web.run("ddm.stub(\(Self.stubJSON(key, sent)))")
            reset()
            Task {
                do {
                    try await engine.send(request)
                    flash()
                    afterSend()
                } catch {
                    if case APIError.unauthorized = error { onAuthDead() }
                    conversation.web.run("ddm.unstub(\"\(key)\")")
                    // Текст возвращается, если композер пуст (не затираем набранное заново).
                    if draft.isEmpty { draft = sent }
                    if replyTo == nil { replyTo = sentReply }
                    self.error = Self.sendError(error)
                }
            }
        }
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
                    for a in o.attachments {
                        let bin = try await engine.attachment(uid: o.uid, index: a.index)
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
            case let .ok(request, to):
                do {
                    try await engine.send(request)
                    if case .new = snapshot.branch { sentTarget = Set(to.map { $0.lowercased() }) }
                    sheet = nil
                    flash()
                    afterSend()
                } catch {
                    if case APIError.unauthorized = error { onAuthDead() }
                    fail(Self.sendError(error))
                }
            }
        }

        func fail(_ message: String) {
            sheet?.busy = false
            sheet?.error = message
        }
    }

    private func flash() {
        sentFlash = true
        Task {
            try? await Task.sleep(for: .seconds(2))
            sentFlash = false
        }
    }

    /// Копия в «Отправленных» появится после доставки — подтягиваем её.
    private func afterSend() {
        Task {
            for d in [2, 6, 15, 40] {
                try? await Task.sleep(for: .seconds(d))
                syncSoon()
            }
        }
    }

    static func sendError(_ e: Error) -> String {
        let why: String
        switch e {
        case APIError.unauthorized: why = "сессия истекла — войдите заново"
        case APIError.network: why = "нет соединения с сервером"
        case let APIError.server(code, _): why = code == 413 ? "вложения больше лимита сервера" : "сервер ответил ошибкой \(code)"
        default: why = e.localizedDescription
        }
        return "Не отправлено: \(why)."
    }

    /// Заглушка для документа ленты: `{key, text, attachments:[{filename,size}]}`.
    static func stubJSON(_ key: String, _ d: Draft) -> String {
        struct A: Encodable { let filename: String; let size: Int }
        struct S: Encodable { let key: String; let text: String; let attachments: [A] }
        let s = S(key: key, text: d.text, attachments: d.attachments.map { A(filename: $0.name, size: $0.size) })
        let json = String(decoding: (try? JSONEncoder().encode(s)) ?? Data("{}".utf8), as: UTF8.self)
        return json.replacingOccurrences(of: "\u{2028}", with: "\\u2028").replacingOccurrences(of: "\u{2029}", with: "\\u2029")
    }
}
