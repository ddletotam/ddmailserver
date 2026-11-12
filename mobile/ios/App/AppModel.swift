import DDMailAPI
import DDMailAuth
import DDMailDAV
import DDMailStore
import DDMailSync
import Foundation
import Observation

/// Состояние учётки (контракт §2, внешние учётки §7): «нужен вход» отдельно
/// от «нет связи» — и отдельно для каждой учётки.
enum SessionStatus: Equatable {
    case checking
    case ok
    case offline(String)
    /// Ретраи не помогут, нужен вход (refresh ddmail отказал, IMAP/SMTP отверг
    /// пароль). Липкое: снимает его только удачный проход или новый вход.
    case dead
}

/// Корень приложения: учётки есть — рабочее пространство, нет — подключение.
@MainActor @Observable
final class AppModel {
    enum Phase {
        case connect(ConnectModel)
        case ready(Workspace)
    }

    private(set) var phase: Phase
    /// «Добавить учётку» и «Войти» поверх рабочего пространства — то же окно подключения.
    var connectSheet: ConnectModel?
    let accounts: AccountStore
    let transport: HTTPTransport

    init(accounts: KeychainAccountStore = KeychainAccountStore(), transport: HTTPTransport = AppModel.makeSession()) {
        self.accounts = accounts
        self.transport = transport
        phase = .connect(ConnectModel(transport: transport))
        #if DEBUG
        // UI-тесты начинают с чистого листа: секреты в Keychain симулятора
        // переживают переустановку приложения.
        if ProcessInfo.processInfo.arguments.contains("-ddmail-reset") {
            accounts.removeAll()
            KeychainSessionStore().clear()
            try? FileManager.default.removeItem(at: Workspace.cacheURL.deletingLastPathComponent())
            if let id = Bundle.main.bundleIdentifier { UserDefaults.standard.removePersistentDomain(forName: id) }
        }
        #endif
        accounts.migrateLegacy()
        phase = Self.phase(for: accounts.accounts(), self)
    }

    private static func phase(for list: [MailAccount], _ app: AppModel) -> Phase {
        if list.isEmpty { return .connect(app.makeConnect(.init())) }
        return .ready(Workspace(accounts: list, store: app.accounts, transport: app.transport))
    }

    /// URLSession клиента: без кук и без дискового кэша ответов — в ответах
    /// API письма и адреса, им нечего делать в общем кэше.
    nonisolated static func makeSession() -> URLSession {
        let c = URLSessionConfiguration.default
        c.httpCookieStorage = nil
        c.httpShouldSetCookies = false
        c.urlCache = nil
        c.requestCachePolicy = .reloadIgnoringLocalCacheData
        c.timeoutIntervalForRequest = 30
        return URLSession(configuration: c)
    }

    var workspace: Workspace? { if case let .ready(w) = phase { w } else { nil } }

    /// Учётка добавлена или вошли заново: набор учёток другой — рабочее
    /// пространство собирается заново (кэш остаётся: он по учёткам).
    func added(_ account: MailAccount) {
        connectSheet = nil
        reload()
    }

    func reload() {
        workspace?.close()
        phase = Self.phase(for: accounts.accounts(), self)
    }

    /// «Добавить учётку» из настроек.
    func addAccount() {
        connectSheet = makeConnect(.init(adding: true))
    }

    /// Повторный вход в учётку: тот же сервер и логин, осталось ввести пароль.
    func relogin(_ key: String) {
        guard let a = accounts.accounts().first(where: { $0.key == key }) else { return }
        connectSheet = makeConnect(.init(relogin: a))
    }

    /// Правка настроек учётки (адреса, имя, DAV) — без секрета.
    func update(_ account: MailAccount) {
        try? accounts.upsert(account, secret: nil)
        reload()
    }

    /// Удалить учётку: секреты и кэш учётки (контракт §1).
    func remove(_ key: String) {
        workspace?.forget(key)
        try? accounts.remove(key)
        if accounts.accounts().isEmpty {
            workspace?.close()
            Workspace.clearDeviceState()
        }
        reload()
    }

    private func makeConnect(_ prefill: ConnectModel.Prefill) -> ConnectModel {
        ConnectModel(transport: transport, accounts: accounts, prefill: prefill) { [weak self] in self?.added($0) }
    }
}

/// Живая учётка: источник почты и её состояние.
@MainActor @Observable
final class AccountLink: Identifiable {
    let account: MailAccount
    let provider: any MailProvider
    /// Только у ddmail: API сервера (календарь, контакты, аватарки, WebSocket).
    let api: SessionAPI?
    let engine: SyncEngine?
    /// Только у IMAP: движок (IDLE, закрытие соединений).
    let imap: ImapEngine?
    private(set) var status: SessionStatus = .checking

    nonisolated var id: String { account.key }

    init(account: MailAccount, provider: any MailProvider, api: SessionAPI? = nil, engine: SyncEngine? = nil, imap: ImapEngine? = nil) {
        self.account = account
        self.provider = provider
        self.api = api
        self.engine = engine
        self.imap = imap
    }

    /// Итог синхронизации или проверки. «Нужен вход» липкий: временная
    /// ошибка его не снимает.
    func report(_ s: SessionStatus) {
        if status == .dead, case .offline = s { return }
        status = s
    }

    /// Ошибка прохода → состояние.
    func report(error: Error) {
        if isAuthFailure(error) { report(.dead) } else if !(error is CancellationError) { report(.offline(error.localizedDescription)) }
    }
}

/// Учётки, кэш почты, почта на экране и разделы ddmail (календарь, задачи,
/// контакты — у первой ddmail-учётки; внешние CalDAV/CardDAV — §8).
@MainActor @Observable
final class Workspace {
    let store: AccountStore
    let cache: MailStore?
    let links: [AccountLink]
    /// Кэш не открылся (диск полон, файл повреждён) — почта без кэша не работает.
    private(set) var cacheError: String?
    @ObservationIgnored private(set) lazy var mail: MailModel? = cache.map { MailModel(workspace: self, cache: $0) }

    /// Первая ddmail-учётка: её сервер ведёт календарь, задачи, контакты,
    /// аватарки и документы папок.
    var primary: AccountLink? { links.first { $0.engine != nil } }

    /// Общий календарь: ddmail (первая учётка) и CalDAV внешних учёток (§8).
    @ObservationIgnored private(set) lazy var calendarHub: CalendarHub? = {
        var sources: [(key: String, source: any CalendarSource)] = []
        if let p = primary, let api = p.api { sources.append((p.account.key, api)) }
        for l in links where l.account.kind == .imap {
            guard let url = URL(string: l.account.caldavURL), !l.account.caldavURL.isEmpty else { continue }
            sources.append((l.account.key, DAVCalendarSource(accountKey: l.account.key, url: url, email: l.account.email, client: davClient(l.account))))
        }
        return sources.isEmpty ? nil : CalendarHub(sources, onFailure: reportFailure)
    }()

    /// Общая книга: ddmail и CardDAV внешних учёток.
    @ObservationIgnored private(set) lazy var contactsHub: ContactsHub? = {
        var sources: [(key: String, source: any ContactsSource)] = []
        if let p = primary, let api = p.api { sources.append((p.account.key, api)) }
        for l in links where l.account.kind == .imap {
            guard let url = URL(string: l.account.carddavURL), !l.account.carddavURL.isEmpty else { continue }
            sources.append((l.account.key, DAVContactsSource(accountKey: l.account.key, url: url, email: l.account.email, client: davClient(l.account))))
        }
        return sources.isEmpty ? nil : ContactsHub(sources, onFailure: reportFailure)
    }()

    /// DAV внешней учётки — Basic паролем учётки; пароль — из Keychain при каждом запросе.
    private func davClient(_ a: MailAccount) -> DAVClient {
        let store = self.store, key = a.key
        return DAVClient(user: a.imapLogin, password: { store.secret(key)?.password })
    }

    /// Отказ источника календаря или контактов — состояние его учётки.
    private var reportFailure: CalendarHub.Failure {
        { [weak self] key, error in
            await MainActor.run { self?.link(key)?.report(error: error) }
        }
    }

    @ObservationIgnored private(set) lazy var calendar: CalendarModel? = calendarHub.map { hub in
        let m = CalendarModel(api: hub) { [weak self] in self?.primary?.report(.dead) }
        // Календарь перечитан (правка, calendar_updated) — напоминания пересеять.
        m.onReloaded = { [weak self] in Task { await self?.reminders?.refresh() } }
        m.myEmails = Set(links.flatMap { $0.provider.identities().map { $0.email.lowercased() } })
        return m
    }
    @ObservationIgnored private(set) lazy var reminders: ReminderService? = calendarHub.map { ReminderService(api: $0) }
    @ObservationIgnored private(set) lazy var tasks: TasksModel? = primary?.api.map { api in
        TasksModel(api: api) { [weak self] in self?.primary?.report(.dead) }
    }
    @ObservationIgnored private(set) lazy var contacts: ContactsModel? = contactsHub.map { hub in
        ContactsModel(api: hub) { [weak self] in self?.primary?.report(.dead) }
    }

    init(accounts: [MailAccount], store: AccountStore, transport: HTTPTransport) {
        self.store = store
        var cache: MailStore?
        do {
            cache = try MailStore(url: Self.cacheURL)
        } catch {
            cacheError = error.localizedDescription
        }
        self.cache = cache
        links = accounts.compactMap { Self.link($0, store: store, cache: cache, transport: transport) }
    }

    private static func link(_ a: MailAccount, store: AccountStore, cache: MailStore?, transport: HTTPTransport) -> AccountLink? {
        guard let cache else { return nil }
        switch a.kind {
        case .ddmail:
            // Секрета нет (восстановили из копии) — учётка видна, но просит входа.
            let session = store.secret(a.key)?.session ?? Session(serverURL: a.serverURL, username: a.username, userID: a.userID, token: "")
            // Один источник токена на оба API: обменявший токен не оставит другому просроченный.
            let tokens = StoredTokens(session: session, store: AccountSessionStore(key: a.key, store: store))
            let api = SessionAPI(baseURL: session.serverURL, transport: transport, tokens: tokens)
            let wifi = SessionAPI(baseURL: session.serverURL, transport: wifiOnlySession, tokens: tokens)
            let engine = SyncEngine(accountKey: a.key, api: api, store: cache, prefetchAPI: wifi)
            let link = AccountLink(account: a, provider: engine, api: api, engine: engine)
            if session.token.isEmpty { link.report(.dead) }
            return link
        case .imap:
            guard let imap = a.imap, let smtp = a.smtp else { return nil }
            let config = ImapAccountConfig(
                key: a.key, email: a.email, displayName: a.displayName, login: a.imapLogin, imap: imap, smtp: smtp,
                smtpLogin: a.smtpLogin, smtpAuth: a.smtpAuth, ownAddresses: a.ownAddresses
            )
            let key = a.key
            // Пароль читается из Keychain при каждом соединении: в памяти его не держим.
            let engine = ImapEngine(config: config, store: cache, password: { store.secret(key)?.password })
            let link = AccountLink(account: a, provider: engine, imap: engine)
            if store.secret(a.key)?.password == nil { link.report(.dead) }
            return link
        }
    }

    nonisolated static let wifiOnlySession: URLSession = {
        let c = URLSessionConfiguration.default
        c.httpCookieStorage = nil
        c.urlCache = nil
        c.allowsExpensiveNetworkAccess = false
        c.allowsConstrainedNetworkAccess = false
        c.timeoutIntervalForRequest = 30
        return URLSession(configuration: c)
    }()

    /// Кэш — в Application Support (не в Caches: система не должна его
    /// выбрасывать, иначе на старте пустой список до ответа сервера).
    static var cacheURL: URL {
        URL.applicationSupportDirectory.appending(path: "ddmail/cache.sqlite")
    }

    func link(_ key: String) -> AccountLink? { links.first { $0.account.key == key } }

    /// Учётки, которым нужен вход.
    var dead: [AccountLink] { links.filter { $0.status == .dead } }

    /// Докачать тела по выбору пользователя (§4): «Всегда» — обычной сетью,
    /// «Только по Wi-Fi» — экономной, «Нет» — ничего. Учётки — по очереди.
    func prefetch() async {
        let constrained: Bool
        switch AppSettings.shared.prefetch {
        case .always: constrained = false
        case .wifi: constrained = true
        case .never, nil: return
        }
        for l in links where l.status != .dead {
            do {
                _ = try await l.provider.prefetchBodies(constrained: constrained) { !Task.isCancelled }
            } catch {
                if isAuthFailure(error) { l.report(.dead) }
            }
        }
    }

    /// Фоновый проход (BGAppRefreshTask): синк всех учёток и уведомления из кэша.
    func backgroundRefresh() async {
        await mail?.syncAll(notify: true)
        await reminders?.refresh()
    }

    /// Учётку удаляют: её кэш и уведомления — прочь.
    func forget(_ key: String) {
        try? cache?.clearAccount(key)
        Notifier.clear(account: key)
        AppSettings.shared.forget(account: key)
    }

    /// Остановить наблюдение (пространство пересобирается или учёток не осталось).
    func close() {
        mail?.stop()
        for l in links { if let e = l.imap { Task { await e.close() } } }
    }

    /// Учёток не осталось: состояние чужого человека не остаётся на устройстве.
    static func clearDeviceState() {
        ImageSenders.clear()
        AvatarStore.clearDisk()
        AppSettings.shared.clearAccountState()
        CalendarPrefs.shared.clear()
        Task { await ReminderService.clearAll() }
        Notifier.clearAll()
    }
}
