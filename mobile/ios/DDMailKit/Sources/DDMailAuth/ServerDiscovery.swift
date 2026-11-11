import DDMailAPI
import Foundation
import Synchronization

/// Шаг поиска для полосы прогресса: сделано из верхней границы числа проверок.
public struct DiscoveryProgress: Equatable, Sendable {
    public let done: Int
    public let total: Int
    public let stage: String
}

/// Поиск сервера ddmail по адресу почты (контракт §3; порт `ServerDiscovery`
/// Android, логика — ddmail-часть десктопного `discover`):
///
/// 1. разом — MX домена и `/.well-known/ddmail` на `mail.<домен>` и `<домен>`;
/// 2. если там нет — то же на MX-хостах (не больше `maxMX`).
///
/// Проверки идут параллельно, но побеждает первая успешная **по порядку**, и
/// ждём только тех, кто стоит раньше неё. Прогресс — от верхней границы,
/// известной заранее; несостоявшиеся проверки засчитываются сразу, так что
/// полоса доходит до конца при любом исходе. Пароль здесь не участвует.
public struct ServerDiscovery: Sendable {
    public static let maxMX = 2
    /// MX-запрос + два прямых хоста + MX-хосты.
    public static let total = 1 + 2 + maxMX

    let probe: @Sendable (String) async -> Bool
    let mx: @Sendable (String) async -> [String]

    public init(probe: @escaping @Sendable (String) async -> Bool, mx: @escaping @Sendable (String) async -> [String]) {
        self.probe = probe
        self.mx = mx
    }

    /// Настоящие проверки: `.well-known` по https и MX системным резолвером.
    public static func live(transport: HTTPTransport) -> ServerDiscovery {
        ServerDiscovery(
            probe: { await DdmailAPI.isDdmail(host: $0, transport: transport) },
            mx: { await DnsMX.lookup($0) }
        )
    }

    /// `https://хост` найденного сервера или `nil`.
    public func find(email: String, onProgress: @escaping @Sendable (DiscoveryProgress) -> Void) async -> String? {
        let progress = Progress(onProgress)
        guard let domain = emailDomain(email) else {
            progress.finish()
            return nil
        }
        progress.stage("Ищем сервер для \(domain)…")

        let direct = ["mail.\(domain)", domain]
        let mxTask = Task { [mx] in
            let hosts = Array(await mx(domain).prefix(Self.maxMX))
            progress.tick()
            return hosts
        }
        if let hit = await firstInOrder(direct, progress) {
            mxTask.cancel()
            progress.finish()
            return "https://\(hit)"
        }

        let mxHosts = await mxTask.value.filter { !direct.contains($0) }
        progress.tick(Self.maxMX - mxHosts.count)
        if !mxHosts.isEmpty { progress.stage("Проверяем почтовые серверы домена…") }
        let hit = await firstInOrder(mxHosts, progress)
        progress.finish()
        return hit.map { "https://\($0)" }
    }

    /// Все проверки разом; ответ — первый успешный по порядку списка.
    private func firstInOrder(_ hosts: [String], _ progress: Progress) async -> String? {
        guard !hosts.isEmpty else { return nil }
        return await withTaskGroup(of: (Int, Bool).self) { group in
            for (i, h) in hosts.enumerated() {
                group.addTask { [probe] in
                    let ok = await probe(h)
                    return (i, ok)
                }
            }
            var results = [Bool?](repeating: nil, count: hosts.count)
            for await (i, ok) in group {
                results[i] = ok
                progress.tick()
                // Решено, когда все до первого успеха ответили «нет».
                for (j, r) in results.enumerated() {
                    guard let r else { break }
                    if r {
                        group.cancelAll()
                        progress.tick(results.filter { $0 == nil }.count)
                        return hosts[j]
                    }
                }
            }
            return nil
        }
    }

    /// Счётчик прогресса: тики из параллельных проверок, не больше `total`.
    private final class Progress: Sendable {
        private let state = Mutex((done: 0, stage: ""))
        private let emit: @Sendable (DiscoveryProgress) -> Void

        init(_ emit: @escaping @Sendable (DiscoveryProgress) -> Void) { self.emit = emit }

        func tick(_ n: Int = 1) {
            guard n > 0 else { return }
            send { $0.done = min($0.done + n, ServerDiscovery.total) }
        }

        func stage(_ s: String) { send { $0.stage = s } }

        func finish() { send { $0.done = ServerDiscovery.total } }

        /// Отдаём значение под замком: тики из разных проверок не обгонят друг
        /// друга по дороге к экрану, и полоса не дёрнется назад.
        private func send(_ change: (inout (done: Int, stage: String)) -> Void) {
            state.withLock { s in
                change(&s)
                emit(DiscoveryProgress(done: s.done, total: ServerDiscovery.total, stage: s.stage))
            }
        }
    }
}
