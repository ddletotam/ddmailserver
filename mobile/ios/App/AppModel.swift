import DDMailAPI
import DDMailAuth
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
        if ProcessInfo.processInfo.arguments.contains("-ddmail-reset") { store.clear() }
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

    /// Повторный вход: тот же сервер и логин, осталось ввести пароль.
    func relogin() {
        guard case let .connected(c) = phase else { return }
        phase = .connect(makeConnect(.init(server: c.session.serverURL, username: c.session.username)))
    }

    /// Выход: сессия чужой учётки не остаётся на устройстве.
    func logout() {
        store.clear()
        phase = .connect(makeConnect(.init()))
    }

    private func makeConnect(_ prefill: ConnectModel.Prefill) -> ConnectModel {
        ConnectModel(transport: transport, store: store, prefill: prefill) { [weak self] in self?.connected($0) }
    }
}

/// Живая сессия: API и её состояние.
@MainActor @Observable
final class Connection {
    let session: Session
    let api: SessionAPI
    private(set) var status: SessionStatus = .checking
    private var checking = false

    init(session: Session, store: SessionStore, transport: HTTPTransport) {
        self.session = session
        api = SessionAPI(
            baseURL: session.serverURL,
            transport: transport,
            tokens: StoredTokens(session: session, store: store)
        )
    }

    /// Проверить сессию лёгким запросом. Синхронизация (следующий этап) будет
    /// сообщать то же самое по итогам каждого прохода.
    func check() async {
        guard !checking else { return }
        checking = true
        defer { checking = false }
        do {
            _ = try await api.identities()
            status = .ok
        } catch APIError.unauthorized {
            status = .dead
        } catch is CancellationError {
        } catch {
            // «Нужен вход» не смывается сетевой ошибкой: ретраи его не лечат.
            if status != .dead { status = .offline(error.localizedDescription) }
        }
    }
}
