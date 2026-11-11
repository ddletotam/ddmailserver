import DDMailAPI
import DDMailAuth
import DDMailStore
import DDMailSync
import Foundation
import Observation

/// Состояние сессии (контракт §2): «нужен вход» отдельно от «нет связи».
enum SessionStatus: Equatable {
    case checking
    case ok
    case offline(String)
    /// Refresh отказал — ретраи не помогут, нужен вход. Липкое: снимает его
    /// только удачный запрос (токен обменялся) или новый вход.
    case dead
}

/// Корень приложения: есть ли сессия, и что с ней.
@MainActor @Observable
final class AppModel {
    enum Phase {
        case connect(ConnectModel)
        case connected(Connection)
    }

    private(set) var phase: Phase
    let store: SessionStore
    let transport: HTTPTransport

    init(store: SessionStore = KeychainSessionStore(), transport: HTTPTransport = AppModel.makeSession()) {
        self.store = store
        self.transport = transport
        phase = .connect(ConnectModel(transport: transport))
        #if DEBUG
        // UI-тесты начинают с чистого листа: сессия в Keychain симулятора
        // переживает переустановку приложения.
        if ProcessInfo.processInfo.arguments.contains("-ddmail-reset") {
            store.clear()
            if let id = Bundle.main.bundleIdentifier { UserDefaults.standard.removePersistentDomain(forName: id) }
        }
        #endif
        if let s = store.load() {
            phase = .connected(Connection(session: s, store: store, transport: transport))
        } else {
            phase = .connect(makeConnect(ConnectModel.Prefill()))
        }
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

    /// Вход прошёл: сессия уже в Keychain.
    func connected(_ session: Session) {
        phase = .connected(Connection(session: session, store: store, transport: transport))
    }

    /// Текущая живая сессия (для фона и уведомлений).
    var connection: Connection? { if case let .connected(c) = phase { c } else { nil } }

    /// Повторный вход: тот же сервер и логин, осталось ввести пароль.
    func relogin() {
        guard case let .connected(c) = phase else { return }
        phase = .connect(makeConnect(.init(server: c.session.serverURL, username: c.session.username)))
    }

    /// Выход: сессия чужой учётки не остаётся на устройстве.
    func logout() {
        if case let .connected(c) = phase { c.close() }
        store.clear()
        phase = .connect(makeConnect(.init()))
    }

    private func makeConnect(_ prefill: ConnectModel.Prefill) -> ConnectModel {
        ConnectModel(transport: transport, store: store, prefill: prefill) { [weak self] in self?.connected($0) }
    }
}

/// Живая сессия: API, кэш почты, движок синхронизации и состояние.
@MainActor @Observable
final class Connection {
    let session: Session
    let api: SessionAPI
    /// Докачка «Только по Wi-Fi»: система не пустит эти запросы в сотовую сеть,
    /// через точку доступа и в режиме экономии трафика (§4).
    let prefetchAPI: SessionAPI
    let cache: MailStore?
    let engine: SyncEngine?
    @ObservationIgnored private(set) lazy var mail: MailModel? = engine.map { MailModel(connection: self, engine: $0) }
    @ObservationIgnored private(set) lazy var calendar: CalendarModel = {
        let m = CalendarModel(api: api) { [weak self] in self?.report(.dead) }
        // Календарь перечитан (правка, calendar_updated) — напоминания пересеять.
        m.onReloaded = { [weak self] in Task { await self?.reminders.refresh() } }
        return m
    }()
    @ObservationIgnored private(set) lazy var reminders = ReminderService(api: api)
    @ObservationIgnored private(set) lazy var tasks = TasksModel(api: api) { [weak self] in self?.report(.dead) }
    @ObservationIgnored private(set) lazy var contacts = ContactsModel(api: api) { [weak self] in self?.report(.dead) }
    private(set) var status: SessionStatus = .checking
    /// Кэш не открылся (диск полон, файл повреждён) — почта без кэша не работает.
    private(set) var cacheError: String?

    init(session: Session, store: SessionStore, transport: HTTPTransport) {
        self.session = session
        // Один источник токена на оба API: обменявший токен не оставит
        // другому просроченный.
        let tokens = StoredTokens(session: session, store: store)
        api = SessionAPI(baseURL: session.serverURL, transport: transport, tokens: tokens)
        prefetchAPI = SessionAPI(baseURL: session.serverURL, transport: Self.wifiOnlySession, tokens: tokens)
        do {
            let c = try MailStore(url: Self.cacheURL)
            // Кэш другой учётки (вошли под другим логином) — не наш: с нуля.
            if try c.meta("account_key") != session.accountKey {
                try c.clearAll()
                try c.setMeta("account_key", session.accountKey)
            }
            cache = c
            engine = SyncEngine(accountKey: session.accountKey, api: api, store: c)
        } catch {
            cache = nil
            engine = nil
            cacheError = error.localizedDescription
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

    /// Докачать тела по выбору пользователя (§4): «Всегда» — обычным API,
    /// «Только по Wi-Fi» — экономным, «Нет» — ничего.
    func prefetch() async {
        guard let engine else { return }
        let api: SessionAPI
        switch AppSettings.shared.prefetch {
        case .always: api = self.api
        case .wifi: api = prefetchAPI
        case .never, nil: return
        }
        do {
            _ = try await engine.prefetchBodies(via: api) { !Task.isCancelled }
        } catch APIError.unauthorized {
            report(.dead)
        } catch {}
    }

    /// Фоновый проход (BGAppRefreshTask): синк и уведомления из кэша.
    func backgroundRefresh() async {
        guard let engine else { return }
        do {
            try await engine.sync()
            report(.ok)
            await Notifier.check(engine: engine, openID: nil)
            await reminders.refresh()
        } catch APIError.unauthorized {
            report(.dead)
        } catch {}
    }

    /// Кэш — в Application Support (не в Caches: система не должна его
    /// выбрасывать, иначе на старте пустой список до ответа сервера).
    static var cacheURL: URL {
        URL.applicationSupportDirectory.appending(path: "ddmail/cache.sqlite")
    }

    /// Итог синхронизации или проверки (§2). «Нужен вход» липкий: его снимает
    /// только удачный проход (токен обменялся, запросы прошли) или новый вход.
    func report(_ s: SessionStatus) {
        if status == .dead, case .offline = s { return }
        status = s
    }

    /// Проверить сессию лёгким запросом — когда синхронизировать нечем (кэш не открылся).
    func check() async {
        do {
            _ = try await api.identities()
            report(.ok)
        } catch APIError.unauthorized {
            report(.dead)
        } catch is CancellationError {
        } catch {
            report(.offline(error.localizedDescription))
        }
    }

    /// Выход: остановить наблюдение и стереть кэш учётки.
    func close() {
        mail?.stop()
        try? cache?.clearAll()
        ImageSenders.clear()
        AppSettings.shared.clearAccountState()
        CalendarPrefs.shared.clear()
        let r = reminders
        Task { await r.clear() }
        Notifier.clearAll()
    }
}
