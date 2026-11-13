import DDMailAPI
import DDMailSync
import Foundation
import Observation
#if os(macOS)
import AppKit
#endif

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
    /// Заглушки, которые встанут в ленту следующей открытой беседы (новое
    /// письмо ушло, «Новая беседа» сменилась настоящей до его прихода).
    var carry: [String] = []

    /// Действия меню пузыря, которые делает композер экрана (у окна беседы
    /// Мака — свой композер). Ставит `ConversationView`.
    struct Actions {
        var reply: (@MainActor (MessageBody) -> Void)?
        var forward: (@MainActor (MessageBody) -> Void)?
    }

    @ObservationIgnored var actions = Actions()
    /// Открытое во QuickLook вложение.
    var preview: URL?
    /// Вложение, которое сохраняют в «Файлы».
    var exporting: URL?
    var notice: String?
    /// «Удалить письмо» ждёт подтверждения (удаление необратимо).
    var confirmDelete: MessageBody?
    /// «Показать… → Заголовки / Исходник»: открытый просмотр.
    var source: SourceText?
    /// Письма, показанные текстовой версией (`ddm.view`).
    private var textView = Set<Int64>()
    /// Скачанные для просмотра вложения: удаляются, когда открыта другая беседа.
    private var downloaded: [URL] = []

    /// Текст для просмотра «Заголовки / Исходник».
    struct SourceText: Identifiable {
        let id = UUID()
        let title: String
        let text: String
        /// Показано не всё (огромное письмо с вложениями); копируется всё.
        let full: String
    }

    init(provider: @escaping @MainActor (String) -> (any MailProvider)?, onAuthDead: @escaping @MainActor (String) -> Void) {
        self.provider = provider
        self.onAuthDead = onAuthDead
        AttachmentFiles.purgeOnce()
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
        if c?.ref != conversation?.ref { dropDownloads() }
        conversation = c
        shown.removeAll()
        bodies.removeAll()
        textView.removeAll()
        // До документа новой беседы: прежние разрешения ей не достаются.
        gate.open(spam: c?.spam == true, uids: c?.messages.map(\.uid) ?? [])
        web.run("ddm.set({messages:[],hasOlder:false})")
        let stubs = carry
        carry = []
        guard let c else { return }
        window = FeedWindow(c.messages)
        let first = window.first
        let focus = window.focus
        loading = Task {
            let loaded = await load(first, of: c)
            guard !Task.isCancelled, conversation?.ref == c.ref else { return }
            send("set", c, loaded, focus: focus)
            stubs.forEach { web.run($0) }
        }
    }

    /// Беседа обновилась (дельта, событие доставки): новые письма — снизу,
    /// у показанных со сменившимся статусом доставки пузырь перерисовывается
    /// на месте — без перезагрузки ленты.
    func updated(_ c: Conversation) {
        guard conversation?.ref == c.ref else { return }
        conversation = c
        // Письма, которых в беседе больше нет (удалены здесь или на другом
        // устройстве), — пузыри прочь.
        let gone = window.removed(c.messages)
        if !gone.isEmpty { dropFromFeed(gone) }
        let redelivered = window.redelivered(c.messages).filter { shown.contains($0.uid) }
        if !redelivered.isEmpty {
            Task {
                // Тело — из кэша (событие уже положило статус) или с сервера
                // (статус пришёл дельтой, `SyncEngine.bodies` перезапросит).
                let loaded = await load(redelivered, of: c)
                guard conversation?.ref == c.ref else { return }
                refresh(c, loaded)
            }
        }
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
        logDelivery(op, fresh)
        web.run("ddm.\(op)(\(batch.javaScriptLiteral()))")
    }

    /// Перерисовать показанные письма с новым статусом доставки
    /// (`FeedBatch.updateScript`: на месте, если документ умеет).
    private func refresh(_ c: Conversation, _ list: [MessageBody]) {
        let changed = list.filter { shown.contains($0.uid) }
        guard !changed.isEmpty else { return }
        let senders = ImageSenders.all
        let all = bodies.values.filter { shown.contains($0.uid) }
        let update = FeedBatch(conversation: c, bodies: changed, hasOlder: window.hasOlder, focusUid: nil, imageSenders: senders)
        let full = FeedBatch(conversation: c, bodies: all, hasOlder: window.hasOlder, focusUid: nil, imageSenders: senders)
        logDelivery("update", changed)
        web.run(FeedBatch.updateScript(update, orSet: full))
    }

    /// Что ушло в документ о доставке своих писем: так видно, что поле
    /// `delivery` дошло до ленты, даже если документ его ещё не рисует.
    private func logDelivery(_ op: String, _ list: [MessageBody]) {
        for b in list where b.isOutgoing && !b.deliveryStatus.isEmpty {
            webLog.notice("feed \(op, privacy: .public): uid \(b.uid) delivery=\(b.deliveryStatus, privacy: .public) error=\(b.deliveryError, privacy: .public) failed=\(b.deliveryFailed.count)")
        }
    }

    /// Убрать письма из ленты (удалены): пузыри, тела, окно.
    private func dropFromFeed(_ uids: [Int64]) {
        guard !uids.isEmpty else { return }
        for u in uids {
            shown.remove(u)
            bodies[u] = nil
            textView.remove(u)
        }
        web.run("ddm.remove && ddm.remove(\(uids))")
    }

    // MARK: Меню пузыря (§8, десктоп app.slint «Ответить … Удалить»)

    /// Пункты как у десктопа: «Ответить», «Переслать», «Копировать текст»,
    /// «Копировать адрес», «Показать… → Заголовки / Исходник / Текстовую
    /// версию», «Удалить письмо». На спам не отвечают и не пересылают (§5).
    func bubbleMenu(_ b: MessageBody) -> [MenuEntry] {
        var items: [MenuEntry] = []
        if let why = DeliveryText.of(b) {
            items += [.note(why, b.delivery == .failed ? "exclamationmark.triangle" : "clock"), .divider]
        }
        if conversation?.spam != true {
            if let reply = actions.reply { items.append(.action("Ответить", "arrowshape.turn.up.left") { reply(b) }) }
            if let forward = actions.forward { items.append(.action("Переслать", "arrowshape.turn.up.right") { forward(b) }) }
        }
        items.append(.action("Копировать текст", "doc.on.doc") { Clipboard.copy(PlainText.of(b)) })
        if !b.fromAddr.isEmpty {
            items.append(.action("Копировать адрес", "at") { Clipboard.copy(b.fromAddr) })
        }
        var show: [MenuEntry] = [
            .action("Заголовки", "list.bullet.rectangle") { [weak self] in self?.showSource(b, headersOnly: true) },
            .action("Исходник сообщения", "chevron.left.forwardslash.chevron.right") { [weak self] in self?.showSource(b, headersOnly: false) },
        ]
        let hasHTML = !(b.html ?? "").trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        let hasText = !(b.text ?? "").trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        let asText = textView.contains(b.uid)
        if hasHTML && !asText {
            show.append(.action("Исходник тела (HTML)", "curlybraces") { [weak self] in
                self?.source = SourceText(title: "Исходник тела — \(Self.subject(b))", text: b.html ?? "", full: b.html ?? "")
            })
            if hasText {
                show.append(.action("Текстовую версию", "text.alignleft") { [weak self] in self?.view(b.uid, asText: true) })
            }
        }
        if hasHTML && asText {
            show.append(.action("HTML-версию", "doc.richtext") { [weak self] in self?.view(b.uid, asText: false) })
        }
        items += [.divider, .submenu("Показать…", "eye", show), .divider]
        items.append(.action("Удалить письмо", "trash", destructive: true) { [weak self] in self?.confirmDelete = b })
        return items
    }

    static func subject(_ b: MessageBody) -> String { b.subject.isEmpty ? "(без темы)" : b.subject }

    /// Текстовая версия письма вместо HTML (и обратно) — на месте, в ленте.
    private func view(_ uid: Int64, asText: Bool) {
        if asText { textView.insert(uid) } else { textView.remove(uid) }
        web.run("ddm.view && ddm.view(\(uid), '\(asText ? "text" : "html")')")
    }

    /// «Заголовки» / «Исходник сообщения»: письмо целиком с сервера (IMAP —
    /// `BODY.PEEK[]`), заголовки — до первой пустой строки.
    private func showSource(_ b: MessageBody, headersOnly: Bool) {
        guard let p = current, let key = conversation?.accountKey else { return }
        Task {
            do {
                let raw = try await p.source(uid: b.uid)
                let text = MessageSource.text(headersOnly ? MessageSource.headers(raw) : raw)
                // Сотни килобайт base64 вложений текстом не нужны и тормозят:
                // показывается начало, копируется и отдаётся всё.
                let limit = 512 * 1024
                let shown = text.count > limit ? String(text.prefix(limit)) + "\n\n… (показано начало; «Скопировать» берёт всё)" : text
                source = SourceText(title: "\(headersOnly ? "Заголовки" : "Исходник") — \(Self.subject(b))", text: shown, full: text)
            } catch {
                if isAuthFailure(error) { onAuthDead(key) }
                notice = "Не удалось получить письмо: \(error.localizedDescription)"
            }
        }
    }

    /// «Удалить письмо» (подтверждено): из ленты сразу, из кэша и с сервера —
    /// источником учётки. Не вышло — беседа открывается заново такой, какой
    /// её видит кэш.
    func delete(_ b: MessageBody) {
        guard let c = conversation, let p = current else { return }
        let ref = c.messages.first { $0.uid == b.uid } ?? MessageRef(folder: b.folder, uid: b.uid, messageID: b.messageID)
        dropFromFeed([b.uid])
        Task {
            do {
                try await p.deleteMessages([ref])
                notice = "✓ Письмо удалено"
            } catch {
                if isAuthFailure(error) { onAuthDead(c.accountKey) }
                notice = "Не удалось удалить письмо: \(error.localizedDescription)"
                if conversation?.ref == c.ref { open(conversation) }
            }
        }
    }

    // MARK: Вложения

    func attachmentName(uid: Int64, index: Int) -> String {
        bodies[uid]?.attachments.first { $0.index == index }?.filename ?? "attachment-\(index)"
    }

    /// Меню чипа (iOS §5 «Вложения»): «Открыть», «Поделиться», «Сохранить в
    /// Файлы»; Мак — «Открыть программой по умолчанию» и «Сохранить в
    /// Загрузки» (как десктоп §4а), «Поделиться».
    func attachmentMenu(uid: Int64, index: Int, anchor: DocAnchor?) -> [MenuEntry] {
        var items: [MenuEntry] = [
            .action("Открыть", "eye") { [weak self] in self?.openAttachment(uid: uid, index: index) },
        ]
        #if os(macOS)
        items.append(.action("Открыть программой по умолчанию", "arrow.up.forward.app") { [weak self] in
            self?.saveToDownloads(uid: uid, index: index, open: true)
        })
        #endif
        items.append(.action("Поделиться", "square.and.arrow.up") { [weak self] in
            self?.shareAttachment(uid: uid, index: index, anchor: anchor)
        })
        #if os(macOS)
        items.append(.action("Сохранить в Загрузки", "arrow.down.circle") { [weak self] in
            self?.saveToDownloads(uid: uid, index: index, open: false)
        })
        #else
        items.append(.action("Сохранить в «Файлы»", "square.and.arrow.down") { [weak self] in
            self?.saveAttachment(uid: uid, index: index)
        })
        #endif
        return items
    }

    /// Скачать вложение во временный каталог (имя — безопасное).
    func download(uid: Int64, index: Int) async -> URL? {
        let name = safeFileName(attachmentName(uid: uid, index: index))
        do {
            guard let p = current else { return nil }
            let bin = try await p.attachment(uid: uid, index: index)
            // Учёток несколько — id писем разных учёток могут совпасть: каталог
            // по учётке, ключ стабильный между запусками (FNV-1a).
            let account = AttachmentFiles.key(conversation?.accountKey ?? "")
            let dir = AttachmentFiles.root.appending(path: "\(account)/\(uid)-\(index)")
            try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
            let file = dir.appending(path: name)
            try bin.data.write(to: file, options: .atomic)
            downloaded.append(dir)
            return file
        } catch {
            if isAuthFailure(error), let k = conversation?.accountKey { onAuthDead(k) }
            notice = "Не удалось скачать «\(name)»: \(error.localizedDescription)"
        }
        return nil
    }

    /// Открыта другая беседа: её просмотренные вложения больше не нужны.
    private func dropDownloads() {
        for dir in downloaded { try? FileManager.default.removeItem(at: dir) }
        downloaded.removeAll()
    }

    func openAttachment(uid: Int64, index: Int) {
        Task { if let url = await download(uid: uid, index: index) { preview = url } }
    }

    func saveAttachment(uid: Int64, index: Int) {
        Task { if let url = await download(uid: uid, index: index) { exporting = url } }
    }

    func shareAttachment(uid: Int64, index: Int, anchor: DocAnchor?) {
        Task { if let url = await download(uid: uid, index: index) { web.share(url, at: anchor) } }
    }

    #if os(macOS)
    /// Мак, как десктоп §4а: в «Загрузки» (занято имя — «имя 2.ext») и, если
    /// просили, открыть программой по умолчанию.
    func saveToDownloads(uid: Int64, index: Int, open: Bool) {
        Task {
            guard let tmp = await download(uid: uid, index: index) else { return }
            do {
                let dest = try AttachmentFiles.placeInDownloads(tmp)
                if open {
                    if !NSWorkspace.shared.open(dest) { notice = "Не удалось открыть вложение: \(dest.path)" }
                } else {
                    notice = "✓ Сохранено в Загрузки: \(dest.lastPathComponent)"
                }
            } catch {
                notice = "Не удалось сохранить вложение: \(error.localizedDescription)"
            }
        }
    }
    #endif
}

/// Временные файлы вложений (`tmp/attachments`): каталог по учётке со
/// стабильным ключом, чистка на запуске и при смене беседы.
enum AttachmentFiles {
    static var root: URL { URL.temporaryDirectory.appending(path: "attachments") }

    /// FNV-1a 64 в hex: в отличие от `hashValue`, не меняется между запусками.
    static func key(_ s: String) -> String {
        var h: UInt64 = 0xcbf29ce484222325
        for b in s.utf8 {
            h ^= UInt64(b)
            h = h &* 0x100000001b3
        }
        return String(h, radix: 16)
    }

    @MainActor private static var purged = false

    /// Запуск: остатки прошлого сеанса (просмотр, «Поделиться») — прочь.
    @MainActor static func purgeOnce() {
        guard !purged else { return }
        purged = true
        try? FileManager.default.removeItem(at: root)
    }

    #if os(macOS)
    /// Копия в «Загрузки» под свободным именем.
    static func placeInDownloads(_ file: URL) throws -> URL {
        let fm = FileManager.default
        let dir = try fm.url(for: .downloadsDirectory, in: .userDomainMask, appropriateFor: nil, create: true)
        let ext = file.pathExtension
        let base = file.deletingPathExtension().lastPathComponent
        var dest = dir.appending(path: file.lastPathComponent)
        var n = 2
        while fm.fileExists(atPath: dest.path) {
            dest = dir.appending(path: ext.isEmpty ? "\(base) \(n)" : "\(base) \(n).\(ext)")
            n += 1
        }
        try fm.copyItem(at: file, to: dest)
        return dest
    }
    #endif
}

extension ConversationModel: ConversationWebHost {
    func webReady() {}

    /// Процесс WebKit упал, документ пуст: беседу заново (окно, `shown` — с нуля).
    func webRestarted() {
        guard conversation != nil else { return }
        open(conversation)
    }

    func webRequestOlder() { older() }
    func webOpenLink(_ url: URL) { LinkOpener.open(url) }
    func webOpenAttachment(uid: Int64, index: Int) { openAttachment(uid: uid, index: index) }

    func webAttachmentMenu(uid: Int64, index: Int, anchor: DocAnchor?) {
        web.showMenu(attachmentName(uid: uid, index: index), attachmentMenu(uid: uid, index: index, anchor: anchor), at: anchor)
    }

    func webBubbleMenu(uid: Int64, anchor: DocAnchor?) {
        guard let b = bodies[uid] else { return }
        web.showMenu(Self.subject(b), bubbleMenu(b), at: anchor)
    }

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

    /// Отзыв («Настройки → Внешний контент»): дальше картинки этого
    /// отправителя — снова по нажатию.
    static func revoke(_ addrs: some Sequence<String>) {
        var s = all
        for a in addrs { s.remove(a.lowercased()) }
        UserDefaults.standard.set(Array(s).sorted(), forKey: key)
    }

    /// Выход из учётки: разрешения чужого человека не остаются.
    static func clear() { UserDefaults.standard.removeObject(forKey: key) }
}
