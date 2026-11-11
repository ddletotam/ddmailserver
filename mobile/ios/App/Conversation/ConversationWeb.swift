import DDMailSync
import Foundation
import os
import WebKit

let webLog = Logger(subsystem: "ru.letotam.ddmail", category: "web")

/// Что документ ленты просит у нативной части (мост `native(name, …args)`
/// в `conversation.js`).
@MainActor
protocol ConversationWebHost: AnyObject {
    func webReady()
    func webRequestOlder()
    func webOpenLink(_ url: URL)
    func webOpenAttachment(uid: Int64, index: Int)
    func webAttachmentMenu(uid: Int64, index: Int)
    func webBubbleMenu(uid: Int64)
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
    private var pending: [String] = []
    private let handler = DocSchemeHandler()

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

    /// Новая беседа: всё, что не успело уйти прежней, — уже не нужно.
    func reset() { pending.removeAll() }

    fileprivate func message(_ body: Any) {
        guard let dict = body as? [String: Any], let name = dict["name"] as? String else { return }
        let args = dict["args"] as? [Any] ?? []
        func int(_ i: Int) -> Int64? { (args.count > i ? args[i] as? NSNumber : nil)?.int64Value }
        guard let host else { return }
        switch name {
        case "ready":
            ready = true
            let queued = pending
            pending.removeAll()
            queued.forEach(run)
            host.webReady()
        case "requestOlder": host.webRequestOlder()
        case "openLink":
            if let s = args.first as? String, let u = URL(string: s) { host.webOpenLink(u) }
        case "openAttachment":
            if let uid = int(0), let i = int(1) { host.webOpenAttachment(uid: uid, index: Int(i)) }
        case "attachmentMenu":
            if let uid = int(0), let i = int(1) { host.webAttachmentMenu(uid: uid, index: Int(i)) }
        case "bubbleMenu":
            if let uid = int(0) { host.webBubbleMenu(uid: uid) }
        case "allowSender":
            if let uid = int(0) { host.webAllowSender(uid: uid) }
        case "allowSpamImages":
            if let uid = int(0) { host.webAllowSpamImages(uid: uid) }
        default:
            webLog.debug("unknown bridge call \(name, privacy: .public)")
        }
    }

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

    /// Процесс WebKit упал (память) — документ заново, текущую беседу заново.
    func webViewWebContentProcessDidTerminate(_ webView: WKWebView) {
        ready = false
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
    private static let remoteSession: URLSession = {
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
        do {
            let (data, response) = try await Self.remoteSession.data(for: req)
            guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode),
                  let type = http.value(forHTTPHeaderField: "Content-Type"), type.lowercased().hasPrefix("image/"),
                  data.count <= Self.maxRemote
            else { return deny(task) }
            respond(task, data: data, type: type)
        } catch {
            deny(task)
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
