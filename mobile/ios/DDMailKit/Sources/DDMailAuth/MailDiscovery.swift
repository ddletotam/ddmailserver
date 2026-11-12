import DDMailAPI
import DDMailIMAP
import DDMailNet
import DDMailSMTP
import Foundation
import Synchronization

/// Сервер: хост и порт (шифрование — по порту, как у десктопа).
public struct Endpoint: Equatable, Hashable, Sendable {
    public var host: String
    public var port: Int

    public init(_ host: String, _ port: Int) {
        self.host = host
        self.port = port
    }
}

/// Что нашлось. Всё пусто — не нашлось ничего, пусть вводит руками.
public struct DiscoveryResult: Equatable, Sendable {
    /// `https://<хост>` сервера ddmail — остальное тогда не ищется.
    public var ddmailURL: String?
    public var imap: Endpoint?
    public var smtp: Endpoint?
    /// Исходящий объявил AUTH. Нет — внутренний релей без авторизации.
    public var smtpAuth = true
    /// Логин из автонастройки, если он не email (например, часть до @).
    public var login: String?
    /// Название службы (автонастройка или таблица).
    public var provider: String?

    public init() {}
}

/// Службы, которые клиент знает сам (порт `mail_presets` десктопа): их серверы
/// не выводятся из домена (`imap.mail.ru` обслуживает и `bk.ru`, и `inbox.ru`).
/// Outlook.com нет намеренно: Microsoft не пускает IMAP по паролю.
public struct MailPreset: Equatable, Sendable {
    public var imapHost: String
    public var smtpHost: String
    public var smtpPort: Int
    public var name: String
    /// Что сказать про пароль: многие службы пускают внешние программы только
    /// по паролю приложения, а обычный отвергают как неверный.
    public var passwordHint: String

    static let table: [(domains: [String], preset: MailPreset)] = [
        (["mail.ru", "inbox.ru", "list.ru", "bk.ru", "internet.ru", "xmail.ru"],
         MailPreset(imapHost: "imap.mail.ru", smtpHost: "smtp.mail.ru", smtpPort: 465, name: "Mail.ru",
                    passwordHint: "Mail.ru пускает внешние программы только по паролю приложения: Настройки → Безопасность → Пароли для внешних приложений.")),
        (["yandex.ru", "ya.ru", "yandex.com", "yandex.by", "yandex.kz", "narod.ru"],
         MailPreset(imapHost: "imap.yandex.ru", smtpHost: "smtp.yandex.ru", smtpPort: 465, name: "Яндекс Почты",
                    passwordHint: "Нужен пароль приложения (id.yandex.ru → Безопасность → Пароли приложений) и включённый IMAP в настройках почты.")),
        (["gmail.com", "googlemail.com"],
         MailPreset(imapHost: "imap.gmail.com", smtpHost: "smtp.gmail.com", smtpPort: 465, name: "Gmail",
                    passwordHint: "Gmail принимает только пароль приложения — он появляется после включения двухэтапной аутентификации в аккаунте Google.")),
        (["gmx.com"],
         MailPreset(imapHost: "imap.gmx.com", smtpHost: "mail.gmx.com", smtpPort: 465, name: "GMX",
                    passwordHint: "Доступ по IMAP в GMX выключен по умолчанию: включите его в настройках ящика (POP3 & IMAP).")),
        (["gmx.net", "gmx.de", "gmx.at", "gmx.ch"],
         MailPreset(imapHost: "imap.gmx.net", smtpHost: "mail.gmx.net", smtpPort: 465, name: "GMX",
                    passwordHint: "Доступ по IMAP в GMX выключен по умолчанию: включите его в настройках ящика (POP3 & IMAP).")),
        (["rambler.ru", "lenta.ru", "autorambler.ru", "myrambler.ru", "ro.ru"],
         MailPreset(imapHost: "imap.rambler.ru", smtpHost: "smtp.rambler.ru", smtpPort: 465, name: "Рамблер Почты",
                    passwordHint: "Включите доступ почтовым программам в настройках Рамблер Почты.")),
        // iCloud: SMTP только STARTTLS на 587.
        (["icloud.com", "me.com", "mac.com"],
         MailPreset(imapHost: "imap.mail.me.com", smtpHost: "smtp.mail.me.com", smtpPort: 587, name: "iCloud",
                    passwordHint: "iCloud пускает только по паролю приложения: account.apple.com → Вход и безопасность → Пароли приложений.")),
    ]

    /// Служба из таблицы по адресу; `nil` — не из таблицы.
    public static func known(_ email: String) -> MailPreset? {
        guard let domain = email.trimmingCharacters(in: .whitespaces).split(separator: "@").last?.lowercased() else { return nil }
        return table.first { $0.domains.contains(domain) }?.preset
    }

    /// Догадка для ручной формы: служба из таблицы или `imap.<домен>`/`smtp.<домен>`.
    public static func guess(_ email: String) -> MailPreset? {
        guard let at = email.trimmingCharacters(in: .whitespaces).lastIndex(of: "@") else { return nil }
        let domain = email.trimmingCharacters(in: .whitespaces)[email.trimmingCharacters(in: .whitespaces).index(after: at)...].lowercased()
        guard let dot = domain.lastIndex(of: "."), dot != domain.startIndex, domain.distance(from: dot, to: domain.endIndex) > 2 else { return nil }
        if let p = known(email) { return p }
        return MailPreset(imapHost: "imap.\(domain)", smtpHost: "smtp.\(domain)", smtpPort: 465, name: "", passwordHint: "")
    }
}

/// Прогресс поиска: сделано из верхней границы числа проверок.
public struct MailDiscoveryProgress: Equatable, Sendable {
    public let done: Int
    public let total: Int
    public let stage: String
}

/// Автонастройка по адресу почты — порт `desktop/core/src/discover.rs`
/// (контракт внешних учёток §2). Фазы; внутри фазы проверки идут
/// параллельно, а победитель — первый успешный **по порядку доверия**:
///
/// 1. MX домена, ddmail на `mail.<домен>` и `<домен>`, автонастройка
///    Thunderbird (домен, его `.well-known`, ISPDB);
/// 2. ddmail на MX-хостах — нашёлся ddmail, дальше не ищем;
/// 3. IMAP: 993 (TLS) и 143 (STARTTLS) на кандидатах;
/// 4. SMTP: 465 (TLS), 587 (STARTTLS), затем 25 (STARTTLS, не на MX).
///
/// Пароль не нужен и никуда не уходит: проверяется только, что на порту
/// живой сервер с рабочим TLS. Открытый текст не принимается. Отмена задачи
/// проверяется между фазами.
public struct MailDiscovery: Sendable {
    public static let maxMX = 2
    public static let maxIMAP = 16
    public static let maxSMTP = 27
    public static let autoconfigURLs = 3
    public static let total = 1 + 2 + autoconfigURLs + maxMX + maxIMAP + maxSMTP

    public var ddmail: @Sendable (String) async -> Bool
    public var mx: @Sendable (String) async -> [String]
    /// GET по https; `nil` — нет ответа или не 2xx.
    public var fetch: @Sendable (URL) async -> String?
    public var probeIMAP: @Sendable (Endpoint) async -> Bool
    /// `nil` — не SMTP с TLS; иначе — объявлен ли AUTH.
    public var probeSMTP: @Sendable (Endpoint) async -> Bool?

    public init(
        ddmail: @escaping @Sendable (String) async -> Bool, mx: @escaping @Sendable (String) async -> [String],
        fetch: @escaping @Sendable (URL) async -> String?, probeIMAP: @escaping @Sendable (Endpoint) async -> Bool,
        probeSMTP: @escaping @Sendable (Endpoint) async -> Bool?
    ) {
        self.ddmail = ddmail; self.mx = mx; self.fetch = fetch; self.probeIMAP = probeIMAP; self.probeSMTP = probeSMTP
    }

    /// Настоящие проверки: https системной сетью, MX системным резолвером,
    /// IMAP/SMTP — клиентами `DDMailIMAP`/`DDMailSMTP` (TCP 3 с, ответ 8 с).
    public static func live(transport: HTTPTransport) -> MailDiscovery {
        MailDiscovery(
            ddmail: { await DdmailAPI.isDdmail(host: $0, transport: transport) },
            mx: { await DnsMX.lookup($0) },
            fetch: { url in
                var r = URLRequest(url: url)
                r.timeoutInterval = 8
                guard let (data, resp) = try? await transport.send(r), (200..<300).contains(resp.statusCode) else { return nil }
                return String(data: data, encoding: .utf8)
            },
            probeIMAP: { e in
                await withTimeout(.seconds(8)) {
                    let c = try await IMAPClient.connect(MailEndpoint(host: e.host, port: e.port, security: .imap(port: e.port)),
                                                         connector: SystemMailConnector(), timeout: .seconds(3))
                    await c.logout()
                    return true
                } ?? false
            },
            probeSMTP: { e in
                await withTimeout(.seconds(8)) {
                    let c = try await SMTPClient.connect(MailEndpoint(host: e.host, port: e.port, security: .smtp(port: e.port)),
                                                         credentials: nil, connector: SystemMailConnector(), timeout: .seconds(3))
                    let auth = await c.advertisesAuth
                    await c.quit()
                    return auth
                }
            }
        )
    }

    /// Найти серверы для адреса.
    public func find(email: String, onProgress: @escaping @Sendable (MailDiscoveryProgress) -> Void) async -> DiscoveryResult {
        let meter = Meter(onProgress)
        var found = DiscoveryResult()
        let address = email.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let domain = emailDomain(address) else {
            meter.finish()
            return found
        }
        let hint = MailPreset.known(address)

        // ── 1. MX, ddmail на домене, автонастройка ──
        meter.stage("Ищем сервер для \(domain)…")
        let direct = ["mail.\(domain)", domain]
        let mxTask = Task { [mx] in
            let hosts = Array(await mx(domain).prefix(Self.maxMX))
            meter.tick()
            return hosts
        }
        let autoTask = Task { await autoconfig(email: address, domain: domain, meter: meter) }
        if let hit = await firstInOrder(direct.map(ddmailCheck), meter) {
            mxTask.cancel()
            autoTask.cancel()
            found.ddmailURL = "https://\(direct[hit.0])"
            meter.finish()
            return found
        }
        let mxHosts = await mxTask.value
        let auto = await autoTask.value
        if Task.isCancelled { return found }

        // ── 2. ddmail на MX ──
        let mxNew = mxHosts.filter { !direct.contains($0) }
        meter.tick(Self.maxMX - mxNew.count)
        if !mxNew.isEmpty {
            meter.stage("Проверяем почтовые серверы домена…")
            if let hit = await firstInOrder(mxNew.map(ddmailCheck), meter) {
                found.ddmailURL = "https://\(mxNew[hit.0])"
                meter.finish()
                return found
            }
        }
        if Task.isCancelled { return found }
        found.login = auto?.login
        found.provider = auto?.name ?? hint?.name

        // ── 3. IMAP ──
        meter.stage("Ищем входящий сервер (IMAP)…")
        let imap = Self.imapCandidates(domain: domain, hint: hint, auto: auto, mx: mxHosts)
        meter.tick(Self.maxIMAP - imap.count)
        let imapChecks: [@Sendable () async -> Bool?] = imap.map { e in { [probeIMAP] in await probeIMAP(e) ? true : nil } }
        if let hit = await firstInOrder(imapChecks, meter) {
            found.imap = imap[hit.0]
        }
        if Task.isCancelled { return found }

        // ── 4. SMTP ──
        meter.stage("Ищем исходящий сервер (SMTP)…")
        let smtp = Self.smtpCandidates(domain: domain, hint: hint, auto: auto, mx: mxHosts, imap: found.imap)
        meter.tick(Self.maxSMTP - smtp.count)
        let smtpChecks: [@Sendable () async -> Bool?] = smtp.map { e in { [probeSMTP] in await probeSMTP(e) } }
        if let hit = await firstInOrder(smtpChecks, meter) {
            found.smtp = smtp[hit.0]
            found.smtpAuth = hit.1
        }
        meter.finish()
        return found
    }

    private func ddmailCheck(_ host: String) -> @Sendable () async -> Bool? {
        { [ddmail] in await ddmail(host) ? true : nil }
    }

    // MARK: Кандидаты

    static func push(_ v: inout [Endpoint], _ e: Endpoint) {
        if !e.host.isEmpty && !v.contains(e) { v.append(e) }
    }

    /// Служба по MX: домен на Яндекс 360 или VK WorkSpace ходит на их серверы.
    static func mxProvider(_ mx: String) -> (imap: String, smtp: String)? {
        let m = mx.lowercased()
        if m.hasSuffix(".yandex.net") || m.hasSuffix(".yandex.ru") { return ("imap.yandex.ru", "smtp.yandex.ru") }
        if m.hasSuffix(".mail.ru") { return ("imap.mail.ru", "smtp.mail.ru") }
        if m.hasSuffix(".google.com") || m.hasSuffix(".googlemail.com") { return ("imap.gmail.com", "smtp.gmail.com") }
        return nil
    }

    /// По убыванию доверия: автонастройка, таблица, служба по MX, догадки по домену, MX.
    static func imapCandidates(domain: String, hint: MailPreset?, auto: Autoconfig?, mx: [String]) -> [Endpoint] {
        var v: [Endpoint] = []
        if let e = auto?.imap { push(&v, e) }
        var hosts: [String] = []
        if let h = hint { hosts.append(h.imapHost) }
        hosts += mx.compactMap { mxProvider($0)?.imap }
        hosts += ["imap.\(domain)", "mail.\(domain)", domain]
        hosts += mx
        for h in hosts {
            push(&v, Endpoint(h, 993))
            push(&v, Endpoint(h, 143))
        }
        return Array(v.prefix(maxIMAP))
    }

    /// 465 и 587 на всех хостах раньше любого 25; 25 на MX не пробуем (там
    /// приём почты, чужие письма наружу он не повезёт).
    static func smtpCandidates(domain: String, hint: MailPreset?, auto: Autoconfig?, mx: [String], imap: Endpoint?) -> [Endpoint] {
        var v: [Endpoint] = []
        if let e = auto?.smtp { push(&v, e) }
        if let h = hint { push(&v, Endpoint(h.smtpHost, h.smtpPort)) }
        var hosts: [String] = mx.compactMap { mxProvider($0)?.smtp }
        hosts.append("smtp.\(domain)")
        if let i = imap { hosts.append(i.host) }
        hosts += ["mail.\(domain)", domain]
        hosts += mx
        var uniq: [String] = []
        for h in hosts where !uniq.contains(h) { uniq.append(h) }
        for h in uniq {
            push(&v, Endpoint(h, 465))
            push(&v, Endpoint(h, 587))
        }
        for h in uniq where !mx.contains(h) { push(&v, Endpoint(h, 25)) }
        return Array(v.prefix(maxSMTP))
    }

    // MARK: Автонастройка

    struct Autoconfig: Equatable, Sendable {
        var imap: Endpoint?
        var smtp: Endpoint?
        var login: String?
        var name: String?
    }

    /// Только https: по http подменённый ответ увёл бы пароль на чужой хост.
    func autoconfig(email: String, domain: String, meter: Meter) async -> Autoconfig? {
        let enc = email.addingPercentEncoding(withAllowedCharacters: .alphanumerics) ?? email
        let urls = [
            "https://autoconfig.\(domain)/mail/config-v1.1.xml?emailaddress=\(enc)",
            "https://\(domain)/.well-known/autoconfig/mail/config-v1.1.xml",
            "https://autoconfig.thunderbird.net/v1.1/\(domain)",
        ].compactMap(URL.init(string:))
        let checks: [@Sendable () async -> Autoconfig?] = urls.map { u in { [fetch] in
            guard let xml = await fetch(u), let a = Self.parseAutoconfig(xml, email: email), a.imap != nil || a.smtp != nil else { return nil }
            return a
        } }
        let hit = await firstInOrder(checks, meter)
        return hit?.1
    }

    static func expand(_ tpl: String, _ email: String) -> String {
        let parts = email.split(separator: "@", maxSplits: 1, omittingEmptySubsequences: false)
        let local = parts.first.map(String.init) ?? email
        let dom = parts.count > 1 ? String(parts[1]) : ""
        return tpl.replacingOccurrences(of: "%EMAILADDRESS%", with: email)
            .replacingOccurrences(of: "%EMAILLOCALPART%", with: local)
            .replacingOccurrences(of: "%EMAILDOMAIN%", with: dom)
    }

    /// Разбор `config-v1.1.xml`. Сервер годится, если его шифрование совпадает
    /// с тем, что подключение выберет по порту; `plain` отвергается.
    static func parseAutoconfig(_ xml: String, email: String) -> Autoconfig? {
        guard let root = XMLNode.parse(xml) else { return nil }
        func pick(_ tag: String, _ kind: String, _ rule: (Int) -> MailSecurity) -> (Endpoint, String?)? {
            for n in root.descendants(tag) where n.attrs["type"] == kind {
                guard let hostTpl = n.child("hostname"), let port = n.child("port").flatMap({ Int($0) }),
                      let sock = n.child("socketType")?.uppercased() else { continue }
                let sec: MailSecurity
                switch sock {
                case "SSL", "TLS": sec = .tls
                case "STARTTLS": sec = .startTLS
                default: continue
                }
                let host = expand(hostTpl, email).lowercased()
                if rule(port) == sec && !host.isEmpty { return (Endpoint(host, port), n.child("username")) }
            }
            return nil
        }
        let imap = pick("incomingServer", "imap", MailSecurity.imap(port:))
        let smtp = pick("outgoingServer", "smtp", MailSecurity.smtp(port:))
        let login = imap?.1.map { expand($0, email) }.flatMap { $0.isEmpty || $0 == email ? nil : $0 }
        let name = (root.descendants("displayShortName").first ?? root.descendants("displayName").first)?.text
            .trimmingCharacters(in: .whitespacesAndNewlines)
        return Autoconfig(imap: imap?.0, smtp: smtp?.0, login: login, name: name?.isEmpty == false ? name : nil)
    }

    // MARK: Порядок доверия

    /// Проверки разом; победитель — первый успешный по порядку списка: ждём
    /// только тех, кто стоит раньше него, остальные бросаем (их шаги
    /// засчитываются сразу).
    func firstInOrder<T: Sendable>(_ checks: [@Sendable () async -> T?], _ meter: Meter) async -> (Int, T)? {
        guard !checks.isEmpty else { return nil }
        return await withTaskGroup(of: (Int, T?).self) { group in
            for (i, c) in checks.enumerated() {
                group.addTask { (i, await c()) }
            }
            var results = [T??](repeating: nil, count: checks.count)
            var finished = 0
            for await (i, r) in group {
                finished += 1
                meter.tick()
                results[i] = .some(r)
                for (j, slot) in results.enumerated() {
                    guard let r = slot else { break }
                    if let v = r {
                        group.cancelAll()
                        meter.tick(checks.count - finished)
                        return (j, v)
                    }
                }
            }
            return nil
        }
    }

    /// Счётчик прогресса: тики из параллельных проверок, не больше `total`.
    final class Meter: Sendable {
        private let state = Mutex((done: 0, stage: ""))
        private let emit: @Sendable (MailDiscoveryProgress) -> Void

        init(_ emit: @escaping @Sendable (MailDiscoveryProgress) -> Void) { self.emit = emit }

        func tick(_ n: Int = 1) {
            guard n > 0 else { return }
            send { $0.done = min($0.done + n, MailDiscovery.total) }
        }

        func stage(_ s: String) { send { $0.stage = s } }
        func finish() { send { $0.done = MailDiscovery.total } }
        var done: Int { state.withLock { $0.done } }

        private func send(_ change: (inout (done: Int, stage: String)) -> Void) {
            state.withLock { s in
                change(&s)
                emit(MailDiscoveryProgress(done: s.done, total: MailDiscovery.total, stage: s.stage))
            }
        }
    }
}

/// Работа с пределом по времени; не успела или упала — `nil`.
public func withTimeout<T: Sendable>(_ limit: Duration, _ work: @escaping @Sendable () async throws -> T) async -> T? {
    await withTaskGroup(of: T?.self) { group in
        group.addTask { try? await work() }
        group.addTask {
            try? await Task.sleep(for: limit)
            return nil
        }
        let first = await group.next() ?? nil
        group.cancelAll()
        return first
    }
}

/// Маленькое дерево XML (автонастройка, ответы DAV) поверх `XMLParser`.
public final class XMLNode: @unchecked Sendable {
    public let name: String
    public let attrs: [String: String]
    public private(set) var children: [XMLNode] = []
    public private(set) var text = ""

    init(name: String, attrs: [String: String]) {
        self.name = name
        self.attrs = attrs
    }

    /// Разобрать документ; имена — без префикса пространства имён.
    public static func parse(_ xml: String) -> XMLNode? {
        let b = Builder()
        let p = XMLParser(data: Data(xml.utf8))
        p.shouldProcessNamespaces = true
        p.delegate = b
        guard p.parse(), let root = b.root else { return nil }
        return root
    }

    public func child(_ tag: String) -> String? {
        children.first { $0.name == tag }?.text.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    public func first(_ tag: String) -> XMLNode? { children.first { $0.name == tag } }

    public func descendants(_ tag: String) -> [XMLNode] {
        var out: [XMLNode] = []
        for c in children {
            if c.name == tag { out.append(c) }
            out += c.descendants(tag)
        }
        return out
    }

    private final class Builder: NSObject, XMLParserDelegate {
        var root: XMLNode?
        var stack: [XMLNode] = []

        func parser(_ parser: XMLParser, didStartElement elementName: String, namespaceURI: String?, qualifiedName qName: String?, attributes: [String: String] = [:]) {
            let n = XMLNode(name: elementName, attrs: attributes)
            stack.last?.children.append(n)
            if root == nil { root = n }
            stack.append(n)
        }

        func parser(_ parser: XMLParser, didEndElement elementName: String, namespaceURI: String?, qualifiedName qName: String?) {
            _ = stack.popLast()
        }

        func parser(_ parser: XMLParser, foundCharacters string: String) {
            stack.last?.text += string
        }

        func parser(_ parser: XMLParser, foundCDATA CDATABlock: Data) {
            stack.last?.text += String(decoding: CDATABlock, as: UTF8.self)
        }
    }
}
