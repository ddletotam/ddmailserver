import DDMailSync
import Foundation
import os
#if os(iOS)
import UIKit
#else
import AppKit
#endif
import WebKit

let webLog = Logger(subsystem: "ru.letotam.ddmail", category: "web")

/// Что документ ленты просит у нативной части (мост `native(name, …args)`
/// в `conversation.js`).
@MainActor
protocol ConversationWebHost: AnyObject {
    func webReady()
    /// Документ загружен заново после падения процесса WebKit: лента пуста.
    func webRestarted()
    func webRequestOlder()
    func webOpenLink(_ url: URL)
    func webOpenAttachment(uid: Int64, index: Int)
    /// Меню чипа вложения и пузыря; `anchor` — где (у WKWebView он есть всегда).
    func webAttachmentMenu(uid: Int64, index: Int, anchor: DocAnchor?)
    func webBubbleMenu(uid: Int64, anchor: DocAnchor?)
    func webAllowSender(uid: Int64)
    func webAllowSpamImages(uid: Int64)
    /// Встроенная часть письма для `/cid/<uid>/<cid>`.
    func webPart(uid: Int64, cid: String) async throws -> (Data, String)
    var webGate: RemoteImageGate { get }
}

/// Один `WKWebView` на всё время жизни почты (контракт §8): создаётся заранее,
/// при открытии беседы меняется только документ (`ddm.set`).
///
/// Безопасность (§8, iOS):
/// - документ — со своей схемы `ddmail-doc://local/`, её обслуживает только
///   `DocSchemeHandler`: файлы документа, `/cid/…` с сервера, `/remote` через
///   прокси приложения по правилам спама; всё остальное — 403;
/// - любая другая сеть (`http(s)`, `file:` …) режется `WKContentRuleList` на
///   уровне движка — второй рубеж после чистки нормализатора и CSP документа;
/// - хранилище WebKit непостоянное: куки и кэш писем не переживают сеанс;
/// - переходы документа наружу не исполняются — ссылки открывает система.
@MainActor
final class ConversationWeb: NSObject {
    static let scheme = "ddmail-doc"
    static let documentURL = URL(string: "\(scheme)://local/conversation.html")!

    weak var host: ConversationWebHost?
    private(set) var view: WKWebView?
    private var ready = false
    /// Процесс WebKit упал, документ грузится заново: на его `ready` — беседу заново.
    private var restarting = false
    private var pending: [String] = []
    private let handler = DocSchemeHandler()
    #if os(iOS)
    /// Якорь меню пузыря и чипа (§8): невидимая кнопка над ними.
    private var menuAnchor: MenuAnchorButton?
    /// Меню выделения, поставленного серией тапов (§8): WebKit рисует маркеры
    /// выделения из скрипта, а меню «Скопировать» — нет.
    private var selectionMenu: UIEditMenuInteraction?
    /// Прямоугольник выделения в координатах веб-представления.
    private var selectionRect = CGRect.zero
    /// Открыта клавиатура (фокус в строке ввода): меню выделения при ней не
    /// показывается — сначала её убрать.
    private var keyboardShown = false
    private var keyboardObservers: [NSObjectProtocol] = []
    #endif

    /// Подготовить веб-представление: правила блокировки компилируются
    /// асинхронно, документ грузится сразу после них.
    func prepare() async {
        guard view == nil else { return }
        let config = WKWebViewConfiguration()
        config.websiteDataStore = .nonPersistent()
        config.setURLSchemeHandler(handler, forURLScheme: Self.scheme)
        config.userContentController.add(WeakMessageHandler(self), name: "ddm")
        if let rules = await Self.blockRules() {
            config.userContentController.add(rules)
        } else {
            // Без правил движка — без документа: CSP и чистка остаются, но
            // второй рубеж обязателен (§8), лента не откроется.
            webLog.error("content rules failed to compile")
            return
        }
        #if os(iOS)
        config.dataDetectorTypes = []
        #endif
        let v = WKWebView(frame: .zero, configuration: config)
        v.navigationDelegate = self
        #if os(iOS)
        v.scrollView.contentInsetAdjustmentBehavior = .always
        v.isOpaque = false
        v.backgroundColor = .clear
        #else
        v.setValue(false, forKey: "drawsBackground")
        #endif
        #if DEBUG
        v.isInspectable = true
        #endif
        #if os(iOS)
        let menu = UIEditMenuInteraction(delegate: self)
        v.addInteraction(menu)
        selectionMenu = menu
        let nc = NotificationCenter.default
        keyboardObservers = [
            nc.addObserver(forName: UIResponder.keyboardWillShowNotification, object: nil, queue: .main) { [weak self] _ in
                MainActor.assumeIsolated { self?.keyboard(true) }
            },
            nc.addObserver(forName: UIResponder.keyboardWillHideNotification, object: nil, queue: .main) { [weak self] _ in
                MainActor.assumeIsolated { self?.keyboard(false) }
            },
        ]
        #endif
        handler.owner = self
        view = v
        v.load(URLRequest(url: Self.documentURL))
    }

    /// Команда документу (`ddm.set(…)` и т. п.); до `ready` — в очередь.
    func run(_ js: String) {
        guard ready, let view else {
            pending.append(js)
            return
        }
        view.evaluateJavaScript(js) { _, error in
            if let error { webLog.error("js failed: \(error.localizedDescription, privacy: .public)") }
        }
    }

    /// Выражение с ответом-флагом (умеет ли документ что-то): документ не
    /// готов или выражение упало — `false`.
    func ask(_ js: String) async -> Bool {
        guard ready, let view else { return false }
        do {
            return try await view.evaluateJavaScript(js) as? Bool ?? false
        } catch {
            webLog.error("js failed: \(error.localizedDescription, privacy: .public)")
            return false
        }
    }

    /// Новая беседа: всё, что не успело уйти прежней, — уже не нужно.
    func reset() { pending.removeAll() }

    fileprivate func message(_ body: Any) {
        guard let dict = body as? [String: Any], let name = dict["name"] as? String else { return }
        let args = dict["args"] as? [Any] ?? []
        func int(_ i: Int) -> Int64? { (args.count > i ? args[i] as? NSNumber : nil)?.int64Value }
        func anchor(after n: Int) -> DocAnchor? { DocAnchor(args.dropFirst(n).compactMap { ($0 as? NSNumber)?.doubleValue }) }
        guard let host else { return }
        switch name {
        case "ready":
            ready = true
            let queued = pending
            pending.removeAll()
            queued.forEach(run)
            host.webReady()
            if restarting {
                restarting = false
                #if os(iOS)
                if keyboardShown { run("ddm.keyboard && ddm.keyboard(true)") }
                #endif
                host.webRestarted()
            }
        case "requestOlder": host.webRequestOlder()
        case "openLink":
            if let s = args.first as? String, let u = URL(string: s) { host.webOpenLink(u) }
        case "openAttachment":
            if let uid = int(0), let i = int(1) { host.webOpenAttachment(uid: uid, index: Int(i)) }
        case "attachmentMenu":
            if let uid = int(0), let i = int(1) { host.webAttachmentMenu(uid: uid, index: Int(i), anchor: anchor(after: 2)) }
        case "bubbleMenu":
            if let uid = int(0) { host.webBubbleMenu(uid: uid, anchor: anchor(after: 1)) }
        case "allowSender":
            if let uid = int(0) { host.webAllowSender(uid: uid) }
        case "allowSpamImages":
            if let uid = int(0) { host.webAllowSpamImages(uid: uid) }
        case "selectionMenu":
            #if os(iOS)
            let n = args.compactMap { ($0 as? NSNumber)?.doubleValue }
            if n.count == 4 { showSelectionMenu(CGRect(x: n[0], y: n[1], width: n[2], height: n[3])) }
            #endif
        default:
            webLog.debug("unknown bridge call \(name, privacy: .public)")
        }
    }

    /// Меню у пузыря/чипа (§8, §9). iPhone/iPad — системное контекстное меню у
    /// пузыря (`anchor` — его прямоугольник), Mac — `NSMenu` в точке правого клика.
    /// Показывается после возврата из обработчика моста.
    func showMenu(_ title: String, _ entries: [MenuEntry], at anchor: DocAnchor?) {
        guard !entries.isEmpty else { return }
        Task { @MainActor [weak self] in
            #if os(iOS)
            self?.presentMenu(MenuEntry.uiMenu(title, entries), at: anchor)
            #else
            self?.presentMenu(MenuEntry.nsMenu(entries))
            #endif
        }
    }

    /// Системное «Поделиться» файлом — у того места, где просили меню.
    func share(_ url: URL, at anchor: DocAnchor?) {
        guard let view else { return }
        #if os(iOS)
        let sheet = UIActivityViewController(activityItems: [url], applicationActivities: nil)
        if let pop = sheet.popoverPresentationController {
            pop.sourceView = view
            pop.sourceRect = anchor.map { menuRect(for: $0, in: view) } ?? CGRect(x: view.bounds.midX, y: view.bounds.midY, width: 1, height: 1)
        }
        var top = view.window?.rootViewController
        while let next = top?.presentedViewController { top = next }
        top?.present(sheet, animated: true)
        #else
        let p = lastMenuPoint ?? CGPoint(x: view.bounds.midX, y: view.bounds.midY)
        NSSharingServicePicker(items: [url]).show(relativeTo: CGRect(origin: p, size: CGSize(width: 1, height: 1)), of: view, preferredEdge: .minY)
        #endif
    }

    #if os(macOS)
    /// Где открывали последнее меню (для «Поделиться» из него).
    private var lastMenuPoint: CGPoint?

    /// Меню Мака — в точке правого клика (мышь ещё там: меню просят сразу).
    private func presentMenu(_ menu: NSMenu) {
        guard let view, let window = view.window else { return }
        let p = view.convert(window.mouseLocationOutsideOfEventStream, from: nil)
        lastMenuPoint = p
        menu.popUp(positioning: nil, at: p, in: view)
    }
    #endif

    /// Правила движка: заблокировать всё, кроме своей схемы и `data:`.
    private static func blockRules() async -> WKContentRuleList? {
        let json = """
        [
          {"trigger": {"url-filter": ".*"}, "action": {"type": "block"}},
          {"trigger": {"url-filter": "^\(scheme):"}, "action": {"type": "ignore-previous-rules"}},
          {"trigger": {"url-filter": "^data:"}, "action": {"type": "ignore-previous-rules"}}
        ]
        """
        return await withCheckedContinuation { cont in
            WKContentRuleListStore.default().compileContentRuleList(forIdentifier: "ddmail-doc-v1", encodedContentRuleList: json) { list, error in
                if let error { webLog.error("rules: \(error.localizedDescription, privacy: .public)") }
                cont.resume(returning: list)
            }
        }
    }
}

#if os(iOS)
extension ConversationWeb: @MainActor UIEditMenuInteractionDelegate {
    /// Клавиатура — себе и документу: когда её убирает тап по письму,
    /// WKWebView второй тап серии забирает себе и выделяет слово сам, без
    /// меню, — документ замечает такое выделение и просит меню (`ddm.keyboard`).
    private func keyboard(_ shown: Bool) {
        keyboardShown = shown
        run("ddm.keyboard && ddm.keyboard(\(shown))")
    }

    /// Меню над выделением. `rect` — в координатах документа (CSS px от
    /// начала страницы): в содержимом прокрутки это они же, умноженные на
    /// масштаб щипка.
    fileprivate func showSelectionMenu(_ rect: CGRect) {
        guard let view, selectionMenu != nil else { return }
        guard keyboardShown else { return presentSelectionMenu(rect) }
        // Клавиатура ещё открыта — при ней меню не показывается. Убрать её и
        // показать меню, когда раскладка устоится, — над выделением, каким
        // оно стало.
        view.window?.endEditing(true)
        Task { [weak self] in
            try? await Task.sleep(for: .milliseconds(600))
            guard let web = self?.view else { return }
            let r = try? await web.evaluateJavaScript("ddm.selectionRect()")
            let n = (r as? [NSNumber])?.map(\.doubleValue) ?? []
            self?.presentSelectionMenu(n.count == 4 ? CGRect(x: n[0], y: n[1], width: n[2], height: n[3]) : rect)
        }
    }

    private func presentSelectionMenu(_ rect: CGRect) {
        guard let view, let selectionMenu else { return }
        let z = view.scrollView.zoomScale
        let content = CGRect(x: rect.minX * z, y: rect.minY * z, width: rect.width * z, height: rect.height * z)
        selectionRect = view.scrollView.convert(content, to: view)
        // После ухода клавиатуры первым ответчиком может не остаться никто —
        // тогда им становится содержимое веба (меню показывает ответчик).
        if !view.isFirstResponder { view.becomeFirstResponder() }
        let config = UIEditMenuConfiguration(identifier: nil, sourcePoint: CGPoint(x: selectionRect.midX, y: selectionRect.minY))
        config.preferredArrowDirection = .automatic
        selectionMenu.presentEditMenu(with: config)
    }

    /// Своё «Скопировать»: системные пункты WKWebView для выделения из
    /// скрипта приходят выключенными (меню из них не показывается — проверено
    /// на симуляторе iOS 26). Текст выделения отдаёт документ
    /// (`ddm.selectedText()`) в момент нажатия — маркеры, сдвинутые после
    /// показа меню, тоже учитываются.
    func editMenuInteraction(_ interaction: UIEditMenuInteraction, menuFor configuration: UIEditMenuConfiguration,
                             suggestedActions: [UIMenuElement]) -> UIMenu? {
        let copy = UIAction(title: "Скопировать", image: UIImage(systemName: "doc.on.doc")) { [weak self] _ in
            self?.view?.evaluateJavaScript("ddm.selectedText()") { r, _ in
                if let s = r as? String, !s.isEmpty { UIPasteboard.general.string = s }
            }
        }
        return UIMenu(children: [copy])
    }

    func editMenuInteraction(_ interaction: UIEditMenuInteraction, targetRectFor configuration: UIEditMenuConfiguration) -> CGRect {
        selectionRect
    }

    /// Прямоугольник документа (CSS px) → координаты веб-представления, как у
    /// меню выделения: содержимое прокрутки — это CSS px × масштаб щипка.
    fileprivate func viewRect(_ r: CGRect, in view: WKWebView) -> CGRect {
        let z = view.scrollView.zoomScale
        return view.scrollView.convert(CGRect(x: r.minX * z, y: r.minY * z, width: r.width * z, height: r.height * z), to: view)
    }

    /// Куда привязать меню: видимая часть пузыря. Пузырь выше половины
    /// видимого (длинное письмо) — квадрат у пальца, иначе меню встало бы
    /// у края экрана, далеко от места нажатия.
    fileprivate func menuRect(for anchor: DocAnchor, in view: WKWebView) -> CGRect {
        let visible = view.bounds.inset(by: view.safeAreaInsets)
        let bubble = viewRect(anchor.rect, in: view).intersection(visible)
        let p = viewRect(CGRect(origin: anchor.point, size: .zero), in: view).origin
        if bubble.isNull || bubble.isEmpty || bubble.height > visible.height / 2 {
            return CGRect(x: p.x - 22, y: p.y - 22, width: 44, height: 44).intersection(visible.insetBy(dx: -22, dy: -22))
        }
        return bubble
    }

    /// Системное контекстное меню у пузыря: невидимая кнопка ложится на его
    /// рамку и показывает меню (`performPrimaryAction`); меню закрылось —
    /// кнопка прячется и касаний не перехватывает.
    fileprivate func presentMenu(_ menu: UIMenu, at anchor: DocAnchor?) {
        guard let view else { return }
        // Меню выделения и меню письма вместе не показываются.
        selectionMenu?.dismissMenu()
        let button = menuAnchor ?? {
            let b = MenuAnchorButton(type: .custom)
            b.showsMenuAsPrimaryAction = true
            b.backgroundColor = .clear
            b.isAccessibilityElement = false
            b.isHidden = true
            view.addSubview(b)
            menuAnchor = b
            return b
        }()
        button.frame = anchor.map { menuRect(for: $0, in: view) }
            ?? CGRect(x: view.bounds.midX - 22, y: view.bounds.midY - 22, width: 44, height: 44)
        button.menu = menu
        button.isHidden = false
        view.bringSubviewToFront(button)
        button.onEnd = { [weak button] in
            button?.isHidden = true
            button?.menu = nil
        }
        button.performPrimaryAction()
    }
}
#endif

extension ConversationWeb: WKNavigationDelegate {
    /// Документ грузится один раз; любая попытка уйти с него — ссылка наружу.
    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction) async -> WKNavigationActionPolicy {
        guard let url = action.request.url else { return .cancel }
        if url.scheme == Self.scheme && action.targetFrame?.isMainFrame == true && url.path == Self.documentURL.path {
            return .allow
        }
        if action.navigationType == .linkActivated, ["http", "https", "mailto", "tel"].contains(url.scheme?.lowercased() ?? "") {
            host?.webOpenLink(url)
        }
        return .cancel
    }

    /// Процесс WebKit упал (память, iOS выгружает в фоне) — документ заново,
    /// а на его `ready` хозяин открывает текущую беседу заново (`webRestarted`):
    /// новый документ пуст, и прежние письма в него иначе не попадут.
    func webViewWebContentProcessDidTerminate(_ webView: WKWebView) {
        webLog.error("web content process terminated, reloading the feed")
        ready = false
        restarting = true
        // Команды, ушедшие в мёртвый документ или ждущие старого, не нужны:
        // беседу пришлют целиком.
        pending.removeAll()
        webView.load(URLRequest(url: Self.documentURL))
    }
}

/// `WKUserContentController` держит обработчик сильно — прослойка, чтобы не
/// было цикла «веб-представление ↔ модель».
private final class WeakMessageHandler: NSObject, WKScriptMessageHandler {
    weak var target: ConversationWeb?

    init(_ target: ConversationWeb) { self.target = target }

    func userContentController(_ controller: WKUserContentController, didReceive message: WKScriptMessage) {
        let body = message.body
        MainActor.assumeIsolated { target?.message(body) }
    }
}

/// Обработчик схемы `ddmail-doc://local/…` (§8): документ, части писем,
/// внешние картинки через прокси. Всё прочее — 403.
@MainActor
final class DocSchemeHandler: NSObject, WKURLSchemeHandler {
    weak var owner: ConversationWeb?
    private var stopped = Set<ObjectIdentifier>()
    private static let maxRemote = 10 * 1024 * 1024

    /// Каталог документа в ресурсах приложения (копия `mobile/shared/conversation-doc`).
    private static let docDir = Bundle.main.url(forResource: "conversation-doc", withExtension: nil)

    /// Прокси внешних картинок: без кук, кэша и наших заголовков.
    nonisolated private static let remoteSession: URLSession = {
        let c = URLSessionConfiguration.ephemeral
        c.httpCookieStorage = nil
        c.httpShouldSetCookies = false
        c.urlCache = nil
        c.timeoutIntervalForRequest = 20
        return URLSession(configuration: c)
    }()

    func webView(_ webView: WKWebView, start task: any WKURLSchemeTask) {
        let id = ObjectIdentifier(task)
        stopped.remove(id)
        guard let url = task.request.url, url.host() == "local" else { return deny(task) }
        let path = url.path()
        switch path {
        case "/conversation.html", "/conversation.css", "/bubble.css", "/conversation.js":
            serveFile(String(path.dropFirst()), task)
        case _ where path.hasPrefix("/cid/"):
            let parts = path.split(separator: "/", maxSplits: 2).map(String.init)
            guard parts.count == 3, let uid = Int64(parts[1]), let cid = parts[2].removingPercentEncoding,
                  let host = owner?.host
            else { return deny(task) }
            Task {
                do {
                    let (data, type) = try await host.webPart(uid: uid, cid: cid)
                    respond(task, data: data, type: type)
                } catch {
                    deny(task)
                }
            }
        case "/remote":
            let q = URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems ?? []
            let uid = q.first { $0.name == "uid" }?.value.flatMap { Int64($0) }
            guard let gate = owner?.host?.webGate,
                  gate.permits(uid: uid, headers: task.request.allHTTPHeaderFields ?? [:]),
                  let raw = q.first(where: { $0.name == "u" })?.value, let target = URL(string: raw),
                  ["http", "https"].contains(target.scheme?.lowercased() ?? "")
            else { return deny(task) }
            Task { await proxy(target, task) }
        default:
            deny(task)
        }
    }

    func webView(_ webView: WKWebView, stop task: any WKURLSchemeTask) {
        stopped.insert(ObjectIdentifier(task))
    }

    private func serveFile(_ name: String, _ task: any WKURLSchemeTask) {
        guard let dir = Self.docDir, let data = try? Data(contentsOf: dir.appending(path: name)) else { return deny(task) }
        let type = switch (name as NSString).pathExtension {
        case "html": "text/html"
        case "css": "text/css"
        default: "text/javascript"
        }
        respond(task, data: data, type: type + "; charset=utf-8")
    }

    /// Внешняя картинка: только `image/*` и не больше 10 МБ (как Android).
    private func proxy(_ url: URL, _ task: any WKURLSchemeTask) async {
        var req = URLRequest(url: url)
        req.setValue("image/*", forHTTPHeaderField: "Accept")
        guard let (data, type) = await Self.fetchImage(req, limit: Self.maxRemote) else { return deny(task) }
        respond(task, data: data, type: type)
    }

    /// Загрузка с лимитом: ответ не `2xx`, не картинка или `Content-Length`
    /// больше лимита — отказ до чтения тела; тело длиннее заявленного —
    /// обрыв на лимите, а не после загрузки всего в память. Читается вне
    /// главного потока.
    @concurrent
    nonisolated static func fetchImage(_ req: URLRequest, limit: Int) async -> (Data, String)? {
        do {
            let (bytes, response) = try await remoteSession.bytes(for: req)
            guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode),
                  let type = http.value(forHTTPHeaderField: "Content-Type"), type.lowercased().hasPrefix("image/"),
                  http.expectedContentLength <= Int64(limit)
            else {
                bytes.task.cancel()
                return nil
            }
            var data = Data()
            if http.expectedContentLength > 0 { data.reserveCapacity(Int(http.expectedContentLength)) }
            var chunk = [UInt8]()
            chunk.reserveCapacity(64 * 1024)
            for try await b in bytes {
                chunk.append(b)
                guard chunk.count == 64 * 1024 else { continue }
                data.append(contentsOf: chunk)
                chunk.removeAll(keepingCapacity: true)
                if data.count > limit {
                    bytes.task.cancel()
                    return nil
                }
            }
            data.append(contentsOf: chunk)
            return data.count <= limit ? (data, type) : nil
        } catch {
            return nil
        }
    }

    private func respond(_ task: any WKURLSchemeTask, data: Data, type: String, status: Int = 200) {
        guard !stopped.contains(ObjectIdentifier(task)), let url = task.request.url else { return }
        let headers = ["Content-Type": type, "Content-Length": String(data.count), "Cache-Control": "no-store"]
        guard let r = HTTPURLResponse(url: url, statusCode: status, httpVersion: "HTTP/1.1", headerFields: headers) else { return }
        task.didReceive(r)
        task.didReceive(data)
        task.didFinish()
    }

    private func deny(_ task: any WKURLSchemeTask) {
        respond(task, data: Data(), type: "text/plain", status: 403)
    }
}
