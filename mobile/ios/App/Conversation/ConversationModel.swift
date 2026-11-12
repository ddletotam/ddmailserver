import DDMailAPI
import DDMailSync
import Foundation
import Observation

/// Открытая беседа (контракт §8, порт `ConversationController` Android): окно
/// отрисовки по 20 писем — сначала последние, старые порциями по просьбе
/// документа; новые письма открытой беседы дописываются снизу.
@MainActor @Observable
final class ConversationModel {
    let web = ConversationWeb()
    let gate = RemoteImageGate()
    /// Источник почты по учётке беседы.
    private let provider: @MainActor (String) -> (any MailProvider)?
    private let onAuthDead: @MainActor (String) -> Void

    private(set) var conversation: Conversation?
    /// Веб-представление создано: контейнер на экране должен его вставить
    /// (оно готовится асинхронно и может опоздать к первой отрисовке).
    private(set) var webPrepared = false
    private var window = FeedWindow([])
    private var shown = Set<Int64>()
    /// Тела показанных писем — для веток ответа и меню пузыря.
    private(set) var bodies: [Int64: MessageBody] = [:]
    private var loading: Task<Void, Never>?

    /// Меню пузыря (долгое нажатие): письмо.
    var bubbleMenu: MessageBody?
    /// Меню вложения (долгое нажатие).
    var attachmentMenu: AttachmentRef?
    /// Открытое во QuickLook вложение.
    var preview: URL?
    /// Вложение, которое сохраняют в «Файлы».
    var exporting: URL?
    var notice: String?

    struct AttachmentRef: Identifiable, Equatable {
        let uid: Int64
        let index: Int
        let name: String
        var id: String { "\(uid)/\(index)" }
    }

    init(provider: @escaping @MainActor (String) -> (any MailProvider)?, onAuthDead: @escaping @MainActor (String) -> Void) {
        self.provider = provider
        self.onAuthDead = onAuthDead
        web.host = self
        Task {
            await web.prepare()
            webPrepared = true
        }
    }

    /// Последнее входящее среди показанных: на него отвечает неявный ответ.
    var lastIncoming: MessageBody? { bodies.values.filter { !$0.isOutgoing }.max { $0.dateTs < $1.dateTs } }
    var last: MessageBody? { bodies.values.max { $0.dateTs < $1.dateTs } }

    func open(_ c: Conversation?) {
        loading?.cancel()
        web.reset()
        conversation = c
        shown.removeAll()
        bodies.removeAll()
        // До документа новой беседы: прежние разрешения ей не достаются.
        gate.open(spam: c?.spam == true, uids: c?.messages.map(\.uid) ?? [])
        web.run("ddm.set({messages:[],hasOlder:false})")
        guard let c else { return }
        window = FeedWindow(c.messages)
        let first = window.first
        let focus = window.focus
        loading = Task {
            let loaded = await load(first, of: c)
            guard !Task.isCancelled, conversation?.ref == c.ref else { return }
            send("set", c, loaded, focus: focus)
        }
    }

    /// Беседа обновилась дельтой: новые письма — снизу, без перерисовки.
    func updated(_ c: Conversation) {
        guard conversation?.ref == c.ref else { return }
        conversation = c
        let fresh = window.fresh(c.messages)
        guard !fresh.isEmpty else { return }
        gate.extend(fresh.map(\.uid))
        Task {
            let loaded = await load(fresh, of: c)
            guard conversation?.ref == c.ref else { return }
            send("append", c, loaded, focus: nil)
        }
    }

    private func older() {
        guard let c = conversation else { return }
        let next = window.older()
        guard !next.isEmpty else { return }
        Task {
            let loaded = await load(next, of: c)
            guard conversation?.ref == c.ref else { return }
            send("prepend", c, loaded, focus: nil)
        }
    }

    private func load(_ refs: [MessageRef], of c: Conversation) async -> [MessageBody] {
        guard let p = provider(c.accountKey) else { return [] }
        do {
            let got = try await p.bodies(refs)
            for b in got { bodies[b.uid] = b }
            return got
        } catch {
            if isAuthFailure(error) { onAuthDead(c.accountKey) }
            return []
        }
    }

    /// Источник почты открытой беседы.
    private var current: (any MailProvider)? { conversation.flatMap { provider($0.accountKey) } }

    private func send(_ op: String, _ c: Conversation, _ list: [MessageBody], focus: Int64?) {
        let fresh = list.filter { shown.insert($0.uid).inserted }
        let batch = FeedBatch(conversation: c, bodies: fresh, hasOlder: window.hasOlder, focusUid: focus, imageSenders: ImageSenders.all)
        web.run("ddm.\(op)(\(batch.javaScriptLiteral()))")
    }

    // MARK: Вложения

    func attachmentName(uid: Int64, index: Int) -> String {
        bodies[uid]?.attachments.first { $0.index == index }?.filename ?? "attachment-\(index)"
    }

    /// Скачать вложение во временный каталог (имя — безопасное).
    func download(uid: Int64, index: Int) async -> URL? {
        let name = safeFileName(attachmentName(uid: uid, index: index))
        do {
            guard let p = current else { return nil }
            let bin = try await p.attachment(uid: uid, index: index)
            // Учёток несколько — id писем разных учёток могут совпасть: каталог по учётке.
            let account = abs((conversation?.accountKey ?? "").hashValue)
            let dir = URL.temporaryDirectory.appending(path: "attachments/\(account)/\(uid)-\(index)")
            try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
            let file = dir.appending(path: name)
            try bin.data.write(to: file, options: .atomic)
            return file
        } catch {
            if isAuthFailure(error), let k = conversation?.accountKey { onAuthDead(k) }
            notice = "Не удалось скачать «\(name)»: \(error.localizedDescription)"
        }
        return nil
    }

    func openAttachment(uid: Int64, index: Int) {
        Task { if let url = await download(uid: uid, index: index) { preview = url } }
    }

    func saveAttachment(uid: Int64, index: Int) {
        Task { if let url = await download(uid: uid, index: index) { exporting = url } }
    }
}

extension ConversationModel: ConversationWebHost {
    func webReady() {}
    func webRequestOlder() { older() }
    func webOpenLink(_ url: URL) { LinkOpener.open(url) }
    func webOpenAttachment(uid: Int64, index: Int) { openAttachment(uid: uid, index: index) }

    func webAttachmentMenu(uid: Int64, index: Int) {
        attachmentMenu = AttachmentRef(uid: uid, index: index, name: attachmentName(uid: uid, index: index))
    }

    func webBubbleMenu(uid: Int64) { bubbleMenu = bodies[uid] }

    /// «Всегда показывать картинки от отправителя». У спама такой кнопки нет,
    /// и сохранять разрешение по спаму нельзя: его отправитель — кто угодно.
    func webAllowSender(uid: Int64) {
        guard conversation?.spam != true, let addr = bodies[uid]?.fromAddr, !addr.isEmpty else { return }
        ImageSenders.allow(addr)
    }

    func webAllowSpamImages(uid: Int64) { gate.allow(uid) }

    func webPart(uid: Int64, cid: String) async throws -> (Data, String) {
        guard let p = current else { throw APIError.network("беседа закрыта") }
        let b = try await p.part(uid: uid, cid: cid)
        return (b.data, b.contentType)
    }

    var webGate: RemoteImageGate { gate }
}

/// Отправители, от которых картинки показываются всегда (§5) — на устройстве.
enum ImageSenders {
    private static let key = "mail.imageSenders"

    static var all: Set<String> { Set(UserDefaults.standard.stringArray(forKey: key) ?? []) }

    static func allow(_ addr: String) {
        var s = all
        s.insert(addr.lowercased())
        UserDefaults.standard.set(Array(s).sorted(), forKey: key)
    }

    /// Выход из учётки: разрешения чужого человека не остаются.
    static func clear() { UserDefaults.standard.removeObject(forKey: key) }
}
