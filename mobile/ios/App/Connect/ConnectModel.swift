import DDMailAPI
import DDMailAuth
import DDMailDAV
import DDMailNet
import Foundation
import Observation

/// Подключение (контракт §3, внешние учётки §1–§2): адрес почты → поиск
/// (`MailDiscovery`) → нашёлся ddmail — вход на сервер; иначе найденные
/// IMAP/SMTP и пароль; «Я знаю все настройки» — ручная форма. Перед
/// сохранением IMAP-учётки — проверка входа на IMAP и SMTP. Ничего не
/// сохраняется, пока вход не прошёл. Первый запуск и «Добавить учётку» —
/// одно и то же окно.
@MainActor @Observable
final class ConnectModel {
    enum Step {
        case email, searching
        /// Нашёлся ddmail: логин и пароль.
        case found
        /// Адрес сервера ddmail вручную.
        case manual
        /// Найдены IMAP/SMTP: пароль.
        case imap
        /// «Я знаю все настройки».
        case imapManual
    }

    struct Prefill {
        var server = ""
        var username = ""
        /// Окно открыто поверх рабочего пространства («Добавить учётку», «Войти»).
        var sheet = false
        /// Повторный вход в эту учётку.
        var relogin: MailAccount?

        init(server: String = "", username: String = "") {
            self.server = server
            self.username = username
        }

        init(adding: Bool) {
            sheet = adding
        }

        init(relogin a: MailAccount) {
            sheet = true
            relogin = a
            server = a.serverURL
            username = a.username
        }
    }

    var step: Step = .email
    var email = ""
    // ddmail
    var server = ""
    var username = ""
    var password = ""
    // IMAP / SMTP
    var imapHost = ""
    var imapPort = "993"
    var imapSecurity: MailSecurity = .tls
    var smtpHost = ""
    var smtpPort = "465"
    var smtpSecurity: MailSecurity = .tls
    var imapUser = ""
    var smtpNoAuth = false
    var displayName = ""
    /// Название службы и подсказка о пароле приложения.
    private(set) var providerName: String?
    private(set) var passwordHint: String?

    private(set) var progress: MailDiscoveryProgress?
    /// Нейтральное сообщение (не ошибка): «сервер не найден, введите вручную».
    private(set) var notice: String?
    var error: String?
    private(set) var busy = false
    private(set) var busyText = ""

    private let transport: HTTPTransport
    private let accounts: AccountStore?
    private let discovery: MailDiscovery
    private let onAdded: (MailAccount) -> Void
    private var search: Task<Void, Never>?
    let prefill: Prefill

    init(
        transport: HTTPTransport,
        accounts: AccountStore? = nil,
        prefill: Prefill = Prefill(),
        onAdded: @escaping (MailAccount) -> Void = { _ in }
    ) {
        self.transport = transport
        self.accounts = accounts
        self.onAdded = onAdded
        self.prefill = prefill
        discovery = .live(transport: transport)
        if let a = prefill.relogin {
            email = a.email
            if a.kind == .imap {
                fill(from: a)
                step = .imap
                return
            }
        }
        if !prefill.server.isEmpty {
            step = .found
            server = prefill.server
            username = prefill.username
        }
    }

    var title: String {
        switch step {
        case .email: prefill.sheet ? "Новая учётка" : "Подключение"
        case .searching: "Ищем сервер"
        case .found: "Вход"
        case .manual: "Вход на сервер ddmail"
        case .imap: providerName.map { "Вход: \($0)" } ?? "Вход в почту"
        case .imapManual: "Настройки почты"
        }
    }

    var isRelogin: Bool { prefill.relogin != nil }

    // MARK: Поиск

    func findServer() {
        let address = email.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let domain = emailDomain(address) else {
            error = "Введите адрес почты целиком: имя@домен."
            return
        }
        email = address
        step = .searching
        progress = nil
        error = nil
        notice = nil
        let local = address.split(separator: "@").dropLast().joined(separator: "@")
        search?.cancel()
        // Прогресс — потоком в порядке испускания (поиск отдаёт его под
        // замком), поиск — дочерней задачей: отмена `search` отменяет и его.
        let (ticks, sink) = AsyncStream.makeStream(of: MailDiscoveryProgress.self)
        search = Task { [weak self, discovery] in
            async let found = discovery.find(email: address) { sink.yield($0) }
            for await p in ticks {
                self?.progress = p
                if p.done == p.total { break }
            }
            let r = await found
            sink.finish()
            guard let self, !Task.isCancelled else { return }
            if let url = r.ddmailURL {
                // Сервер входит по имени пользователя; чаще всего это часть до @.
                if username.isEmpty { username = local }
                server = url
                step = .found
                return
            }
            applyFound(r, address: address)
            if r.imap != nil && r.smtp != nil {
                step = .imap
            } else {
                step = .imapManual
                notice = "Серверы для \(domain) нашлись не все. Проверьте настройки — подставлены догадки."
            }
        }
    }

    /// Найденное — в поля формы; не найденное — из таблицы служб или догадкой.
    private func applyFound(_ r: DiscoveryResult, address: String) {
        let guess = MailPreset.guess(address)
        let known = MailPreset.known(address)
        providerName = r.provider ?? known?.name
        passwordHint = known?.passwordHint
        let imap = r.imap ?? guess.map { Endpoint($0.imapHost, 993) }
        let smtp = r.smtp ?? guess.map { Endpoint($0.smtpHost, $0.smtpPort) }
        if let imap {
            imapHost = imap.host
            imapPort = String(imap.port)
            imapSecurity = .imap(port: imap.port)
        }
        if let smtp {
            smtpHost = smtp.host
            smtpPort = String(smtp.port)
            smtpSecurity = .smtp(port: smtp.port)
        }
        smtpNoAuth = r.smtp != nil && !r.smtpAuth
        imapUser = r.login ?? address
    }

    private func fill(from a: MailAccount) {
        if let i = a.imap {
            imapHost = i.host
            imapPort = String(i.port)
            imapSecurity = i.security
        }
        if let s = a.smtp {
            smtpHost = s.host
            smtpPort = String(s.port)
            smtpSecurity = s.security
        }
        imapUser = a.imapLogin
        smtpNoAuth = !a.smtpAuth
        displayName = a.displayName
        passwordHint = MailPreset.known(a.email)?.passwordHint
        providerName = MailPreset.known(a.email)?.name
    }

    /// Адрес сервера ddmail вручную.
    func manual() {
        step = .manual
        error = nil
        notice = nil
    }

    /// «Я знаю все настройки»: ручная форма IMAP/SMTP с догадками по адресу.
    func imapManual() {
        if imapHost.isEmpty { applyFound(DiscoveryResult(), address: email) }
        step = .imapManual
        error = nil
        notice = nil
    }

    /// Порт сменили — шифрование следует за ним (как выбирает десктоп).
    func imapPortChanged() {
        if let p = Int(imapPort) { imapSecurity = .imap(port: p) }
    }

    func smtpPortChanged() {
        if let p = Int(smtpPort) { smtpSecurity = .smtp(port: p) }
    }

    /// «Назад»: из поиска — с отменой поиска; с остальных шагов — к адресу почты.
    func back() {
        guard !busy, step != .email else { return }
        search?.cancel()
        step = step == .imapManual && !imapHost.isEmpty && !isRelogin ? .imap : .email
        error = nil
        notice = nil
        progress = nil
    }

    var canGoBack: Bool { step != .email && !(isRelogin && step == .imap) }

    // MARK: Вход ddmail

    func login() {
        if step == .imap || step == .imapManual {
            loginIMAP()
            return
        }
        guard !busy else { return }
        let user = username.trimmingCharacters(in: .whitespacesAndNewlines)
        let normalized = normalizeServerURL(server)
        guard !user.isEmpty, !password.isEmpty, case let .success(url) = normalized else {
            if case let .failure(e) = normalized, !server.trimmingCharacters(in: .whitespaces).isEmpty {
                error = e.localizedDescription
            } else {
                error = "Заполните адрес сервера, логин и пароль."
            }
            return
        }
        server = url
        busy = true
        busyText = "Входим…"
        error = nil
        let pw = password
        Task {
            defer { busy = false }
            do {
                let r = try await DdmailAPI(baseURL: url, transport: transport).login(username: user, password: pw)
                let session = Session(serverURL: url, username: r.username, userID: r.userID, token: r.token)
                var account = MailAccount.ddmail(session, email: email.contains("@") ? email.lowercased() : "")
                if let clash = clash(account.key, kind: .ddmail) {
                    error = clash
                    return
                }
                // Повторный вход — настройки учётки (адреса DAV и т. п.) сохраняются.
                if let old = existing(account.key) {
                    account = old
                    account.serverURL = session.serverURL
                    account.username = session.username
                    account.userID = session.userID
                }
                try accounts?.upsert(account, secret: .session(session))
                password = ""
                onAdded(account)
            } catch {
                self.error = error.localizedDescription
            }
        }
    }

    // MARK: Вход IMAP

    /// Проверить вход на IMAP и SMTP и сохранить учётку.
    func loginIMAP() {
        guard !busy else { return }
        let address = email.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        let user = imapUser.trimmingCharacters(in: .whitespacesAndNewlines)
        guard address.contains("@") else {
            error = "Введите адрес почты."
            return
        }
        guard !imapHost.isEmpty, let ip = Int(imapPort), (1...65535).contains(ip),
              !smtpHost.isEmpty, let sp = Int(smtpPort), (1...65535).contains(sp) else {
            error = "Проверьте серверы и порты входящей и исходящей почты."
            return
        }
        guard !user.isEmpty, !password.isEmpty else {
            error = "Введите логин и пароль."
            return
        }
        var account = MailAccount.imap(
            email: address, login: user,
            imap: MailEndpoint(host: imapHost.trimmingCharacters(in: .whitespaces).lowercased(), port: ip, security: imapSecurity),
            smtp: MailEndpoint(host: smtpHost.trimmingCharacters(in: .whitespaces).lowercased(), port: sp, security: smtpSecurity),
            smtpAuth: !smtpNoAuth, displayName: displayName.trimmingCharacters(in: .whitespaces)
        )
        if let clash = clash(account.key, kind: .imap) {
            error = clash
            return
        }
        // Повторный вход — прочие настройки учётки (адреса, DAV) остаются.
        if let old = existing(account.key) {
            var kept = old
            kept.imap = account.imap
            kept.smtp = account.smtp
            kept.imapLogin = account.imapLogin
            kept.smtpAuth = account.smtpAuth
            kept.email = account.email
            if !account.displayName.isEmpty { kept.displayName = account.displayName }
            account = kept
        }
        busy = true
        busyText = "Проверяем вход…"
        error = nil
        let pw = password
        let candidate = account
        Task {
            defer { busy = false }
            do {
                try await AccountCheck.check(candidate, password: pw)
                var account = candidate
                // Адреса CalDAV/CardDAV — по домену, тем же паролем (§8); не нашлись — правятся в настройках.
                if account.caldavURL.isEmpty || account.carddavURL.isEmpty {
                    busyText = "Ищем календарь и контакты…"
                    let dav = DAVClient(user: account.imapLogin, password: { pw })
                    let host = account.imap?.host ?? "", mail = account.email
                    async let cal = dav.find(email: mail, imapHost: host, kind: .calendar)
                    async let card = dav.find(email: mail, imapHost: host, kind: .addressBook)
                    let (c, d) = await (cal, card)
                    if account.caldavURL.isEmpty, let c { account.caldavURL = c.absoluteString }
                    if account.carddavURL.isEmpty, let d { account.carddavURL = d.absoluteString }
                }
                try accounts?.upsert(account, secret: .password(pw))
                password = ""
                onAdded(account)
            } catch {
                self.error = error.localizedDescription
            }
        }
    }

    private func existing(_ key: String) -> MailAccount? {
        accounts?.accounts().first { $0.key == key }
    }

    /// Тот же ящик уже подключён другим способом — двух учёток с одним
    /// ключом быть не может (кэш и документы общие по ключу).
    private func clash(_ key: String, kind: MailAccount.Kind) -> String? {
        guard let old = existing(key), old.kind != kind else { return nil }
        return old.kind == .ddmail
            ? "Этот ящик уже подключён через сервер ddmail. Удалите ту учётку в настройках, чтобы подключить его по IMAP."
            : "Этот ящик уже подключён по IMAP. Удалите ту учётку в настройках, чтобы подключить его через сервер ddmail."
    }
}
