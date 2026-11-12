import DDMailAPI
import Foundation

/// Коллекция DAV: календарь или адресная книга.
public struct DAVCollection: Equatable, Sendable {
    public enum Kind: Sendable { case calendar, addressBook }
    public var url: URL
    public var name: String
    public var color: String
    public var canWrite: Bool
}

/// HTTP-клиент CalDAV/CardDAV (порт `caldav_client.rs`/`carddav_client.rs`):
/// Basic паролем учётки, перенаправления — вручную (метод и авторизация
/// сохраняются только на https того же хоста), 401 — нужен вход, 412 —
/// ресурс изменился на сервере.
public struct DAVClient: Sendable {
    public let user: String
    let password: @Sendable () -> String?
    let session: URLSession

    public init(user: String, password: @escaping @Sendable () -> String?, session: URLSession = DAVClient.defaultSession) {
        self.user = user
        self.password = password
        self.session = session
    }

    public static let defaultSession: URLSession = {
        let c = URLSessionConfiguration.ephemeral
        c.timeoutIntervalForRequest = 20
        c.tlsMinimumSupportedProtocolVersion = .TLSv12
        c.httpCookieStorage = nil
        c.urlCache = nil
        return URLSession(configuration: c, delegate: NoRedirect(), delegateQueue: nil)
    }()

    /// Перенаправления не выполняются сами: PROPFIND превратился бы в GET, а
    /// пароль ушёл бы на чужой хост.
    final class NoRedirect: NSObject, URLSessionTaskDelegate, Sendable {
        func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
                        newRequest request: URLRequest) async -> URLRequest? { nil }
    }

    public struct Response: Sendable {
        public var status: Int
        public var body: Data
        public var etag: String?
        public var text: String { String(decoding: body, as: UTF8.self) }
    }

    public func request(_ method: String, _ url: URL, body: String? = nil, contentType: String = "application/xml; charset=utf-8",
                        headers: [String: String] = [:], redirects: Int = 3) async throws -> Response {
        guard url.scheme == "https" else { throw APIError.server(code: 0, message: "Адрес CalDAV/CardDAV должен быть https.") }
        guard let pw = password() else { throw APIError.unauthorized("нет пароля") }
        var r = URLRequest(url: url)
        r.httpMethod = method
        r.setValue("Basic " + Data("\(user):\(pw)".utf8).base64EncodedString(), forHTTPHeaderField: "Authorization")
        if let body {
            r.httpBody = Data(body.utf8)
            r.setValue(contentType, forHTTPHeaderField: "Content-Type")
        }
        for (k, v) in headers { r.setValue(v, forHTTPHeaderField: k) }
        let (data, resp): (Data, URLResponse)
        do {
            (data, resp) = try await session.data(for: r)
        } catch let e as URLError where e.code == .cancelled {
            throw CancellationError()
        } catch {
            throw APIError.network(error.localizedDescription)
        }
        guard let http = resp as? HTTPURLResponse else { throw APIError.decoding("не HTTP") }
        if [301, 302, 307, 308].contains(http.statusCode), redirects > 0,
           let loc = http.value(forHTTPHeaderField: "Location"), let next = URL(string: loc, relativeTo: url)?.absoluteURL {
            guard next.scheme == "https", next.host == url.host || method == "PROPFIND" && url.path.contains(".well-known") else {
                throw APIError.server(code: http.statusCode, message: "Сервер перенаправил на \(next.host ?? "?") — укажите адрес DAV вручную.")
            }
            return try await request(method, next, body: body, contentType: contentType, headers: headers, redirects: redirects - 1)
        }
        switch http.statusCode {
        case 200..<300:
            return Response(status: http.statusCode, body: data, etag: http.value(forHTTPHeaderField: "ETag"))
        case 401:
            throw APIError.unauthorized("DAV отверг пароль")
        case 412:
            throw APIError.server(code: 412, message: "Запись изменилась на сервере — откройте заново и повторите.")
        case 404 where method == "DELETE":
            return Response(status: 404, body: data, etag: nil)
        default:
            throw APIError.server(code: http.statusCode, message: "Сервер DAV ответил \(http.statusCode).")
        }
    }

    public func propfind(_ url: URL, depth: Int, props: String) async throws -> (url: URL, xml: XMLTree) {
        let body = #"<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:A="urn:ietf:params:xml:ns:carddav" xmlns:I="http://apple.com/ns/ical/"><D:prop>"# + props + "</D:prop></D:propfind>"
        let r = try await request("PROPFIND", url, body: body, headers: ["Depth": String(depth)])
        guard let x = XMLTree.parse(r.text) else { throw APIError.decoding("PROPFIND: не XML") }
        return (url, x)
    }

    public func report(_ url: URL, body: String) async throws -> XMLTree {
        let r = try await request("REPORT", url, body: body, headers: ["Depth": "1"])
        guard let x = XMLTree.parse(r.text) else { throw APIError.decoding("REPORT: не XML") }
        return x
    }

    // MARK: Обнаружение

    /// Все коллекции нужного вида (лучше десктопа: все календари home-set, а не
    /// первый): адрес уже коллекция — она; иначе `current-user-principal` →
    /// `calendar-home-set`/`addressbook-home-set` → коллекции дома.
    public func discover(_ start: URL, kind: DAVCollection.Kind) async throws -> [DAVCollection] {
        let homeProp = kind == .calendar ? "calendar-home-set" : "addressbook-home-set"
        let homePropXML = kind == .calendar ? "<C:calendar-home-set/>" : "<A:addressbook-home-set/>"
        let listProps = "<D:resourcetype/><D:displayname/><I:calendar-color/><D:current-user-privilege-set/><C:supported-calendar-component-set/>"
        let (root, rx) = try await propfind(start, depth: 0, props: "<D:resourcetype/><D:current-user-principal/>" + homePropXML + "<D:displayname/><I:calendar-color/><D:current-user-privilege-set/>")
        if let c = Self.collections(rx, base: root, kind: kind).first { return [c] }
        var home = Self.href(rx, inside: homeProp).flatMap { URL(string: $0, relativeTo: root)?.absoluteURL }
        if home == nil, let principal = Self.href(rx, inside: "current-user-principal").flatMap({ URL(string: $0, relativeTo: root)?.absoluteURL }) {
            let (_, px) = try await propfind(principal, depth: 0, props: homePropXML)
            home = Self.href(px, inside: homeProp).flatMap { URL(string: $0, relativeTo: principal)?.absoluteURL }
        }
        let (h, lx) = try await propfind(home ?? root, depth: 1, props: listProps)
        return Self.collections(lx, base: h, kind: kind)
    }

    static func href(_ x: XMLTree, inside tag: String) -> String? {
        x.descendants(tag).first?.descendants("href").first?.text.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    static func collections(_ x: XMLTree, base: URL, kind: DAVCollection.Kind) -> [DAVCollection] {
        x.descendants("response").compactMap { r in
            guard let href = r.first("href")?.text.trimmingCharacters(in: .whitespacesAndNewlines),
                  let url = URL(string: href, relativeTo: base)?.absoluteURL else { return nil }
            // Свойства — из propstat со статусом 200.
            let ok = r.descendants("propstat").filter { ($0.child("status") ?? "").contains("200") }
            let props = ok.flatMap { $0.descendants("prop") }
            let types = props.flatMap { $0.descendants("resourcetype") }.flatMap(\.children).map(\.name)
            guard types.contains(kind == .calendar ? "calendar" : "addressbook") else { return nil }
            if kind == .calendar {
                let comps = props.flatMap { $0.descendants("comp") }.compactMap { $0.attrs["name"]?.uppercased() }
                if !comps.isEmpty && !comps.contains("VEVENT") { return nil }
            }
            let name = props.compactMap { $0.child("displayname") }.first { !$0.isEmpty } ?? url.lastPathComponent
            let color = props.compactMap { $0.child("calendar-color") }.first ?? ""
            let privs = props.flatMap { $0.descendants("privilege") }.flatMap(\.children).map(\.name)
            let canWrite = privs.isEmpty || privs.contains { ["write", "write-content", "all", "bind"].contains($0) }
            return DAVCollection(url: url, name: name, color: color, canWrite: canWrite)
        }
    }

    /// Пробный вход: адрес отвечает на PROPFIND с этим паролем — DAV есть.
    public func probe(_ url: URL) async -> Bool {
        (try? await propfind(url, depth: 0, props: "<D:current-user-principal/>")) != nil
    }

    /// Адреса DAV по домену (§8: находятся автоматически при добавлении
    /// учётки): службы с известными адресами, затем `.well-known`.
    public static func candidates(email: String, imapHost: String, kind: DAVCollection.Kind) -> [URL] {
        let domain = email.split(separator: "@").last.map { String($0).lowercased() } ?? ""
        let known: [(String, String, String)] = [
            ("yandex", "https://caldav.yandex.ru/", "https://carddav.yandex.ru/"),
            ("mail.ru", "https://calendar.mail.ru/", "https://carddav.mail.ru/"),
            ("me.com", "https://caldav.icloud.com/", "https://contacts.icloud.com/"),
        ]
        var out: [String] = []
        for (marker, cal, card) in known where imapHost.contains(marker) {
            out.append(kind == .calendar ? cal : card)
        }
        let wk = kind == .calendar ? "caldav" : "carddav"
        var hosts = [domain, imapHost]
        if !domain.isEmpty { hosts.append("mail.\(domain)") }
        for h in hosts where !h.isEmpty { out.append("https://\(h)/.well-known/\(wk)") }
        var seen = Set<String>()
        return out.filter { seen.insert($0).inserted }.compactMap(URL.init(string:))
    }

    /// Найти адрес DAV учётки: кандидаты проверяются разом, побеждает первый
    /// успешный по порядку; весь поиск — не дольше `limit`.
    public func find(email: String, imapHost: String, kind: DAVCollection.Kind, limit: Duration = .seconds(10)) async -> URL? {
        let list = Self.candidates(email: email, imapHost: imapHost, kind: kind)
        guard !list.isEmpty else { return nil }
        return await withTaskGroup(of: (Int, Bool)?.self) { group in
            for (i, u) in list.enumerated() {
                group.addTask { (i, await self.probe(u)) }
            }
            group.addTask {
                try? await Task.sleep(for: limit)
                return nil
            }
            var results = [Bool?](repeating: nil, count: list.count)
            for await r in group {
                guard let (i, ok) = r else { break }
                results[i] = ok
                for (j, v) in results.enumerated() {
                    guard let v else { break }
                    if v {
                        group.cancelAll()
                        return list[j]
                    }
                }
            }
            group.cancelAll()
            return nil
        }
    }
}
