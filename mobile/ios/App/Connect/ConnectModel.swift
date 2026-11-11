import DDMailAPI
import DDMailAuth
import Foundation
import Observation

/// Подключение (контракт §3, как `ConnectViewModel` Android): адрес почты →
/// поиск сервера ddmail → вход. Ничего не сохраняется, пока вход не прошёл;
/// пароль не сохраняется вовсе.
@MainActor @Observable
final class ConnectModel {
    enum Step { case email, searching, found, manual }

    struct Prefill {
        var server = ""
        var username = ""
    }

    var step: Step = .email
    var email = ""
    var server = ""
    var username = ""
    var password = ""
    private(set) var progress: DiscoveryProgress?
    /// Нейтральное сообщение (не ошибка): «сервер не найден, введите вручную».
    private(set) var notice: String?
    var error: String?
    private(set) var busy = false

    private let transport: HTTPTransport
    private let store: SessionStore?
    private let discovery: ServerDiscovery
    private let onConnected: (Session) -> Void
    private var search: Task<Void, Never>?

    init(
        transport: HTTPTransport,
        store: SessionStore? = nil,
        prefill: Prefill = Prefill(),
        onConnected: @escaping (Session) -> Void = { _ in }
    ) {
        self.transport = transport
        self.store = store
        self.onConnected = onConnected
        discovery = .live(transport: transport)
        if !prefill.server.isEmpty {
            step = .found
            server = prefill.server
            username = prefill.username
        }
    }

    var title: String {
        switch step {
        case .email: "Подключение"
        case .searching: "Ищем сервер"
        case .found: "Вход"
        case .manual: "Вход на сервер ddmail"
        }
    }

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
        let (ticks, sink) = AsyncStream.makeStream(of: DiscoveryProgress.self)
        search = Task { [weak self, discovery] in
            async let found = discovery.find(email: address) { sink.yield($0) }
            // Поиск всегда кончается тиком `done == total`; отмена задачи
            // прерывает и ожидание потока.
            for await p in ticks {
                self?.progress = p
                if p.done == p.total { break }
            }
            let url = await found
            sink.finish()
            guard let self, !Task.isCancelled else { return }
            // Сервер входит по имени пользователя; чаще всего это часть до @.
            if username.isEmpty { username = local }
            if let url {
                server = url
                step = .found
            } else {
                step = .manual
                notice = "Сервер ddmail для \(domain) не найден. Введите его адрес вручную."
            }
        }
    }

    func manual() {
        step = .manual
        error = nil
        notice = nil
    }

    /// «Назад»: из поиска — с отменой поиска; с остальных шагов — к адресу почты.
    func back() {
        guard !busy, step != .email else { return }
        search?.cancel()
        step = .email
        error = nil
        notice = nil
        progress = nil
    }

    func login() {
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
        error = nil
        let pw = password
        Task {
            defer { busy = false }
            do {
                let r = try await DdmailAPI(baseURL: url, transport: transport).login(username: user, password: pw)
                let session = Session(serverURL: url, username: r.username, userID: r.userID, token: r.token)
                try store?.save(session)
                password = ""
                onConnected(session)
            } catch {
                self.error = error.localizedDescription
            }
        }
    }
}
