import Foundation
import Network
import Security

/// Шифрование соединения с почтовым сервером. Открытого текста нет (контракт
/// внешних учёток §10): либо TLS сразу, либо STARTTLS до любых данных учётки.
public enum MailSecurity: String, Codable, Sendable {
    /// Неявный TLS (IMAP 993, SMTP 465).
    case tls
    /// Открытое приветствие, затем STARTTLS (IMAP 143, SMTP 587/25/2525).
    case startTLS

    /// Режим по порту — как выбирает десктоп (`discover::imap_security`).
    public static func imap(port: Int) -> MailSecurity { port == 143 ? .startTLS : .tls }
    /// `discover::smtp_security`: 587, 25 и 2525 — STARTTLS, остальное — TLS.
    public static func smtp(port: Int) -> MailSecurity { [587, 25, 2525].contains(port) ? .startTLS : .tls }
}

/// Ошибки соединения, разделённые по тому, что с ними делать (как `APIError`):
/// `auth` ретраи не лечат — нужен вход; остальное временное или текст сервера.
public enum MailNetError: Error, Equatable, Sendable {
    /// Неверный логин или пароль (IMAP `NO` на LOGIN, SMTP 535).
    case auth(String)
    /// Не удалось соединиться (DNS, отказ, нет сети).
    case connect(String)
    /// Ошибка TLS — текст как есть (контракт §1: показывать как есть).
    case tls(String)
    case timeout
    /// Сервер закрыл соединение.
    case closed
    /// Ответ сервера с отказом на команду; текст — от сервера.
    case server(String)
    /// Ответ не по протоколу.
    case protocolError(String)

    /// Ретраи не лечат — нужен вход.
    public var isAuth: Bool { if case .auth = self { true } else { false } }
}

extension MailNetError: LocalizedError {
    public var errorDescription: String? {
        switch self {
        case .auth: "Неверный логин или пароль."
        case let .connect(s): "Сервер не отвечает: \(s)"
        case let .tls(s): "Ошибка TLS: \(s)"
        case .timeout: "Сервер не отвечает (истекло время ожидания)."
        case .closed: "Сервер закрыл соединение."
        case let .server(s): s
        case let .protocolError(s): "Сервер ответил не по протоколу: \(s)"
        }
    }
}

/// Двусторонний поток байт до почтового сервера.
public protocol MailSocket: AnyObject, Sendable {
    func send(_ data: Data) async throws
    /// Очередная порция; пустая — сервер закрыл соединение.
    func receive() async throws -> Data
    /// STARTTLS: дальше — TLS на том же соединении.
    func startTLS() async throws
    func close()
}

/// Параметры соединения.
public struct MailEndpoint: Equatable, Hashable, Codable, Sendable {
    public var host: String
    public var port: Int
    public var security: MailSecurity

    public init(host: String, port: Int, security: MailSecurity) {
        self.host = host
        self.port = port
        self.security = security
    }
}

/// Как открывать соединения. Подменяется в тестах.
public protocol MailConnector: Sendable {
    func connect(_ endpoint: MailEndpoint, timeout: Duration) async throws -> MailSocket
}

/// Сеть платформы: неявный TLS — `NWConnection` (Network.framework),
/// STARTTLS — `URLSessionStreamTask`: Network.framework не умеет включить TLS
/// на уже открытом соединении, а поток задачи URLSession умеет
/// (`startSecureConnection`). Доверие в обоих — системное хранилище
/// (контракт §10), TLS не ниже 1.2, проверка имени хоста — системная.
/// `constrained` — «только по Wi-Fi»: дорогие и экономные сети запрещены.
public struct SystemMailConnector: MailConnector {
    public let constrained: Bool

    public init(constrained: Bool = false) { self.constrained = constrained }

    public func connect(_ endpoint: MailEndpoint, timeout: Duration) async throws -> MailSocket {
        switch endpoint.security {
        case .tls:
            let s = NWMailSocket(host: endpoint.host, port: endpoint.port, constrained: constrained)
            try await s.open(timeout: timeout)
            return s
        case .startTLS:
            return StreamTaskSocket(host: endpoint.host, port: endpoint.port, constrained: constrained, timeout: timeout)
        }
    }
}

/// Текст ошибки TLS из кода Security.
func tlsText(_ status: OSStatus) -> String {
    (SecCopyErrorMessageString(status, nil) as String?) ?? "код \(status)"
}

// MARK: - NWConnection (неявный TLS)

final class NWMailSocket: MailSocket, @unchecked Sendable {
    private let connection: NWConnection
    private let queue = DispatchQueue(label: "ru.letotam.ddmail.mailsocket")

    init(host: String, port: Int, constrained: Bool) {
        let tls = NWProtocolTLS.Options()
        sec_protocol_options_set_min_tls_protocol_version(tls.securityProtocolOptions, .TLSv12)
        let tcp = NWProtocolTCP.Options()
        tcp.enableKeepalive = true
        tcp.keepaliveIdle = 60
        let params = NWParameters(tls: tls, tcp: tcp)
        if constrained {
            params.prohibitExpensivePaths = true
            params.prohibitConstrainedPaths = true
        }
        connection = NWConnection(
            host: NWEndpoint.Host(host), port: NWEndpoint.Port(rawValue: UInt16(clamping: port)) ?? 993, using: params
        )
    }

    /// Ждём `ready`; `waiting` (нет сети, отказ) и `failed` — ошибка.
    func open(timeout: Duration) async throws {
        let once = Once()
        try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Void, Error>) in
                connection.stateUpdateHandler = { [connection] state in
                    switch state {
                    case .ready:
                        once.run { cont.resume() }
                    case let .waiting(e), let .failed(e):
                        once.run {
                            connection.cancel()
                            cont.resume(throwing: Self.map(e))
                        }
                    case .cancelled:
                        once.run { cont.resume(throwing: MailNetError.closed) }
                    default: break
                    }
                }
                connection.start(queue: queue)
                queue.asyncAfter(deadline: .now() + timeout.seconds) { [connection] in
                    once.run {
                        connection.cancel()
                        cont.resume(throwing: MailNetError.timeout)
                    }
                }
            }
        } onCancel: { [connection] in
            connection.cancel()
        }
    }

    static func map(_ e: NWError) -> MailNetError {
        switch e {
        case let .tls(status): .tls(tlsText(status))
        case let .posix(code): .connect(String(cString: strerror(code.rawValue)))
        case let .dns(code): .connect("DNS: \(code)")
        default: .connect(e.localizedDescription)
        }
    }

    func send(_ data: Data) async throws {
        try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Void, Error>) in
            connection.send(content: data, completion: .contentProcessed { e in
                if let e { cont.resume(throwing: Self.map(e)) } else { cont.resume() }
            })
        }
    }

    func receive() async throws -> Data {
        try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Data, Error>) in
                connection.receive(minimumIncompleteLength: 1, maximumLength: 256 * 1024) { data, _, complete, e in
                    if let data, !data.isEmpty { cont.resume(returning: data) }
                    else if let e { cont.resume(throwing: Self.map(e)) }
                    else if complete { cont.resume(returning: Data()) }
                    else { cont.resume(returning: Data()) }
                }
            }
        } onCancel: { [connection] in
            connection.cancel()
        }
    }

    func startTLS() async throws {
        throw MailNetError.protocolError("соединение уже под TLS")
    }

    func close() { connection.cancel() }
}

// MARK: - URLSessionStreamTask (STARTTLS)

final class StreamTaskSocket: MailSocket, @unchecked Sendable {
    private let session: URLSession
    private let task: URLSessionStreamTask
    private let timeout: TimeInterval

    init(host: String, port: Int, constrained: Bool, timeout: Duration) {
        let c = URLSessionConfiguration.ephemeral
        c.tlsMinimumSupportedProtocolVersion = .TLSv12
        c.allowsExpensiveNetworkAccess = !constrained
        c.allowsConstrainedNetworkAccess = !constrained
        c.timeoutIntervalForRequest = max(timeout.seconds, 25)
        session = URLSession(configuration: c)
        task = session.streamTask(withHostName: host, port: port)
        self.timeout = max(timeout.seconds, 25)
        task.resume()
    }

    static func map(_ e: Error) -> MailNetError {
        let ns = e as NSError
        if ns.domain == NSURLErrorDomain {
            switch ns.code {
            case NSURLErrorTimedOut: return .timeout
            case NSURLErrorSecureConnectionFailed, NSURLErrorServerCertificateUntrusted,
                 NSURLErrorServerCertificateHasBadDate, NSURLErrorServerCertificateNotYetValid,
                 NSURLErrorServerCertificateHasUnknownRoot, NSURLErrorClientCertificateRejected:
                return .tls(ns.localizedDescription)
            default: return .connect(ns.localizedDescription)
            }
        }
        if ns.domain == NSOSStatusErrorDomain { return .tls(tlsText(OSStatus(ns.code))) }
        if ns.domain == NSPOSIXErrorDomain { return .connect(ns.localizedDescription) }
        return .connect(ns.localizedDescription)
    }

    func send(_ data: Data) async throws {
        try await withTaskCancellationHandler {
            do { try await task.write(data, timeout: timeout) } catch { throw Self.map(error) }
        } onCancel: { [task] in
            task.cancel()
        }
    }

    func receive() async throws -> Data {
        // Отмена задачи (таймаут чтения, отмена поиска) рвёт соединение — иначе
        // чтение висело бы до своего часового предела.
        try await withTaskCancellationHandler {
            do {
                let (data, _) = try await task.readData(ofMinLength: 1, maxLength: 256 * 1024, timeout: 3600)
                return data ?? Data()
            } catch {
                throw Self.map(error)
            }
        } onCancel: { [task] in
            task.cancel()
        }
    }

    func startTLS() async throws {
        task.startSecureConnection()
    }

    func close() {
        task.cancel()
        session.invalidateAndCancel()
    }
}

// MARK: - Служебное

/// Ровно один вызов из нескольких источников (состояние, таймаут, отмена).
final class Once: @unchecked Sendable {
    private let lock = NSLock()
    private var done = false

    func run(_ f: () -> Void) {
        lock.lock()
        let first = !done
        done = true
        lock.unlock()
        if first { f() }
    }
}

extension Duration {
    var seconds: Double {
        let c = components
        return Double(c.seconds) + Double(c.attoseconds) / 1e18
    }
}
